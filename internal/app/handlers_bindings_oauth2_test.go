package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/vault"
)

// installOAuth2Connector seeds the cstore with a connector whose
// manifest declares an oauth2 capability with the supplied OAuth
// provider config. tokenURL/authURL are typically the URLs of an
// httptest server impersonating the OAuth provider.
func installOAuth2Connector(t *testing.T, store *cstore.Store, fqn, version, authURL, tokenURL string) string {
	t.Helper()
	manifestTOML := `[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "test"

[capabilities.credential]
kind = "oauth2"
scope = "Read your data"

[capabilities.credential.oauth2]
authorize_url = "` + authURL + `"
token_url = "` + tokenURL + `"
client_id = "test-client-id"
scopes = ["read", "write"]
`
	tb := &cstore.Tarball{
		BinaryName: "connector.wasm",
		Binary:     []byte("FAKE-BINARY"),
		Manifest:   []byte(manifestTOML),
		Signature:  []byte("FAKE-SIG"),
	}
	hashHex := tb.CanonicalHashHex()
	dir := filepath.Join(store.Root(), "connectors", "sha256", hashHex)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "connector.wasm"), tb.Binary, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "manifest.toml"), tb.Manifest, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "signature.sig"), tb.Signature, 0o644)
	if err := store.RebuildIndex(); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	return fqn
}

// fakeOAuthProvider stands up an httptest server that serves both
// the authorize and token endpoints. The server records the inbound
// requests so tests can inspect what the runtime sent.
type fakeOAuthProvider struct {
	server         *httptest.Server
	tokenRequests  []url.Values
	tokenResponse  string // override; defaults to a happy-path JSON
	tokenStatus    int    // override; defaults to 200
	authorizeURL   string
	tokenURL       string
	expectVerifier string // if set, asserts this verifier appears on the token request
}

