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

	// reauthorize is true when this session was opened to refresh the
	// scopes on an existing binding rather than create a new one.
	// Finish then upserts (preserving created_at, account, etc.) and
	// clears any stale-state labels on success. The init endpoint also
	// builds the authorize URL with prompt=consent so the provider
	// re-prompts for the upgraded scope set rather than silently
	// reissuing a token bound to the prior consent.
	reauthorize bool

	// caller is "cli" or "daemon"; the latter triggers the
	// daemon-hosted callback flow where the OAuth provider redirects
	// directly to /v1/bindings/setup/oauth2/callback and the daemon
	// completes the dance internally before redirecting the browser
	// to returnTo.
	caller string

	// returnTo is the URL the daemon redirects the user's browser to
	// after a successful daemon-hosted callback. Validated as
	// loopback-only at init time. Empty for caller=cli (CLI doesn't
	// need it; the CLI process owns the post-finish UX itself).
	returnTo string
}

// oauth2Sessions is the apiServer's in-memory session store. Each
// session_id maps to one oauth2Session; expired sessions are pruned
// lazily on access.
//
// Two index maps are maintained: the primary `sessions` keyed by
// session_id (used by oauth2/finish where the caller supplies the id
// explicitly), and `byState` keyed by the OAuth state value (used by
// the daemon-hosted callback where the redirect URL carries `state`
// but not `session_id`). Both maps reference the same underlying
// session object; both are cleared atomically on take().
type oauth2Sessions struct {
	mu       sync.Mutex
	sessions map[string]*oauth2Session
	byState  map[string]string // state → session_id
	now      func() time.Time
}

// newOAuth2Sessions returns an empty session store using time.Now as
// the clock.
func newOAuth2Sessions() *oauth2Sessions {
	return &oauth2Sessions{
		sessions: map[string]*oauth2Session{},
		byState:  map[string]string{},
		now:      time.Now,
	}
}

// put stores s under sessionID with a fresh expiry. Also indexes the
// session by its state value so the daemon-hosted callback can find
// it from the redirect URL alone.
func (m *oauth2Sessions) put(sessionID string, s *oauth2Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.expiresAt = m.now().Add(oauth2SessionTTL)
	m.sessions[sessionID] = s
	if s.state != "" {
		m.byState[s.state] = sessionID
	}
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
	if s.state != "" {
		delete(m.byState, s.state)
	}
	if m.now().After(s.expiresAt) {
		return nil, false
	}
	return s, true
}

// takeByState returns and removes the session indexed under the
// supplied state value, or returns (nil, false) when no session matches
// or the matched session has expired. Used by the daemon-hosted OAuth
// callback (#743) — the OAuth provider's redirect carries `state` but
// not `session_id`, so the daemon resolves the session via the reverse
// index. Identical semantics to take(): the session is consumed on the
// first lookup, so a duplicate callback (e.g. browser back-button)
// surfaces as 404 rather than racing two finishes.
func (m *oauth2Sessions) takeByState(state string) (*oauth2Session, bool) {
	if state == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sessionID, ok := m.byState[state]
	if !ok {
		return nil, false
	}
	s, ok := m.sessions[sessionID]
	if !ok {
		// byState carries a stale entry — keep the maps in sync.
		delete(m.byState, state)
		return nil, false
	}
	delete(m.sessions, sessionID)
	delete(m.byState, state)
	if m.now().After(s.expiresAt) {
		return nil, false
	}
	return s, true
}

