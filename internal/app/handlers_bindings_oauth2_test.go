package app

import (
	"encoding/json"
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

