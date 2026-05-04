package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/oauth"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// --- OAuth2 binding-setup endpoints (#388) ---
//
// Two-phase server-driven dance:
//
//   /v1/bindings/setup/oauth2/init    — server allocates session,
//     PKCE pair, callback listener port; returns authorize URL.
//   /v1/bindings/setup/oauth2/finish  — server exchanges code at
//     provider's token endpoint, persists tokens as a binding.
//
// Sessions live in process memory keyed by an opaque random id, with
// a 10-minute TTL. The PKCE verifier never crosses the wire — only
// the challenge (in the authorize URL) does.

// oauth2SessionTTL is the window during which a session is valid
// after init. The user typically completes consent in well under 10
// minutes; longer windows just mean stale sessions accumulate. The
// session is also cleared on first finish.
const oauth2SessionTTL = 10 * time.Minute

// oauth2Session is the in-process state pinned by an init call.
type oauth2Session struct {
	connectorFQN string
	identity     string
	service      string
	account      string
	verifier     string
	state        string
	redirectURI  string
	scopes       []string
	clientID     string
	clientSecret string
	tokenURL     string
	expiresAt    time.Time
}

// oauth2Sessions is the apiServer's in-memory session store. Each
// session_id maps to one oauth2Session; expired sessions are pruned
// lazily on access.
type oauth2Sessions struct {
	mu       sync.Mutex
	sessions map[string]*oauth2Session
	now      func() time.Time
}

// newOAuth2Sessions returns an empty session store using time.Now as
// the clock.
func newOAuth2Sessions() *oauth2Sessions {
	return &oauth2Sessions{
		sessions: map[string]*oauth2Session{},
		now:      time.Now,
	}
}

// put stores s under sessionID with a fresh expiry.
func (m *oauth2Sessions) put(sessionID string, s *oauth2Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.expiresAt = m.now().Add(oauth2SessionTTL)
	m.sessions[sessionID] = s
}

// take returns and removes the session for sessionID, or returns
// (nil, false) when missing or expired.
func (m *oauth2Sessions) take(sessionID string) (*oauth2Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, false
	}
	delete(m.sessions, sessionID)
	if m.now().After(s.expiresAt) {
		return nil, false
	}
	return s, true
}

// InitOAuth2Binding implements the OpenAPI operation. Generates PKCE,
// allocates a session, and returns the authorize URL.
func (s *apiServer) InitOAuth2Binding(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeError(w, http.StatusServiceUnavailable, "installer_disabled",
			"connector store not configured")
		return
	}
	if s.bindings == nil {
		writeError(w, http.StatusServiceUnavailable, "vault_unavailable",
			"binding store not configured")
		return
	}
	if s.oauth2Sessions == nil {
		s.oauth2Sessions = newOAuth2Sessions()
	}
	var req api.OAuth2InitRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.ConnectorFqn == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "connector_fqn is required")
		return
	}
	if req.Identity == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "identity is required")
		return
	}

	connFQN, manifest, err := s.lookupConnector(req.ConnectorFqn)
	if err != nil {
		writeError(w, http.StatusNotFound, "connector_not_installed", err.Error())
		return
	}
	cred := manifest.Capabilities.Credential
	if cred == nil || cred.Kind != cstore.CredentialKindOAuth2 || cred.OAuth2 == nil {
		writeError(w, http.StatusUnprocessableEntity, "not_oauth2",
			"connector "+connFQN+" does not declare an OAuth2 credential capability")
		return
	}

	pkce, err := oauth.NewPKCE()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pkce_error", err.Error())
		return
	}
	state, err := oauth.NewState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "state_error", err.Error())
		return
	}

	// Allocate a loopback listener port up front so the redirect_uri
	// in the authorize URL matches what the CLI will serve. The
	// listener is held open by the caller (CLI), not the server.
	port, err := pickFreePort()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "port_error", err.Error())
		return
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	authURL := buildAuthorizeURL(cred.OAuth2, redirectURI, state, pkce.Challenge)

	sessionID, err := newSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_error", err.Error())
		return
	}
	service := defaultServiceFromFQN(connFQN)
	if req.Service != nil && *req.Service != "" {
		service = *req.Service
	}
	account := ""
	if req.Account != nil {
		account = *req.Account
	}
	s.oauth2Sessions.put(sessionID, &oauth2Session{
		connectorFQN: connFQN,
		identity:     req.Identity,
		service:      service,
		account:      account,
		verifier:     pkce.Verifier,
		state:        state,
		redirectURI:  redirectURI,
		scopes:       cred.OAuth2.Scopes,
		clientID:     cred.OAuth2.ClientID,
		clientSecret: cred.OAuth2.ClientSecret,
		tokenURL:     cred.OAuth2.TokenURL,
	})

	writeJSON(w, http.StatusOK, api.OAuth2InitResponse{
		SessionId:    sessionID,
		AuthorizeUrl: authURL,
		RedirectUri:  redirectURI,
	})
}

