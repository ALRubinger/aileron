package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// daemonCallerCallerDaemon is the API enum value the request body
// uses to opt into the daemon-hosted callback flow. Kept as a local
// shorthand so the tests read top-down.
const daemonCallerCallerDaemon = api.Daemon

// initDaemonCaller drives oauth2/init with caller=daemon and the
// supplied return_to and purpose. Returns the parsed response. Used
// across the callback tests so the setup is one line per case.
func initDaemonCaller(t *testing.T, srv *apiServer, fqn, identity, returnTo string, purpose api.OAuth2InitRequestPurpose) api.OAuth2InitResponse {
	t.Helper()
	caller := daemonCallerCallerDaemon
	purposeVal := purpose
	req := api.OAuth2InitRequest{
		ConnectorFqn: fqn,
		Identity:     identity,
		Caller:       &caller,
		ReturnTo:     &returnTo,
	}
	if purpose != "" {
		req.Purpose = &purposeVal
	}
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init",
			strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("init status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp api.OAuth2InitResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// stateFromAuthorizeURL extracts the `state` query parameter the
// init endpoint embedded in the authorize URL. The OAuth provider's
// redirect carries this value back, and the daemon's callback uses
// it as the session lookup key — the test mimics that round-trip.
func stateFromAuthorizeURL(t *testing.T, authorizeURL string) string {
	t.Helper()
	idx := strings.Index(authorizeURL, "state=")
	if idx < 0 {
		t.Fatalf("authorize URL has no state: %s", authorizeURL)
	}
	rest := authorizeURL[idx+len("state="):]
	if amp := strings.Index(rest, "&"); amp >= 0 {
		rest = rest[:amp]
	}
	return rest
}

// callbackGET invokes the daemon-hosted callback handler with the
// supplied query parameters and returns the response recorder. Tests
// assert on status code, Location header, body, or vault state
// afterward.
func callbackGET(srv *apiServer, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Oauth2BindingCallback(rec,
		httptest.NewRequest(http.MethodGet, "/v1/bindings/setup/oauth2/callback?"+query, nil),
		parseCallbackParams(query))
	return rec
}

// parseCallbackParams turns a URL-encoded query string into the
// generated callback params struct. The generated middleware does
// this from the request URL; we bypass the middleware because the
// tests call the handler directly.
func parseCallbackParams(query string) api.Oauth2BindingCallbackParams {
	values := parseQueryString(query)
	out := api.Oauth2BindingCallbackParams{}
	if v := values["code"]; v != "" {
		out.Code = v
	}
	if v := values["state"]; v != "" {
		out.State = v
	}
	if v, ok := values["error"]; ok && v != "" {
		out.Error = &v
	}
	if v, ok := values["error_description"]; ok && v != "" {
		out.ErrorDescription = &v
	}
	return out
}

// parseQueryString is a tiny URL-encoded form parser tailored for
// the callback test query shapes. Avoids dragging net/url just for
// the pair-split.
func parseQueryString(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, "&") {
		if pair == "" {
			continue
		}
		eq := strings.Index(pair, "=")
		if eq < 0 {
			out[pair] = ""
			continue
		}
		key := pair[:eq]
		val := pair[eq+1:]
		val = strings.ReplaceAll(val, "+", " ")
		// Minimal percent decoding for the values used in tests.
		val = decodePercents(val)
		out[key] = val
	}
	return out
}

func decodePercents(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi := fromHex(s[i+1])
			lo := fromHex(s[i+2])
			if hi >= 0 && lo >= 0 {
				out = append(out, byte(hi*16+lo))
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

func fromHex(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	}
	return -1
}

// --- init: caller=daemon validation ---

// TestInitOAuth2Binding_CallerDaemonRequiresReturnTo: omitting
// return_to under caller=daemon is a hard 400. The whole point of
// the daemon-hosted flow is to bounce the browser back to the
// webapp; without a target there's nowhere to go.
func TestInitOAuth2Binding_CallerDaemonRequiresReturnTo(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	caller := daemonCallerCallerDaemon
	body, _ := json.Marshal(api.OAuth2InitRequest{
		ConnectorFqn: fqn,
		Identity:     "work",
		Caller:       &caller,
		// ReturnTo deliberately omitted.
	})
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init",
			strings.NewReader(string(body))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "return_to is required") {
		t.Errorf("error message missing the return_to hint: %s", rec.Body.String())
	}
}

// TestInitOAuth2Binding_CallerDaemonRejectsNonLoopbackReturnTo:
// validateLoopbackReturnTo refuses anything off-loopback. Allowing
// non-loopback return_to would turn the callback endpoint into an
// open redirector the daemon's hostname can be shot through.
func TestInitOAuth2Binding_CallerDaemonRejectsNonLoopbackReturnTo(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	caller := daemonCallerCallerDaemon
	cases := []string{
		"https://attacker.example/return",
		"http://example.com/x",
		"//missing-scheme/x",
	}
	for _, returnTo := range cases {
		t.Run(returnTo, func(t *testing.T) {
			rt := returnTo
			body, _ := json.Marshal(api.OAuth2InitRequest{
				ConnectorFqn: fqn,
				Identity:     "work",
				Caller:       &caller,
				ReturnTo:     &rt,
			})
			rec := httptest.NewRecorder()
			srv.InitOAuth2Binding(rec,
				httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init",
					strings.NewReader(string(body))))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("return_to %q: status = %d, want 400; body=%s",
					returnTo, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestInitOAuth2Binding_CallerDaemonEmbedsDaemonRedirectURI: the
// authorize URL the daemon builds points at the daemon's own
// callback path, not an ephemeral port. This is the load-bearing
// difference from caller=cli — the OAuth provider redirects to the
// daemon, not to a CLI listener.
func TestInitOAuth2Binding_CallerDaemonEmbedsDaemonRedirectURI(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	resp := initDaemonCaller(t, srv, fqn, "work", "http://127.0.0.1:3000/oauth/done", "")
	wantPrefix := "http://127.0.0.1:9999/v1/bindings/setup/oauth2/callback"
	if resp.RedirectUri != wantPrefix {
		t.Errorf("RedirectUri = %q, want %q", resp.RedirectUri, wantPrefix)
	}
}

// TestInitOAuth2Binding_CallerDaemonRequiresWebappURL: when the
// daemon doesn't know its own URL (no webappURL, no AILERON_WEBAPP_URL),
// it can't construct a redirect_uri for the OAuth provider to hit. The
// init endpoint must surface this as 503 so the operator knows the
// daemon mode isn't reachable on this install.
func TestInitOAuth2Binding_CallerDaemonRequiresWebappURL(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "" // explicitly empty

	caller := daemonCallerCallerDaemon
	rt := "http://127.0.0.1:3000/x"
	body, _ := json.Marshal(api.OAuth2InitRequest{
		ConnectorFqn: fqn,
		Identity:     "work",
		Caller:       &caller,
		ReturnTo:     &rt,
	})
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init",
			strings.NewReader(string(body))))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestInitOAuth2Binding_CallerCLIDefaultUnchanged: the default
// (omitted caller, or caller=cli) must still produce an ephemeral
// loopback port. Regression guard so the existing CLI flow doesn't
// drift when caller=daemon is added.
func TestInitOAuth2Binding_CallerCLIDefaultUnchanged(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	body := `{"connector_fqn":"` + fqn + `","identity":"work"}`
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init",
			strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got api.OAuth2InitResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if !strings.HasPrefix(got.RedirectUri, "http://127.0.0.1:") ||
		!strings.HasSuffix(got.RedirectUri, "/callback") ||
		strings.Contains(got.RedirectUri, "/v1/bindings/setup/oauth2/callback") {
		t.Errorf("RedirectUri = %q; expected ephemeral-port CLI form", got.RedirectUri)
	}
}

// --- callback: happy and unhappy paths ---

// TestOauth2BindingCallback_HappyPathPersistsBindingAndRedirects:
// the canonical success path. Provider redirects with code+state;
// daemon validates state, exchanges code, persists binding, and
// 303s the browser back to return_to with a fragment hint.
func TestOauth2BindingCallback_HappyPathPersistsBindingAndRedirects(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"
	const returnTo = "http://127.0.0.1:3000/oauth/done"

	init := initDaemonCaller(t, srv, fqn, "work", returnTo, "")
	state := stateFromAuthorizeURL(t, init.AuthorizeUrl)

	rec := callbackGET(srv, "code=abc&state="+state)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, returnTo+"#") {
		t.Errorf("Location = %q, want prefix %q with fragment", loc, returnTo+"#")
	}
	if !strings.Contains(loc, "aileron_oauth=ok") {
		t.Errorf("Location fragment missing success hint: %q", loc)
	}
	if !strings.Contains(loc, "binding=") {
		t.Errorf("Location fragment missing binding name: %q", loc)
	}

	// Binding landed in the vault.
	bn, _ := binding.MakeName(cstore.CredentialKindOAuth2, "aileron-connector-google", "work")
	stored, err := srv.bindings.Get(t.Context(), bn)
	if err != nil {
		t.Fatalf("Get binding %s: %v", bn, err)
	}
	if stored.Status != binding.StatusActive {
		t.Errorf("status = %q, want active", stored.Status)
	}
	if !stored.RefreshTokenPresent {
		t.Error("expected RefreshTokenPresent=true (fake provider returns rt)")
	}
}

// TestOauth2BindingCallback_SessionConsumedOnFirstCallback: a duplicate
// callback (e.g. browser back-button replay) finds no session and
// surfaces a 404 HTML page. Prevents racing two finishes against the
// same code.
func TestOauth2BindingCallback_SessionConsumedOnFirstCallback(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	init := initDaemonCaller(t, srv, fqn, "work", "http://127.0.0.1:3000/done", "")
	state := stateFromAuthorizeURL(t, init.AuthorizeUrl)

	rec := callbackGET(srv, "code=abc&state="+state)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("first callback: status = %d", rec.Code)
	}
	// Replay.
	rec2 := callbackGET(srv, "code=abc&state="+state)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("replay: status = %d, want 404; body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "session") {
		t.Errorf("replay body should mention session: %s", rec2.Body.String())
	}
}

// TestOauth2BindingCallback_UnknownStateIs404: a state value the
// daemon never issued returns 404. Together with the consumed-on-first
// case, this guards against the CSRF property of `state` — only the
// session that opened the flow knows the value.
func TestOauth2BindingCallback_UnknownStateIs404(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"
	// No init call — no session exists.

	rec := callbackGET(srv, "code=abc&state=never-issued")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestOauth2BindingCallback_ProviderErrorIsBadRequestHTML: when the
// provider redirects with `?error=access_denied`, the daemon clears
// the session and renders an HTML failure page. No token exchange
// happens, no binding is written.
func TestOauth2BindingCallback_ProviderErrorIsBadRequestHTML(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	init := initDaemonCaller(t, srv, fqn, "work", "http://127.0.0.1:3000/done", "")
	state := stateFromAuthorizeURL(t, init.AuthorizeUrl)
	before := len(fp.tokenRequests)

	rec := callbackGET(srv, "error=access_denied&error_description=User+denied&state="+state)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if !strings.Contains(rec.Body.String(), "access_denied") {
		t.Errorf("body missing provider error code: %s", rec.Body.String())
	}
	if len(fp.tokenRequests) != before {
		t.Errorf("provider-error path attempted a token exchange (delta=%d)",
			len(fp.tokenRequests)-before)
	}
}

// TestOauth2BindingCallback_RejectsCliSession: a session opened in
// CLI mode must not complete via the daemon callback. Reaching this
// branch in production means the URL was crafted or the caller mode
// was tampered with; refuse rather than silently completing.
func TestOauth2BindingCallback_RejectsCliSession(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	// CLI-mode init (default).
	body := `{"connector_fqn":"` + fqn + `","identity":"work"}`
	rec := httptest.NewRecorder()
	srv.InitOAuth2Binding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup/oauth2/init", strings.NewReader(body)))
	var init api.OAuth2InitResponse
	_ = json.NewDecoder(rec.Body).Decode(&init)
	state := stateFromAuthorizeURL(t, init.AuthorizeUrl)

	cb := callbackGET(srv, "code=abc&state="+state)
	if cb.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (wrong caller mode); body=%s",
			cb.Code, cb.Body.String())
	}
	if !strings.Contains(cb.Body.String(), "caller=cli") {
		t.Errorf("error message should explain the caller mismatch: %s", cb.Body.String())
	}
}

// TestOauth2BindingCallback_ReauthorizeUpsertsExistingBinding: a
// callback opened with purpose=reauthorize must upsert the existing
// binding (preserving CreatedAt) rather than create a new one. This
// is the load-bearing case for #743 — the stale-binding reauthorize
// UX from #741 lands here.
func TestOauth2BindingCallback_ReauthorizeUpsertsExistingBinding(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	// Seed an existing binding so reauthorize has something to upsert.
	bn, _ := binding.MakeName(cstore.CredentialKindOAuth2, "aileron-connector-google", "work")
	seed := binding.Binding{
		Name:         bn,
		Kind:         cstore.CredentialKindOAuth2,
		Service:      "aileron-connector-google",
		Identity:     "work",
		ConnectorFQN: fqn,
		Status:       binding.StatusStale,
		StaleReason:  binding.StaleReasonScopeDrift,
		MissingScopes: []string{"drive"},
	}
	if err := srv.bindings.Put(t.Context(), seed, []byte(`{}`), binding.PutCreate); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	originalCreatedAt := mustGetBinding(t, srv, bn).CreatedAt

	init := initDaemonCaller(t, srv, fqn, "work",
		"http://127.0.0.1:3000/done", api.Reauthorize)
	state := stateFromAuthorizeURL(t, init.AuthorizeUrl)

	rec := callbackGET(srv, "code=newcode&state="+state)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}

	got := mustGetBinding(t, srv, bn)
	if got.Status != binding.StatusActive {
		t.Errorf("status = %q, want active (stale cleared on reauthorize)", got.Status)
	}
	if got.StaleReason != "" {
		t.Errorf("StaleReason = %q, want cleared", got.StaleReason)
	}
	if !got.CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("CreatedAt = %v, want preserved %v", got.CreatedAt, originalCreatedAt)
	}
}