// InitOAuth2Binding implements the OpenAPI operation. Generates PKCE,
// allocates a session, and returns the authorize URL.
//
// When the request body sets `purpose: "reauthorize"`, the session is
// pinned to an existing binding (identified by the connector + service
// + identity tuple). Finish upserts that binding rather than creating
// a new one, preserving CreatedAt / Account / other history, and the
// authorize URL is built with `prompt=consent` so the provider
// re-prompts for the upgraded scope set instead of silently reissuing
// from the user's prior consent. First-time setup omits
// `prompt=consent` for a smoother first-flow UX.
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

	reauthorize := false
	if req.Purpose != nil && *req.Purpose == api.Reauthorize {
		reauthorize = true
	}

	caller := callerCLI
	if req.Caller != nil && *req.Caller == api.Daemon {
		caller = callerDaemon
	}

	// Validate return_to up front (before any side effects). Required
	// when caller=daemon since the callback's whole job is to redirect
	// the browser somewhere on success; ignored for caller=cli.
	returnTo := ""
	if caller == callerDaemon {
		if req.ReturnTo == nil || *req.ReturnTo == "" {
			writeError(w, http.StatusBadRequest, "invalid_request",
				"return_to is required when caller=daemon")
			return
		}
		if err := validateLoopbackReturnTo(*req.ReturnTo); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_return_to", err.Error())
			return
		}
		returnTo = *req.ReturnTo
	}

	connFQN, manifest, err := s.lookupConnector(req.ConnectorFqn)
	if err != nil {
		writeError(w, http.StatusNotFound, "connector_not_installed", err.Error())
		return
	}
	cred := manifest.Capabilities.Credential
	if cred == nil || cred.Kind != cstore.CredentialKindOAuth2 || cred.OAuth2 == nil {
		// Carry the connector's declared kind so the CLI's setup flow can
		// route a non-oauth2 connector to the matching secret-entry path
		// (api_key vs aws_sigv4) without a second round trip.
		declaredKind := ""
		if cred != nil {
			declaredKind = cred.Kind
		}
		writeErrorDetails(w, http.StatusUnprocessableEntity, "not_oauth2",
			"connector "+connFQN+" does not declare an OAuth2 credential capability",
			[]map[string]interface{}{{"declared_kind": declaredKind}})
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

	// Compute the redirect_uri the OAuth provider will redirect to.
	// Both branches stay on loopback per ADR-0002 §"v1 OAuth
	// requirements"; they differ only in who binds the port.
	//
	//   caller=cli    — ephemeral port the CLI will bind on its own
	//                   process; oauth2/finish runs after the CLI
	//                   captures code+state from the loopback request.
	//   caller=daemon — daemon's own listen address (webappURL), with
	//                   a stable path /v1/bindings/setup/oauth2/callback.
	//                   The daemon completes the flow internally and
	//                   redirects the browser to session.returnTo.
	var redirectURI string
	switch caller {
	case callerDaemon:
		du, derr := s.daemonCallbackRedirectURI()
		if derr != nil {
			writeError(w, http.StatusServiceUnavailable, "daemon_callback_unavailable", derr.Error())
			return
		}
		redirectURI = du
	default:
		port, err := pickFreePort()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "port_error", err.Error())
			return
		}
		redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	}

	authURL := buildAuthorizeURL(cred.OAuth2, redirectURI, state, pkce.Challenge, reauthorize)

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

	// Reauthorize requires the named binding to already exist so finish
	// has something to upsert. Return 404 here instead of letting finish
	// fail later — the caller hasn't run the user through the browser
	// yet, so the cost of catching it is one round-trip rather than a
	// wasted consent screen.
	if reauthorize {
		bn, mkErr := binding.MakeName(cstore.CredentialKindOAuth2, service, req.Identity)
		if mkErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_identity", mkErr.Error())
			return
		}
		existing, getErr := s.bindings.Get(r.Context(), bn)
		if errors.Is(getErr, binding.ErrNotFound) {
			writeError(w, http.StatusNotFound, "binding_not_found",
				"reauthorize requested for "+string(bn)+" but no such binding exists")
			return
		}
		if getErr != nil {
			writeError(w, http.StatusInternalServerError, "store_error", getErr.Error())
			return
		}
		// Inherit the existing binding's account label so finish doesn't
		// silently strip it when the caller omits the account override.
		if account == "" {
			account = existing.Account
		}
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
		reauthorize:  reauthorize,
		caller:       caller,
		returnTo:     returnTo,
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

	tok, grantedScopes, herr := s.exchangeOAuth2Code(r.Context(), session, req.Code)
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
		GrantedScopes:       grantedScopes,
		// On a successful reauthorize, stale-state labels must be
		// cleared. Defaults here are zero-value, so toMetadata omits
		// the labels — equivalent to clearing them on the next read.
		StaleReason:   "",
		MissingScopes: nil,
	}

	mode := binding.PutCreate
	evt := model.EventTypeBindingCreated
	if session.reauthorize {
		// Preserve the original CreatedAt so the binding's history
		// survives a scope refresh; VaultStore.Put only stamps it when
		// zero, so an explicit value here pins it.
		existing, getErr := s.bindings.Get(r.Context(), bn)
		if getErr == nil {
			b.CreatedAt = existing.CreatedAt
		}
		mode = binding.PutUpsert
		evt = model.EventTypeBindingRebound
	}
	if err := s.bindings.Put(r.Context(), b, envelope, mode); err != nil {
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
	s.recordBindingEvent(r.Context(), evt, string(bn),
		session.connectorFQN, cstore.CredentialKindOAuth2)
	status := http.StatusCreated
	if session.reauthorize {
		status = http.StatusOK
	}
	writeJSON(w, status, toAPIBinding(stored))
}