// FinishOAuth2Binding implements the OpenAPI operation. Validates the
// session, exchanges the code, and persists the resulting tokens.
func (s *apiServer) FinishOAuth2Binding(w http.ResponseWriter, r *http.Request) {
	if s.bindings == nil {
		writeError(w, http.StatusServiceUnavailable, "vault_unavailable",
			"binding store not configured")
		return
	}
	if s.vaultLocked {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before completing oauth flow")
		return
	}
	if s.oauth2Sessions == nil {
		s.oauth2Sessions = newOAuth2Sessions()
	}
	var req api.OAuth2FinishRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	session, ok := s.oauth2Sessions.take(req.SessionId)
	if !ok {
		writeError(w, http.StatusNotFound, "session_not_found",
			"session_id is unknown or expired (sessions are cleared after first finish or 10 minutes)")
		return
	}
	if req.State != session.state {
		writeError(w, http.StatusBadRequest, "state_mismatch",
			"state in callback does not match the session's expected state (CSRF protection)")
		return
	}

	tok, herr := s.exchangeOAuth2Code(r.Context(), session, req.Code)
	if herr != nil {
		writeError(w, herr.status, herr.code, herr.message)
		return
	}

	// Build the binding name and persist the token envelope.
	bn, err := binding.MakeName(cstore.CredentialKindOAuth2, session.service, session.identity)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_identity", err.Error())
		return
	}
	envelope, _ := json.Marshal(tok)
	b := binding.Binding{
		Name:                bn,
		Kind:                cstore.CredentialKindOAuth2,
		Service:             session.service,
		Identity:            session.identity,
		ConnectorFQN:        session.connectorFQN,
		Account:             session.account,
		Scope:               strings.Join(session.scopes, " "),
		Status:              binding.StatusActive,
		RefreshTokenPresent: tok.RefreshToken != "",
	}
	if err := s.bindings.Put(r.Context(), b, envelope, binding.PutCreate); err != nil {
		if errors.Is(err, binding.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, "binding_exists",
				"binding "+string(bn)+" already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	stored, getErr := s.bindings.Get(r.Context(), bn)
	if getErr != nil {
		stored = b
	}
	s.recordBindingEvent(r.Context(), model.EventTypeBindingCreated, string(bn),
		session.connectorFQN, cstore.CredentialKindOAuth2)
	writeJSON(w, http.StatusCreated, toAPIBinding(stored))
}

// exchangeOAuth2Code POSTs the authorization code to the provider's
// token endpoint with PKCE and returns the parsed token envelope.
func (s *apiServer) exchangeOAuth2Code(ctx context.Context, session *oauth2Session, code string) (credential.OAuth2Token, *installActionError) {
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {session.redirectURI},
		"client_id":     {session.clientID},
		"code_verifier": {session.verifier},
	}
	// Some providers (Google Desktop client type) reject token
	// exchange without client_secret even when PKCE is used. Forward
	// it when the manifest declared one; PKCE is still what binds
	// the code to this session.
	if session.clientSecret != "" {
		body.Set("client_secret", session.clientSecret)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, session.tokenURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return credential.OAuth2Token{}, &installActionError{
			status: http.StatusInternalServerError, code: "internal_error", message: err.Error(),
		}
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	client := s.oauth2HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return credential.OAuth2Token{}, &installActionError{
			status: http.StatusBadGateway, code: "token_exchange_failed", message: err.Error(),
		}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return credential.OAuth2Token{}, &installActionError{
			status: http.StatusUnprocessableEntity, code: "token_exchange_failed",
			message: fmt.Sprintf("provider returned %d: %s", resp.StatusCode, string(respBody)),
		}
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return credential.OAuth2Token{}, &installActionError{
			status: http.StatusUnprocessableEntity, code: "token_exchange_failed",
			message: "parse token response: " + err.Error(),
		}
	}
	if parsed.AccessToken == "" {
		return credential.OAuth2Token{}, &installActionError{
			status: http.StatusUnprocessableEntity, code: "token_exchange_failed",
			message: "provider response missing access_token",
		}
	}
	expiresAt := time.Time{}
	if parsed.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	return credential.OAuth2Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    expiresAt,
		TokenType:    parsed.TokenType,
		ClientID:     session.clientID,
		ClientSecret: session.clientSecret,
		TokenURL:     session.tokenURL,
		Scopes:       session.scopes,
	}, nil
}

// buildAuthorizeURL composes the provider's authorize URL with all
// required query parameters per RFC 7636 + the provider's `offline`
// + `prompt=consent` extras (Google insists on the latter to issue a
// refresh token reliably; harmless on others).
func buildAuthorizeURL(o *cstore.ManifestOAuth2, redirectURI, state, challenge string) string {
	q := url.Values{
		"client_id":             {o.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(o.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
	}
	sep := "?"
	if strings.Contains(o.AuthorizeURL, "?") {
		sep = "&"
	}
	return o.AuthorizeURL + sep + q.Encode()
}

// pickFreePort asks the OS for a free TCP port on the loopback
// interface and returns the number. The caller (CLI) will rebind to
// the same port for the callback listener; on a busy host the port
// could be reclaimed in the meantime, in which case the CLI's bind
// attempt fails and the user retries — acceptable v1 trade-off.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// newSessionID returns 32 bytes of crypto-random base64url-encoded.
func newSessionID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
