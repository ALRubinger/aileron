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

// TestFinishOAuth2Binding_CapturesGrantedScopesFromResponse: the
// provider's token response carries a `scope` field listing the
// scopes actually granted (which may be a subset of what we asked
// for if the user de-selected some on the consent screen). Finish
// must record that list — not the requested list — so subsequent
// drift checks compare against truth.
func TestFinishOAuth2Binding_CapturesGrantedScopesFromResponse(t *testing.T) {
	const fqn = "github://acme/g"
	fp := newFakeOAuthProvider(t)
	// Provider grants only `read`, even though we asked for `read write`.
	fp.tokenResponse = `{"access_token":"at","refresh_token":"rt","expires_in":3600,"scope":"read"}`
	srv := oauth2TestServer(t, fp, fqn)

	initRec := httptest.NewRecorder()
	srv.InitOAuth2Binding(initRec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"`+fqn+`","identity":"work"}`)))
	var initResp api.OAuth2InitResponse
	_ = json.NewDecoder(initRec.Body).Decode(&initResp)
	parsedURL, _ := url.Parse(initResp.AuthorizeUrl)
	state := parsedURL.Query().Get("state")

	body := `{"session_id":"` + initResp.SessionId + `","code":"c","state":"` + state + `"}`
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got api.Binding
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.GrantedScopes == nil || len(*got.GrantedScopes) != 1 || (*got.GrantedScopes)[0] != "read" {
		t.Errorf("GrantedScopes = %v, want [read]", got.GrantedScopes)
	}
}

// TestFinishOAuth2Binding_FallsBackToRequestedScopes: when the
// provider omits `scope` from a successful token response, the
// requested set is recorded — RFC 6749 §5.1 lets providers omit
// `scope` when the granted set equals the requested set.
func TestFinishOAuth2Binding_FallsBackToRequestedScopes(t *testing.T) {
	const fqn = "github://acme/g"
	fp := newFakeOAuthProvider(t)
	// No `scope` field — implicit "granted = requested".
	fp.tokenResponse = `{"access_token":"at","refresh_token":"rt","expires_in":3600}`
	srv := oauth2TestServer(t, fp, fqn)

	initRec := httptest.NewRecorder()
	srv.InitOAuth2Binding(initRec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init",
		strings.NewReader(`{"connector_fqn":"`+fqn+`","identity":"work"}`)))
	var initResp api.OAuth2InitResponse
	_ = json.NewDecoder(initRec.Body).Decode(&initResp)
	parsedURL, _ := url.Parse(initResp.AuthorizeUrl)
	state := parsedURL.Query().Get("state")

	body := `{"session_id":"` + initResp.SessionId + `","code":"c","state":"` + state + `"}`
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got api.Binding
	_ = json.NewDecoder(rec.Body).Decode(&got)
	// Connector manifest's requested scopes are ["read","write"] (see
	// installOAuth2Connector). Both should round-trip.
	if got.GrantedScopes == nil || len(*got.GrantedScopes) != 2 {
		t.Errorf("GrantedScopes = %v, want fallback to [read write]", got.GrantedScopes)
	}
}

// TestInitOAuth2Binding_ReauthorizeRejectsWhenBindingMissing: init
// with `purpose: reauthorize` requires the binding to already exist
// — otherwise finish has nothing to upsert. Catching it at init
// avoids walking the user through an OAuth consent screen for a flow
// that's going to fail at the back end anyway.
func TestInitOAuth2Binding_ReauthorizeRejectsWhenBindingMissing(t *testing.T) {
	const fqn = "github://acme/g"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)

	body := `{"connector_fqn":"` + fqn + `","identity":"work","purpose":"reauthorize"}`
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init", strings.NewReader(body)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "binding_not_found") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

// TestFinishOAuth2Binding_ReauthorizeUpsertsClearsStale: a reauthorize
// of an existing binding marked `stale (scope_drift)` must (a)
// preserve CreatedAt, (b) refresh GrantedScopes from the token
// response, (c) clear StaleReason and MissingScopes, (d) return 200
// (not 201). This is the load-bearing path from issue #726.
func TestFinishOAuth2Binding_ReauthorizeUpsertsClearsStale(t *testing.T) {
	const fqn = "github://acme/g"
	fp := newFakeOAuthProvider(t)
	// Provider now grants the upgraded scope set.
	fp.tokenResponse = `{"access_token":"at2","refresh_token":"rt2","expires_in":3600,"scope":"read write"}`
	srv := oauth2TestServer(t, fp, fqn)

	// Seed a pre-existing stale binding for the same connector +
	// service + identity.
	ctx := context.Background()
	bn, _ := binding.MakeName("oauth2", "g", "work")
	seed := binding.Binding{
		Name: bn, Kind: "oauth2", Service: "g", Identity: "work",
		ConnectorFQN: fqn, Account: "alr@example.com",
		Status: binding.StatusStale, StaleReason: binding.StaleReasonScopeDrift,
		MissingScopes: []string{"write"},
		GrantedScopes: []string{"read"},
	}
	if err := srv.bindings.Put(ctx, seed, []byte(`{"access_token":"old"}`), binding.PutCreate); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	// Capture CreatedAt as the store stamped it.
	stored, err := srv.bindings.Get(ctx, bn)
	if err != nil {
		t.Fatalf("Get seed: %v", err)
	}
	originalCreatedAt := stored.CreatedAt

	// Drive init with purpose=reauthorize.
	body := `{"connector_fqn":"` + fqn + `","identity":"work","purpose":"reauthorize"}`
	initRec := httptest.NewRecorder()
	srv.InitOAuth2Binding(initRec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init", strings.NewReader(body)))
	if initRec.Code != http.StatusOK {
		t.Fatalf("init status = %d: %s", initRec.Code, initRec.Body.String())
	}
	var initResp api.OAuth2InitResponse
	_ = json.NewDecoder(initRec.Body).Decode(&initResp)

	parsedURL, _ := url.Parse(initResp.AuthorizeUrl)
	state := parsedURL.Query().Get("state")
	// Reauthorize MUST set prompt=consent (so Google re-prompts for
	// the upgraded scope set rather than silently reissuing).
	if parsedURL.Query().Get("prompt") != "consent" {
		t.Errorf("reauthorize authorize URL missing prompt=consent: %s", initResp.AuthorizeUrl)
	}

	finishBody := `{"session_id":"` + initResp.SessionId + `","code":"x","state":"` + state + `"}`
	rec := httptest.NewRecorder()
	srv.FinishOAuth2Binding(rec, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/finish", strings.NewReader(finishBody)))
	if rec.Code != http.StatusOK {
		t.Errorf("finish status = %d, want 200 (reauthorize upsert); body = %s",
			rec.Code, rec.Body.String())
	}

	// Re-read the binding: stale state cleared, GrantedScopes
	// refreshed, CreatedAt preserved, Account preserved.
	got, err := srv.bindings.Get(ctx, bn)
	if err != nil {
		t.Fatalf("Get post-finish: %v", err)
	}
	if got.Status != binding.StatusActive {
		t.Errorf("Status = %q, want active", got.Status)
	}
	if got.StaleReason != "" {
		t.Errorf("StaleReason = %q, want empty", got.StaleReason)
	}
	if len(got.MissingScopes) != 0 {
		t.Errorf("MissingScopes = %v, want empty", got.MissingScopes)
	}
	if len(got.GrantedScopes) != 2 || got.GrantedScopes[0] != "read" || got.GrantedScopes[1] != "write" {
		t.Errorf("GrantedScopes = %v, want [read write]", got.GrantedScopes)
	}
	if !got.CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("CreatedAt = %v, want preserved %v", got.CreatedAt, originalCreatedAt)
	}
	if got.Account != "alr@example.com" {
		t.Errorf("Account = %q, want preserved", got.Account)
	}

	// First-time setup MUST NOT include prompt=consent (user picked
	// "reauthorize only" semantics). Re-run init with default purpose
	// against an unbound identity and verify.
	body2 := `{"connector_fqn":"` + fqn + `","identity":"second"}`
	initRec2 := httptest.NewRecorder()
	srv.InitOAuth2Binding(initRec2, httptest.NewRequest(http.MethodPost,
		"/v1/bindings/setup/oauth2/init", strings.NewReader(body2)))
	if initRec2.Code != http.StatusOK {
		t.Fatalf("second init status = %d", initRec2.Code)
	}
	var initResp2 api.OAuth2InitResponse
	_ = json.NewDecoder(initRec2.Body).Decode(&initResp2)
	parsedURL2, _ := url.Parse(initResp2.AuthorizeUrl)
	if parsedURL2.Query().Get("prompt") != "" {
		t.Errorf("first-time setup must NOT force prompt=consent: %s", initResp2.AuthorizeUrl)
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
	got := buildAuthorizeURL(o, "http://127.0.0.1:1234/callback", "state-x", "challenge-y", false)
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
	} {
		if !strings.Contains(got, want) {
			t.Errorf("authorize URL missing %q:\n%s", want, got)
		}
	}
	// First-time setup must NOT force consent — that's reserved for
	// reauthorize so the provider re-prompts for upgraded scopes.
	if strings.Contains(got, "prompt=consent") {
		t.Errorf("first-time setup URL contains prompt=consent: %s", got)
	}
}

func TestBuildAuthorizeURL_ForceConsentSetsPromptParam(t *testing.T) {
	// Reauthorize flow needs prompt=consent so Google re-prompts for
	// the upgraded scope set instead of silently reissuing a token
	// bound to the user's prior consent.
	o := &cstore.ManifestOAuth2{
		AuthorizeURL: "https://provider.test/auth",
		ClientID:     "abc",
		Scopes:       []string{"read"},
	}
	got := buildAuthorizeURL(o, "http://127.0.0.1/cb", "s", "c", true)
	if !strings.Contains(got, "prompt=consent") {
		t.Errorf("reauthorize URL missing prompt=consent: %s", got)
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
	got := buildAuthorizeURL(o, "http://127.0.0.1/callback", "s", "c", false)
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
	_, _, herr := api.exchangeOAuth2Code(context.Background(), &oauth2Session{
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
	_, _, herr := api.exchangeOAuth2Code(context.Background(), &oauth2Session{
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
	_, _, herr := api.exchangeOAuth2Code(context.Background(), &oauth2Session{
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

// TestExchangeOAuth2Code_ForwardsClientSecretWhenSet asserts that a
// connector manifest declaring `client_secret` causes the runtime to
// forward it on token exchange. This is the Google "Desktop app" code
// path: the provider rejects token exchanges without a registered
// client_secret even with PKCE present.
func TestExchangeOAuth2Code_ForwardsClientSecretWhenSet(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		_, _ = io.WriteString(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	api := &apiServer{oauth2HTTPClient: srv.Client()}
	tok, _, herr := api.exchangeOAuth2Code(context.Background(), &oauth2Session{
		clientID:     "cid",
		clientSecret: "csecret",
		tokenURL:     srv.URL,
		redirectURI:  "http://127.0.0.1:0/callback",
		verifier:     "verifier",
	}, "code")
	if herr != nil {
		t.Fatalf("unexpected error: %+v", herr)
	}
	if got.Get("client_secret") != "csecret" {
		t.Errorf("client_secret in body = %q, want csecret", got.Get("client_secret"))
	}
	if got.Get("client_id") != "cid" || got.Get("code_verifier") != "verifier" {
		t.Errorf("PKCE/client_id fields not preserved: %v", got)
	}
	if tok.ClientSecret != "csecret" {
		t.Errorf("returned envelope ClientSecret = %q, want csecret (refresh path needs it)", tok.ClientSecret)
	}
}

// TestExchangeOAuth2Code_OmitsClientSecretWhenUnset asserts the default
// PKCE-only path: when the manifest does not declare client_secret, the
// runtime MUST NOT add an empty value to the form (some providers reject
// empty client_secret as a malformed request).
func TestExchangeOAuth2Code_OmitsClientSecretWhenUnset(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		_, _ = io.WriteString(w, `{"access_token":"at","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	api := &apiServer{oauth2HTTPClient: srv.Client()}
	if _, _, herr := api.exchangeOAuth2Code(context.Background(), &oauth2Session{
		clientID: "cid", tokenURL: srv.URL,
		redirectURI: "http://127.0.0.1:0/cb", verifier: "v",
	}, "code"); herr != nil {
		t.Fatalf("unexpected error: %+v", herr)
	}
	if _, present := got["client_secret"]; present {
		t.Errorf("client_secret unexpectedly present in form when manifest didn't declare one: %v", got)
	}
}