// TestOauth2BindingCallback_TokenExchangeFailure422: the fake provider
// returns 500 on /token, the daemon must surface a 422 HTML page and
// leave the binding store untouched.
func TestOauth2BindingCallback_TokenExchangeFailure422(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	fp.tokenStatus = http.StatusInternalServerError
	fp.tokenResponse = `{"error":"server_error"}`
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"

	init := initDaemonCaller(t, srv, fqn, "work", "http://127.0.0.1:3000/done", "")
	state := stateFromAuthorizeURL(t, init.AuthorizeUrl)

	rec := callbackGET(srv, "code=abc&state="+state)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	// No binding should have been written.
	bn, _ := binding.MakeName(cstore.CredentialKindOAuth2, "aileron-connector-google", "work")
	if _, err := srv.bindings.Get(t.Context(), bn); err == nil {
		t.Error("expected ErrNotFound — binding must not be persisted on token-exchange failure")
	}
}

// mustGetBinding looks up a binding by name and fails the test on
// any error. Keeps the reauthorize test readable.
func mustGetBinding(t *testing.T, srv *apiServer, bn binding.Name) binding.Binding {
	t.Helper()
	b, err := srv.bindings.Get(t.Context(), bn)
	if err != nil {
		t.Fatalf("Get binding %s: %v", bn, err)
	}
	return b
}