// exchangeOAuth2Code POSTs the authorization code to the provider's
// token endpoint with PKCE and returns the parsed token envelope plus
// the granted scope list.
//
// The granted scopes come from the provider's token response's
// `scope` field (space-separated, per RFC 6749 §3.3) when present.
// Some providers omit it on successful grants where the granted set
// matches the requested set; in that case we fall back to the
// requested scopes since the provider implicitly confirmed them.
// Returning a separate value rather than stuffing scopes onto the
// token envelope keeps the credential envelope shape stable (it's
// JSON-encoded into the vault and parsed by OAuth2VaultResolver).
func (s *apiServer) exchangeOAuth2Code(ctx context.Context, session *oauth2Session, code string) (credential.OAuth2Token, []string, *installActionError) {
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
		return credential.OAuth2Token{}, nil, &installActionError{
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
		return credential.OAuth2Token{}, nil, &installActionError{
			status: http.StatusBadGateway, code: "token_exchange_failed", message: err.Error(),
		}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return credential.OAuth2Token{}, nil, &installActionError{
			status: http.StatusUnprocessableEntity, code: "token_exchange_failed",
			message: fmt.Sprintf("provider returned %d: %s", resp.StatusCode, string(respBody)),
		}
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return credential.OAuth2Token{}, nil, &installActionError{
			status: http.StatusUnprocessableEntity, code: "token_exchange_failed",
			message: "parse token response: " + err.Error(),
		}
	}
	if parsed.AccessToken == "" {
		return credential.OAuth2Token{}, nil, &installActionError{
			status: http.StatusUnprocessableEntity, code: "token_exchange_failed",
			message: "provider response missing access_token",
		}
	}
	expiresAt := time.Time{}
	if parsed.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	granted := strings.Fields(parsed.Scope)
	if len(granted) == 0 {
		// Provider omitted `scope` from the response — RFC 6749 §5.1
		// permits this when the granted set matches the requested set.
		// Use what we asked for as the recorded grant.
		granted = append([]string(nil), session.scopes...)
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
	}, granted, nil
}