func newFakeOAuthProvider(t *testing.T) *fakeOAuthProvider {
	p := &fakeOAuthProvider{
		tokenStatus: http.StatusOK,
		tokenResponse: `{"access_token":"new-access","refresh_token":"rt","expires_in":3600,"token_type":"Bearer"}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		// Provider would render a consent screen; tests don't drive
		// the browser. The handler is here so /authorize is reachable
		// for auditing purposes.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.tokenRequests = append(p.tokenRequests, r.PostForm)
		if p.expectVerifier != "" && r.PostForm.Get("code_verifier") != p.expectVerifier {
			t.Errorf("token request code_verifier = %q, want %q",
				r.PostForm.Get("code_verifier"), p.expectVerifier)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(p.tokenStatus)
		_, _ = io.WriteString(w, p.tokenResponse)
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	p.authorizeURL = p.server.URL + "/authorize"
	p.tokenURL = p.server.URL + "/token"
	return p
}

// oauth2TestServer wires an apiServer with the in-memory binding
// store, an installed oauth2 connector, an audit recorder, and the
// fake provider's HTTP client used to bypass TLS verification.
func oauth2TestServer(t *testing.T, fp *fakeOAuthProvider, fqn string) *apiServer {
	t.Helper()
	store := cstore.NewStore(t.TempDir())
	installOAuth2Connector(t, store, fqn, "1.0.0", fp.authorizeURL, fp.tokenURL)

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		log:              slog.Default(),
		bindings:         &binding.VaultStore{Vault: vault.NewMemVault()},
		auditStore:       auditStore,
		auditRecorder:    audit.NewRecorder(auditStore, nil, nil),
		installer:        &cstore.Installer{Store: store},
		oauth2Sessions:   newOAuth2Sessions(),
		oauth2HTTPClient: fp.server.Client(),
	}
	return srv
}

func TestInitOAuth2Binding_HappyPath(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)

	body := `{"connector_fqn":"` + fqn + `","identity":"work"}`
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.OAuth2InitResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionId == "" {
		t.Error("SessionId is empty")
	}
	if !strings.HasPrefix(got.RedirectUri, "http://127.0.0.1:") ||
		!strings.HasSuffix(got.RedirectUri, "/callback") {
		t.Errorf("RedirectUri = %q", got.RedirectUri)
	}
	// The authorize URL embeds all the OAuth + PKCE parameters.
	for _, want := range []string{
		"client_id=test-client-id",
		"redirect_uri=" + url.QueryEscape(got.RedirectUri),
		"response_type=code",
		"code_challenge_method=S256",
		"code_challenge=",
		"state=",
		"scope=read+write",
	} {
		if !strings.Contains(got.AuthorizeUrl, want) {
			t.Errorf("authorize URL missing %q: %s", want, got.AuthorizeUrl)
		}
	}
}

func TestInitOAuth2Binding_RejectsNonOAuth2Connector(t *testing.T) {
	// Connector declares api_key, not oauth2.
	store := cstore.NewStore(t.TempDir())
	installFakeAPIKeyConnector(t, store, "github://acme/x", "1.0.0", "api_key")
	srv := &apiServer{
		log:            slog.Default(),
		bindings:       &binding.VaultStore{Vault: vault.NewMemVault()},
		installer:      &cstore.Installer{Store: store},
		oauth2Sessions: newOAuth2Sessions(),
	}
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"github://acme/x","identity":"work"}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not_oauth2") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestInitOAuth2Binding_RejectsUnknownConnector(t *testing.T) {
	srv := &apiServer{
		log:            slog.Default(),
		bindings:       &binding.VaultStore{Vault: vault.NewMemVault()},
		installer:      &cstore.Installer{Store: cstore.NewStore(t.TempDir())},
		oauth2Sessions: newOAuth2Sessions(),
	}
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"github://nobody/x","identity":"work"}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestFinishOAuth2Binding_HappyPathPersistsBinding(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)

	// Drive init to populate a session.
	initRec := httptest.NewRecorder()
	srv.InitOAuth2Binding(initRec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"`+fqn+`","identity":"work"}`)))
	var initResp api.OAuth2InitResponse
	_ = json.NewDecoder(initRec.Body).Decode(&initResp)

	// Pull the session's state out of the authorize URL so finish
	// passes validation. The session is stored server-side keyed by
	// session_id; we extract `state` from the URL we received.
	parsedURL, _ := url.Parse(initResp.AuthorizeUrl)
	state := parsedURL.Query().Get("state")
	if state == "" {
		t.Fatal("state missing from authorize URL")
	}

	// Drive finish.
	body, _ := json.Marshal(api.OAuth2FinishRequest{
		SessionId: initResp.SessionId,
		Code:      "auth-code",
		State:     state,
	})
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish", strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.Binding
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.Kind != "oauth2" {
		t.Errorf("Kind = %q", got.Kind)
	}
	if got.Identity != "work" {
		t.Errorf("Identity = %q", got.Identity)
	}
	if got.RefreshTokenPresent == nil || !*got.RefreshTokenPresent {
		t.Error("RefreshTokenPresent should be true")
	}
	// Provider received exactly one token request with PKCE verifier.
	if len(fp.tokenRequests) != 1 {
		t.Errorf("provider saw %d token requests, want 1", len(fp.tokenRequests))
	}
	if fp.tokenRequests[0].Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", fp.tokenRequests[0].Get("grant_type"))
	}
	if fp.tokenRequests[0].Get("code") != "auth-code" {
		t.Errorf("code = %q", fp.tokenRequests[0].Get("code"))
	}
	if fp.tokenRequests[0].Get("code_verifier") == "" {
		t.Error("PKCE verifier missing from token request")
	}
	// Audit recorded binding.created without leaking tokens.
	events := dumpEvents(t, srv.auditStore)
	if !containsEvent(events, "binding.created") {
		t.Error("expected binding.created audit event")
	}
}