// TestOauth2BindingCallback_VaultLockedRejected: callback can't write
// the binding when the vault is locked. Returns 423 HTML so the user
// in the browser sees what's wrong and can unlock.
func TestOauth2BindingCallback_VaultLockedRejected(t *testing.T) {
	const fqn = "github://acme/aileron-connector-google"
	fp := newFakeOAuthProvider(t)
	srv := oauth2TestServer(t, fp, fqn)
	srv.webappURL = "http://127.0.0.1:9999"
	srv.vaultLocked = true

	rec := callbackGET(srv, "code=abc&state=anything")
	if rec.Code != http.StatusLocked {
		t.Errorf("status = %d, want 423; body=%s", rec.Code, rec.Body.String())
	}
}

// TestValidateLoopbackReturnTo: unit-level coverage for the helper.
// The init handler's call site is exercised end-to-end above; this
// pins behaviour on the IP literals and edge cases.
func TestValidateLoopbackReturnTo(t *testing.T) {
	good := []string{
		"http://127.0.0.1/x",
		"http://127.0.0.1:3000/oauth/done",
		"http://localhost/foo",
		"http://[::1]:9999/x",
		"https://localhost/secure",
	}
	for _, s := range good {
		if err := validateLoopbackReturnTo(s); err != nil {
			t.Errorf("validate(%q) returned %v, want nil", s, err)
		}
	}
	bad := []string{
		"https://attacker.example/x",
		"ftp://127.0.0.1/x",
		"http://10.0.0.1/x",
		"://missing-scheme",
		"http://",
	}
	for _, s := range bad {
		if err := validateLoopbackReturnTo(s); err == nil {
			t.Errorf("validate(%q) returned nil, want error", s)
		}
	}
}

// TestAppendCallbackResult: the fragment-hint helper must preserve
// any existing fragment on return_to (rare but legitimate — webapps
// using hash-routing already have one). The fragment is the wire
// the webapp listens on to flip from "waiting" to "bound".
func TestAppendCallbackResult(t *testing.T) {
	cases := []struct {
		in, status, name, want string
	}{
		{"", "ok", "x", ""},
		{"http://127.0.0.1/x", "ok", "oauth2/g/work",
			"http://127.0.0.1/x#aileron_oauth=ok&binding=oauth2%2Fg%2Fwork"},
		{"http://127.0.0.1/x#already", "ok", "",
			"http://127.0.0.1/x#already&aileron_oauth=ok"},
	}
	for _, c := range cases {
		got := appendCallbackResult(c.in, c.status, c.name)
		if got != c.want {
			t.Errorf("appendCallbackResult(%q,%q,%q) = %q, want %q",
				c.in, c.status, c.name, got, c.want)
		}
	}
}