// buildAuthorizeURL composes the provider's authorize URL with all
// required query parameters per RFC 7636 + Google's `access_type=offline`
// extra (harmless on other providers).
//
// `forceConsent` adds `prompt=consent` for the reauthorize path. Google
// will otherwise silently reissue a token under the user's prior
// consent — so when the connector's manifest demands new scopes, the
// new scopes never get granted. `prompt=consent` forces the provider
// to re-prompt and re-issue against the current scope set. First-time
// setup omits it so the user's initial flow is one fewer click; the
// trade-off is that a user who happens to have granted these scopes
// to this OAuth client before may not receive a refresh token without
// `prompt=consent`, but that's vanishingly rare for first connect.
func buildAuthorizeURL(o *cstore.ManifestOAuth2, redirectURI, state, challenge string, forceConsent bool) string {
	q := url.Values{
		"client_id":             {o.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(o.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
	}
	if forceConsent {
		q.Set("prompt", "consent")
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

// Caller selects who serves the OAuth redirect; mirrors the OpenAPI
// enum (`cli` / `daemon`) but kept as plain strings inside the package
// because no behavior elsewhere needs the generated type.
const (
	callerCLI    = "cli"
	callerDaemon = "daemon"
)

// daemonCallbackPath is the URL path the daemon listens at for
// browser-driven OAuth callbacks (#743). Kept as a constant so the
// init handler (constructs the redirect_uri), the callback handler
// (validates incoming requests), and the OpenAPI route registration
// all reference one source of truth.
const daemonCallbackPath = "/v1/bindings/setup/oauth2/callback"

// daemonCallbackRedirectURI builds the redirect_uri for a
// caller=daemon flow. Falls back to the apiServer's webappURL (set by
// internal/server/main.go to the daemon's own listen address when no
// override is configured) because the daemon serves the webapp on the
// same listener it accepts API requests on — webappURL IS the daemon
// URL on loopback installs. Returns an error when neither is set;
// the init handler surfaces that as 503 so the operator knows the
// caller=daemon flow isn't reachable until webappURL is wired.
func (s *apiServer) daemonCallbackRedirectURI() (string, error) {
	base := strings.TrimRight(s.webappURL, "/")
	if base == "" {
		return "", fmt.Errorf("caller=daemon requires the daemon to know its own URL; set AILERON_WEBAPP_URL or run under launch")
	}
	return base + daemonCallbackPath, nil
}

// validateLoopbackReturnTo rejects a return_to URL that points
// anywhere outside the local loopback. ADR-0002's loopback boundary
// applies here: a caller=daemon callback's whole purpose is to
// shepherd a user from the daemon back to the webapp, and the webapp
// is loopback-only in v1. Permitting non-loopback redirects would
// turn this endpoint into an open redirector the daemon can be
// trick-shot through.
func validateLoopbackReturnTo(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse: %s", err.Error())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("host required")
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("host must be loopback (127.0.0.1, localhost, or [::1]); got %q", host)
	}
	return nil
}

// isLoopbackHost is a host-only loopback check: name-resolution
// alternatives ("localhost") plus the literal v4/v6 loopback IPs.
// IP-only (no DNS) because the daemon shouldn't honor a redirect to
// a name that *currently* resolves to a loopback IP but might not
// later — that would be a TOCTOU footgun.
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// Oauth2BindingCallback implements the daemon-hosted OAuth callback
// (#743). The OAuth provider redirects the user's browser here after
// consent; the handler looks up the session by `state`, completes the
// token exchange, persists the binding, and redirects the user to
// session.returnTo. Errors are rendered as HTML — a 401 JSON envelope
// in a browser tab is unhelpful UX — but they still carry the
// structured `code` so support escalation can pattern-match.
//
// The endpoint deliberately requires no auth and no CSRF token; CSRF
// protection is the OAuth `state` value (cryptographically random,
// pinned to the session at init, validated here). An attacker without
// the state value cannot construct a redirect that resolves to a
// session.
func (s *apiServer) Oauth2BindingCallback(w http.ResponseWriter, r *http.Request, params api.Oauth2BindingCallbackParams) {
	if s.bindings == nil {
		s.renderCallbackError(w, http.StatusServiceUnavailable,
			"vault_unavailable", "binding store not configured", "")
		return
	}
	if s.vaultLocked {
		s.renderCallbackError(w, http.StatusLocked,
			"vault_locked", "Unlock the vault before completing OAuth.", "")
		return
	}
	if s.oauth2Sessions == nil {
		s.oauth2Sessions = newOAuth2Sessions()
	}

	// Provider-reported error short-circuits the exchange. The session
	// is still consumed so a duplicate retry doesn't replay against a
	// session that's already invalidated client-side.
	if params.Error != nil && *params.Error != "" {
		desc := ""
		if params.ErrorDescription != nil {
			desc = *params.ErrorDescription
		}
		returnTo := ""
		if sess, ok := s.oauth2Sessions.takeByState(params.State); ok {
			returnTo = sess.returnTo
		}
		s.renderCallbackError(w, http.StatusBadRequest,
			"provider_error", "OAuth provider declined: "+*params.Error+
				ifEmpty(desc, "", " — "+desc), returnTo)
		return
	}

	session, ok := s.oauth2Sessions.takeByState(params.State)
	if !ok {
		s.renderCallbackError(w, http.StatusNotFound,
			"session_not_found",
			"This OAuth callback can't be matched to an in-flight session. Sessions expire after 10 minutes and clear on first use; if you opened this link twice, the second open lands here.",
			"")
		return
	}
	// Daemon callbacks are only valid for sessions opened in
	// caller=daemon mode; a CLI session reaching this endpoint means
	// either a misconfigured client or a forged callback. Refuse and
	// surface the mismatch.
	if session.caller != callerDaemon {
		s.renderCallbackError(w, http.StatusBadRequest,
			"wrong_caller_mode",
			"Session was opened for caller=cli; complete it via the CLI rather than the browser.",
			"")
		return
	}

	tok, grantedScopes, herr := s.exchangeOAuth2Code(r.Context(), session, params.Code)
	if herr != nil {
		s.renderCallbackError(w, herr.status, herr.code, herr.message, session.returnTo)
		return
	}

	bn, err := binding.MakeName(cstore.CredentialKindOAuth2, session.service, session.identity)
	if err != nil {
		s.renderCallbackError(w, http.StatusBadRequest, "invalid_identity", err.Error(), session.returnTo)
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
		GrantedScopes:       grantedScopes,
		StaleReason:         "",
		MissingScopes:       nil,
	}
	mode := binding.PutCreate
	evt := model.EventTypeBindingCreated
	if session.reauthorize {
		existing, getErr := s.bindings.Get(r.Context(), bn)
		if getErr == nil {
			b.CreatedAt = existing.CreatedAt
		}
		mode = binding.PutUpsert
		evt = model.EventTypeBindingRebound
	}
	if err := s.bindings.Put(r.Context(), b, envelope, mode); err != nil {
		if errors.Is(err, binding.ErrAlreadyExists) {
			s.renderCallbackError(w, http.StatusConflict, "binding_exists",
				"Binding "+string(bn)+" already exists.", session.returnTo)
			return
		}
		s.renderCallbackError(w, http.StatusInternalServerError, "store_error",
			err.Error(), session.returnTo)
		return
	}
	s.recordBindingEvent(r.Context(), evt, string(bn),
		session.connectorFQN, cstore.CredentialKindOAuth2)

	// Success → bounce back to the webapp's return URL with a hint that
	// reads cleanly server-side (logs) and client-side (URL fragment).
	dest := appendCallbackResult(session.returnTo, "ok", string(bn))
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// ifEmpty returns alt when v is empty, otherwise v. Tiny helper so
// the callback's error rendering can collapse "no description"
// branches inline.
func ifEmpty[T comparable](v T, zero T, alt string) string {
	if v == zero {
		return ""
	}
	return alt
}

// appendCallbackResult appends a fragment-form result hint to the
// return URL so the webapp can react without polling. Fragment is
// preferred over a query parameter because (a) it never reaches the
// server, keeping the daemon's HTTP logs free of binding names, and
// (b) it survives a SPA hash-router intercept naturally. Existing
// fragments on return_to are preserved.
func appendCallbackResult(returnTo, status, name string) string {
	if returnTo == "" {
		return returnTo
	}
	frag := "aileron_oauth=" + url.QueryEscape(status)
	if name != "" {
		frag += "&binding=" + url.QueryEscape(name)
	}
	if strings.Contains(returnTo, "#") {
		return returnTo + "&" + frag
	}
	return returnTo + "#" + frag
}

// renderCallbackError writes an HTML failure page to the user's
// browser tab. We don't have a templating engine wired up here — the
// page is plain HTML built inline. Keep the rendered surface
// deliberately small; a stack trace or full daemon error message in a
// browser is more noise than signal for the user. The structured
// `code` is included for support paths.
//
// returnTo, when non-empty, becomes a "Return to webapp" link so the
// user has a way back even on a hard failure. Empty when no session
// matched (we don't know where the user came from).
func (s *apiServer) renderCallbackError(w http.ResponseWriter, status int, code, message, returnTo string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	var ret string
	if returnTo != "" {
		ret = `<p><a href="` + htmlEscape(appendCallbackResult(returnTo, "error", "")) + `">Return to webapp</a></p>`
	}
	body := `<!doctype html>
<html><head><title>OAuth callback — Aileron</title>
<style>body{font:14px/1.5 system-ui,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1rem;color:#222}h1{font-size:1.25rem}.code{font-family:ui-monospace,monospace;background:#f6f6f6;padding:0.1rem 0.3rem;border-radius:4px}</style>
</head><body>
<h1>OAuth callback failed</h1>
<p>` + htmlEscape(message) + `</p>
<p>Error code: <span class="code">` + htmlEscape(code) + `</span></p>
` + ret + `
</body></html>`
	_, _ = io.WriteString(w, body)
}

// htmlEscape replaces the four characters that need escaping in HTML
// text content. We don't ship template/html here because the rendered
// surface is tiny and importing the package for two tags is overkill.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}