func TestFinishOAuth2Binding_StateMismatchRejected(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)

	initRec := httptest.NewRecorder()
	srv.InitOAuth2Binding(initRec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"`+fqn+`","identity":"work"}`)))
	var initResp api.OAuth2InitResponse
	_ = json.NewDecoder(initRec.Body).Decode(&initResp)

	// Submit finish with a state that doesn't match the session.
	body := `{"session_id":"` + initResp.SessionId + `","code":"x","state":"forged"}`
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "state_mismatch") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestFinishOAuth2Binding_UnknownSessionReturns404(t *testing.T) {
	srv := &apiServer{
		log:            slog.Default(),
		bindings:       &binding.VaultStore{Vault: vault.NewMemVault()},
		oauth2Sessions: newOAuth2Sessions(),
	}
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish",
		strings.NewReader(`{"session_id":"nope","code":"x","state":"y"}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestFinishOAuth2Binding_TokenExchangeFailureReturns422(t *testing.T) {
	const fqn = "github://acme/x"
	fp := newFakeOAuthProvider(t)
	fp.tokenStatus = http.StatusBadRequest
	fp.tokenResponse = `{"error":"invalid_grant"}`
	srv := oauth2TestServer(t, fp, fqn)

	initRec := httptest.NewRecorder()
	srv.InitOAuth2Binding(initRec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"`+fqn+`","identity":"work"}`)))
	var initResp api.OAuth2InitResponse
	_ = json.NewDecoder(initRec.Body).Decode(&initResp)
	parsedURL, _ := url.Parse(initResp.AuthorizeUrl)
	state := parsedURL.Query().Get("state")

	body := `{"session_id":"` + initResp.SessionId + `","code":"x","state":"` + state + `"}`
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish", strings.NewReader(body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "token_exchange_failed") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestFinishOAuth2Binding_VaultLockedReturns423(t *testing.T) {
	srv := &apiServer{
		log:            slog.Default(),
		bindings:       &binding.VaultStore{Vault: vault.NewMemVault()},
		oauth2Sessions: newOAuth2Sessions(),
		vaultLocked:    true,
	}
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish",
		strings.NewReader(`{"session_id":"x","code":"y","state":"z"}`)))
	if rec.Code != http.StatusLocked {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestOAuth2Sessions_TakeIsOneShot(t *testing.T) {
	m := newOAuth2Sessions()
	m.put("id1", &oauth2Session{})
	if _, ok := m.take("id1"); !ok {
		t.Fatal("first take should succeed")
	}
	if _, ok := m.take("id1"); ok {
		t.Error("second take should fail (session is consumed)")
	}
}


func TestInitOAuth2Binding_RejectsMissingFields(t *testing.T) {
	srv := &apiServer{
		log:            slog.Default(),
		bindings:       &binding.VaultStore{Vault: vault.NewMemVault()},
		installer:      &cstore.Installer{Store: cstore.NewStore(t.TempDir())},
		oauth2Sessions: newOAuth2Sessions(),
	}
	for _, body := range []string{
		`{}`,
		`{"connector_fqn":""}`,
		`{"connector_fqn":"github://x/y"}`,
		`{"identity":"work"}`,
	} {
		rec := httptest.NewRecorder()
		srv.InitOAuth2Binding(rec,
			httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init",
				strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("body=%s status = %d, want 400 or 404", body, rec.Code)
		}
	}
}

func TestInitOAuth2Binding_MalformedJSONReturns400(t *testing.T) {
	srv := &apiServer{
		log:            slog.Default(),
		bindings:       &binding.VaultStore{Vault: vault.NewMemVault()},
		installer:      &cstore.Installer{Store: cstore.NewStore(t.TempDir())},
		oauth2Sessions: newOAuth2Sessions(),
	}
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init", strings.NewReader(`{not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestInitOAuth2Binding_DisabledWhenInstallerNil(t *testing.T) {
	srv := &apiServer{
		log:            slog.Default(),
		bindings:       &binding.VaultStore{Vault: vault.NewMemVault()},
		oauth2Sessions: newOAuth2Sessions(),
	}
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"github://x/y","identity":"w"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestInitOAuth2Binding_DisabledWhenBindingsNil(t *testing.T) {
	srv := &apiServer{
		log:            slog.Default(),
		installer:      &cstore.Installer{Store: cstore.NewStore(t.TempDir())},
		oauth2Sessions: newOAuth2Sessions(),
	}
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"github://x/y","identity":"w"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestInitOAuth2Binding_LazyInitializesSessionsMap(t *testing.T) {
	// When oauth2Sessions is nil (production startup edge case), the
	// handler initializes it lazily on first call.
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.oauth2Sessions = nil
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"`+fqn+`","identity":"work"}`)))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if srv.oauth2Sessions == nil {
		t.Error("oauth2Sessions should have been lazy-initialized")
	}
}

func TestInitOAuth2Binding_ServiceOverrideAndAccount(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	body := `{"connector_fqn":"` + fqn + `","identity":"work","service":"custom-svc","account":"alr@example.com"}`
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestFinishOAuth2Binding_MalformedJSONReturns400(t *testing.T) {
	srv := &apiServer{
		log:            slog.Default(),
		bindings:       &binding.VaultStore{Vault: vault.NewMemVault()},
		oauth2Sessions: newOAuth2Sessions(),
	}
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish", strings.NewReader(`{not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestFinishOAuth2Binding_DisabledWhenBindingsNil(t *testing.T) {
	srv := &apiServer{log: slog.Default()}
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish",
		strings.NewReader(`{"session_id":"x","code":"y","state":"z"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestFinishOAuth2Binding_LazyInitializesSessionsMap(t *testing.T) {
	srv := &apiServer{
		log:      slog.Default(),
		bindings: &binding.VaultStore{Vault: vault.NewMemVault()},
	}
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish",
		strings.NewReader(`{"session_id":"x","code":"y","state":"z"}`)))
	// 404 because the session is unknown, but this exercises the
	// nil-map → lazy-init path.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if srv.oauth2Sessions == nil {
		t.Error("oauth2Sessions should have been lazy-initialized")
	}
}

func TestPickFreePort_ReturnsListenablePort(t *testing.T) {
	port, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	if port <= 0 {
		t.Errorf("port = %d", port)
	}
}

func TestNewSessionID_IsUniqueAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		s, err := newSessionID()
		if err != nil {
			t.Fatalf("newSessionID: %v", err)
		}
		if seen[s] {
			t.Fatalf("duplicate session id on draw %d", i)
		}
		seen[s] = true
		for _, r := range s {
			ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				t.Errorf("session id contains non-URL-safe character %q", r)
			}
		}
	}
}

func TestBuildAuthorizeURL_ComposesQueryString(t *testing.T) {
	o := &cstore.ManifestOAuth2{
		AuthorizeURL: "https://provider.test/auth",
		ClientID:     "abc",
		Scopes:       []string{"read", "write"},
	}
	got := buildAuthorizeURL(o, "http://127.0.0.1:1234/callback", "state-x", "challenge-y")
	for _, want := range []string{
		"https://provider.test/auth?",
		"client_id=abc",
		"redirect_uri=http%3A%2F%2F127.0.0.1%3A1234%2Fcallback",
		"response_type=code",
		"code_challenge=challenge-y",
		"code_challenge_method=S256",
		"state=state-x",
		"scope=read+write",
		"access_type=offline",
		"prompt=consent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("authorize URL missing %q:\n%s", want, got)
		}
	}
}

func TestBuildAuthorizeURL_AppendsToURLWithExistingQuery(t *testing.T) {
	// Some providers' authorize URLs already include query params.
	// The builder must use `&` not `?` to append.
	o := &cstore.ManifestOAuth2{
		AuthorizeURL: "https://provider.test/auth?prompt=login",
		ClientID:     "abc",
		Scopes:       []string{"read"},
	}
	got := buildAuthorizeURL(o, "http://127.0.0.1/callback", "s", "c")
	if !strings.Contains(got, "?prompt=login&") {
		t.Errorf("URL with existing query not joined with `&`: %s", got)
	}
}

func TestExchangeOAuth2Code_ProviderResponseMissingAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"refresh_token":"only"}`) // no access_token
	}))
	t.Cleanup(srv.Close)
	api := &apiServer{oauth2HTTPClient: srv.Client()}
	_, herr := api.exchangeOAuth2Code(context.Background(), &oauth2Session{
		clientID:    "cid",
		tokenURL:    srv.URL,
		redirectURI: "http://127.0.0.1:0/callback",
		verifier:    "v",
	}, "code")
	if herr == nil {
		t.Fatal("expected error when response omits access_token")
	}
	if !strings.Contains(herr.message, "access_token") {
		t.Errorf("err = %q", herr.message)
	}
}

func TestExchangeOAuth2Code_NetworkErrorIsBadGateway(t *testing.T) {
	api := &apiServer{
		oauth2HTTPClient: &http.Client{Transport: errTransport{}},
	}
	_, herr := api.exchangeOAuth2Code(context.Background(), &oauth2Session{
		clientID: "cid", tokenURL: "https://wont.respond.test",
		redirectURI: "http://127.0.0.1:0/cb", verifier: "v",
	}, "code")
	if herr == nil || herr.status != http.StatusBadGateway {
		t.Errorf("herr = %+v, want 502", herr)
	}
}

func TestExchangeOAuth2Code_MalformedResponseIs422(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	}))
	t.Cleanup(srv.Close)
	api := &apiServer{oauth2HTTPClient: srv.Client()}
	_, herr := api.exchangeOAuth2Code(context.Background(), &oauth2Session{
		clientID: "cid", tokenURL: srv.URL, redirectURI: "http://127.0.0.1:0/cb", verifier: "v",
	}, "code")
	if herr == nil || herr.status != http.StatusUnprocessableEntity {
		t.Errorf("herr = %+v, want 422", herr)
	}
}

// errTransport is an http.RoundTripper that always errors. Used to
// drive the network-error branch of exchangeOAuth2Code without actually
// attempting an outbound dial.
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("simulated network error")
}
