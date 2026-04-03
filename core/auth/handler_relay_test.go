package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newRelayTestEnv creates a testEnv configured with a stable callbackBaseURL and
// trustedOrigins, with the fake provider registered.
func newRelayTestEnv(identity *Identity, callbackBaseURL string, trustedOrigins []string) *testEnv {
	te := newTestEnv()
	te.handler.callbackBaseURL = strings.TrimRight(callbackBaseURL, "/")
	te.handler.trustedOrigins = trustedOrigins
	te.handler.registry.Register(&fakeProvider{name: "fake", identity: identity})
	return te
}

// doOAuthCallbackFromHost simulates a callback arriving at an explicit host,
// without a state cookie (as happens on the stable relay domain).
func (te *testEnv) doOAuthCallbackFromHost(provider, code, state, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/auth/"+provider+"/callback?code="+code+"&state="+state, nil)
	r.Host = host
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)
	return w
}

// --- composeState / parseState ---

func TestComposeParseState_RoundTrip(t *testing.T) {
	csrf := "abc123csrf"
	host := "feat-foo.up.railway.app"
	state := composeState(csrf, host)
	gotCSRF, gotHost := parseState(state)
	if gotCSRF != csrf {
		t.Errorf("parseState csrf = %q, want %q", gotCSRF, csrf)
	}
	if gotHost != host {
		t.Errorf("parseState host = %q, want %q", gotHost, host)
	}
}

func TestParseState_PlainToken(t *testing.T) {
	// A plain CSRF token with no embedded host returns an empty originating host.
	csrf, host := parseState("plaintokennohost")
	if csrf != "plaintokennohost" {
		t.Errorf("csrf = %q, want plaintokennohost", csrf)
	}
	if host != "" {
		t.Errorf("host = %q, want empty for plain token", host)
	}
}

func TestParseState_InvalidBase64AfterDot(t *testing.T) {
	// A state with a dot but invalid base64 after it should be treated as a
	// plain CSRF token (no originating host extracted).
	csrf, host := parseState("token.!!!invalid!!!")
	if csrf != "token.!!!invalid!!!" {
		t.Errorf("csrf = %q, want full raw value when base64 is invalid", csrf)
	}
	if host != "" {
		t.Errorf("host = %q, want empty for invalid base64", host)
	}
}

func TestParseState_EmptyString(t *testing.T) {
	csrf, host := parseState("")
	if csrf != "" {
		t.Errorf("csrf = %q, want empty", csrf)
	}
	if host != "" {
		t.Errorf("host = %q, want empty", host)
	}
}

func TestComposeState_ProducesCompoundFormat(t *testing.T) {
	state := composeState("csrf123", "branch.up.railway.app")
	// Must contain exactly one dot separating the CSRF token from the encoded host.
	if !strings.Contains(state, ".") {
		t.Errorf("composeState = %q, expected a dot separator", state)
	}
	parts := strings.SplitN(state, ".", 2)
	if parts[0] != "csrf123" {
		t.Errorf("CSRF part = %q, want csrf123", parts[0])
	}
}

// --- requestHost ---

func TestRequestHost_UsesXForwardedHost(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "internal-host:8080"
	r.Header.Set("X-Forwarded-Host", "public.example.com")
	if got := requestHost(r); got != "public.example.com" {
		t.Errorf("requestHost = %q, want public.example.com", got)
	}
}

func TestRequestHost_FallsBackToHost(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "myhost:9090"
	if got := requestHost(r); got != "myhost:9090" {
		t.Errorf("requestHost = %q, want myhost:9090", got)
	}
}

func TestRequestHost_EmptyXForwardedHostFallsBack(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "fallback.example.com"
	r.Header.Set("X-Forwarded-Host", "")
	if got := requestHost(r); got != "fallback.example.com" {
		t.Errorf("requestHost = %q, want fallback.example.com", got)
	}
}

// --- stableHost ---

func TestStableHost_ValidURL(t *testing.T) {
	if got := stableHost("https://auth.example.com"); got != "auth.example.com" {
		t.Errorf("stableHost = %q, want auth.example.com", got)
	}
}

func TestStableHost_URLWithPath(t *testing.T) {
	if got := stableHost("https://auth.example.com/some/path"); got != "auth.example.com" {
		t.Errorf("stableHost = %q, want auth.example.com", got)
	}
}

func TestStableHost_InvalidURL(t *testing.T) {
	if got := stableHost(":not-a-valid-url"); got != "" {
		t.Errorf("stableHost(%q) = %q, want empty for invalid URL", ":not-a-valid-url", got)
	}
}

func TestStableHost_EmptyString(t *testing.T) {
	if got := stableHost(""); got != "" {
		t.Errorf("stableHost(\"\") = %q, want empty", got)
	}
}

// --- isTrustedOrigin ---

func TestIsTrustedOrigin(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		patterns []string
		want     bool
	}{
		{
			name:     "exact match",
			host:     "staging.example.com",
			patterns: []string{"staging.example.com"},
			want:     true,
		},
		{
			name:     "wildcard subdomain match",
			host:     "feat-foo.up.railway.app",
			patterns: []string{"*.up.railway.app"},
			want:     true,
		},
		{
			name:     "wildcard no match - different suffix",
			host:     "feat-foo.up.heroku.com",
			patterns: []string{"*.up.railway.app"},
			want:     false,
		},
		{
			name:     "no patterns",
			host:     "example.com",
			patterns: []string{},
			want:     false,
		},
		{
			name:     "nil patterns",
			host:     "example.com",
			patterns: nil,
			want:     false,
		},
		{
			name:     "exact match among multiple patterns",
			host:     "specific.example.com",
			patterns: []string{"*.up.railway.app", "specific.example.com"},
			want:     true,
		},
		{
			name:     "wildcard match among multiple patterns",
			host:     "branch.up.railway.app",
			patterns: []string{"specific.example.com", "*.up.railway.app"},
			want:     true,
		},
		{
			name:     "bare domain does not match wildcard requiring subdomain",
			host:     "up.railway.app",
			patterns: []string{"*.up.railway.app"},
			want:     false,
		},
		{
			name:     "no match with non-empty patterns",
			host:     "evil.com",
			patterns: []string{"*.up.railway.app", "staging.example.com"},
			want:     false,
		},
		{
			name:     "empty host does not match exact pattern",
			host:     "",
			patterns: []string{"staging.example.com"},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTrustedOrigin(tt.host, tt.patterns); got != tt.want {
				t.Errorf("isTrustedOrigin(%q, %v) = %v, want %v", tt.host, tt.patterns, got, tt.want)
			}
		})
	}
}

// --- callbackURL with stable base URL ---

func TestCallbackURL_StableBaseOverridesDynamicDerivation(t *testing.T) {
	h := &Handler{callbackBaseURL: "https://auth.example.com"}
	r := httptest.NewRequest("GET", "/auth/google/login", nil)
	r.Host = "branch-deploy.up.railway.app"
	got := h.callbackURL(r, "google")
	want := "https://auth.example.com/auth/google/callback"
	if got != want {
		t.Errorf("callbackURL = %q, want %q", got, want)
	}
}

func TestCallbackURL_StableBaseIgnoresXForwardedHost(t *testing.T) {
	h := &Handler{callbackBaseURL: "https://auth.example.com"}
	r := httptest.NewRequest("GET", "/auth/github/login", nil)
	r.Host = "internal.example.com"
	r.Header.Set("X-Forwarded-Host", "should-be-ignored.railway.app")
	got := h.callbackURL(r, "github")
	want := "https://auth.example.com/auth/github/callback"
	if got != want {
		t.Errorf("callbackURL = %q, want %q", got, want)
	}
}

// --- handleLogin with callbackBaseURL (state composition) ---

func TestOAuthLogin_StableBase_StateComposesOriginatingHost(t *testing.T) {
	// When callbackBaseURL is set and the request comes from a branch deployment,
	// the oauth_state cookie must embed the originating host.
	te := newRelayTestEnv(
		&Identity{Subject: "sub", Email: "a@b.com", Provider: "fake"},
		"https://auth.stable.example.com",
		[]string{"*.up.railway.app"},
	)

	r := httptest.NewRequest("GET", "/auth/fake/login", nil)
	r.Host = "feat-my-branch.up.railway.app"
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTemporaryRedirect)
	}

	var stateCookieValue string
	for _, c := range w.Result().Cookies() {
		if c.Name == "oauth_state" {
			stateCookieValue = c.Value
		}
	}
	if stateCookieValue == "" {
		t.Fatal("expected oauth_state cookie to be set")
	}

	_, originatingHost := parseState(stateCookieValue)
	if originatingHost != "feat-my-branch.up.railway.app" {
		t.Errorf("originating host in state = %q, want feat-my-branch.up.railway.app", originatingHost)
	}
}

func TestOAuthLogin_StableBase_NoCompositionWhenRequestIsFromStableDomain(t *testing.T) {
	// When the request originates from the stable domain itself, the state must
	// NOT embed an originating host (no relay needed).
	te := newRelayTestEnv(
		&Identity{Subject: "sub", Email: "a@b.com", Provider: "fake"},
		"https://auth.stable.example.com",
		[]string{"*.up.railway.app"},
	)

	r := httptest.NewRequest("GET", "/auth/fake/login", nil)
	r.Host = "auth.stable.example.com"
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTemporaryRedirect)
	}

	var stateCookieValue string
	for _, c := range w.Result().Cookies() {
		if c.Name == "oauth_state" {
			stateCookieValue = c.Value
		}
	}
	if stateCookieValue == "" {
		t.Fatal("expected oauth_state cookie to be set")
	}

	_, originatingHost := parseState(stateCookieValue)
	if originatingHost != "" {
		t.Errorf("originating host = %q, want empty (stable domain should not relay to itself)", originatingHost)
	}
}

// --- handleCallback relay mode ---

func TestOAuthCallback_RelayToTrustedOrigin(t *testing.T) {
	originatingHost := "feat-branch.up.railway.app"
	te := newRelayTestEnv(
		&Identity{Subject: "sub", Email: "a@b.com", Provider: "fake"},
		"https://auth.stable.example.com",
		[]string{"*.up.railway.app"},
	)

	// Build a compound state that encodes the originating host.
	state := composeState("csrf-token-abc", originatingHost)

	// Callback arrives at the stable domain; no state cookie present since
	// cookie was set at the originating deployment.
	w := te.doOAuthCallbackFromHost("fake", "auth-code-xyz", state, "auth.stable.example.com")

	// Should 302-redirect to the originating host to complete the auth flow.
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (relay redirect); body: %s", w.Code, http.StatusFound, w.Body.String())
	}

	location := w.Header().Get("Location")
	if !strings.Contains(location, originatingHost) {
		t.Errorf("Location = %q, want to contain originating host %q", location, originatingHost)
	}
	if !strings.Contains(location, "/auth/fake/callback") {
		t.Errorf("Location = %q, want to contain /auth/fake/callback", location)
	}
	if !strings.Contains(location, "code=") {
		t.Errorf("Location = %q, want to contain code parameter", location)
	}
	if !strings.Contains(location, "state=") {
		t.Errorf("Location = %q, want to contain state parameter", location)
	}
}

func TestOAuthCallback_RelayRejectedUntrustedOrigin(t *testing.T) {
	te := newRelayTestEnv(
		&Identity{Subject: "sub", Email: "a@b.com", Provider: "fake"},
		"https://auth.stable.example.com",
		[]string{"*.up.railway.app"},
	)

	// Originating host is not in the trusted origins list.
	state := composeState("csrf-token", "evil.attacker.com")
	w := te.doOAuthCallbackFromHost("fake", "auth-code", state, "auth.stable.example.com")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "untrusted") {
		t.Errorf("body = %q, want to contain 'untrusted'", w.Body.String())
	}
}

func TestOAuthCallback_RelayProviderErrorNotForwarded(t *testing.T) {
	// When the OAuth provider returns an error during relay (e.g. user denied
	// consent), the stable domain must return an error rather than relaying
	// a bad callback to the originating deployment.
	te := newRelayTestEnv(
		&Identity{Subject: "sub", Email: "a@b.com", Provider: "fake"},
		"https://auth.stable.example.com",
		[]string{"*.up.railway.app"},
	)

	state := composeState("csrf-token", "feat-branch.up.railway.app")
	r := httptest.NewRequest("GET", "/auth/fake/callback?error=access_denied&state="+state, nil)
	r.Host = "auth.stable.example.com"
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "access_denied") {
		t.Errorf("body = %q, want to contain provider error 'access_denied'", w.Body.String())
	}
}

func TestOAuthCallback_RelayUnknownProvider(t *testing.T) {
	te := newRelayTestEnv(
		&Identity{Subject: "sub", Email: "a@b.com", Provider: "fake"},
		"https://auth.stable.example.com",
		[]string{"*.up.railway.app"},
	)

	state := composeState("csrf-token", "feat-branch.up.railway.app")
	w := te.doOAuthCallbackFromHost("nonexistent", "auth-code", state, "auth.stable.example.com")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestOAuthCallback_NoRelayWhenNoOriginatingHost(t *testing.T) {
	// A plain (non-compound) state should fall through to the normal callback
	// flow, not the relay path — even when callbackBaseURL is configured.
	te := newRelayTestEnv(
		&Identity{Subject: "sub", Email: "a@b.com", Provider: "fake"},
		"https://auth.stable.example.com",
		[]string{"*.up.railway.app"},
	)

	// Plain state — no embedded originating host.
	plainState := "plain-csrf-token"
	r := httptest.NewRequest("GET", "/auth/fake/callback?code=c&state="+plainState, nil)
	r.Host = "auth.stable.example.com"
	r.AddCookie(&http.Cookie{Name: "oauth_state", Value: plainState})
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	// Should proceed to normal auth flow (307 redirect to UI) rather than relay.
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (normal auth completion); body: %s", w.Code, http.StatusTemporaryRedirect, w.Body.String())
	}
}

func TestOAuthCallback_NoRelayWhenOriginatingHostMatchesCurrentHost(t *testing.T) {
	// If state embeds a host equal to the current host, treat as normal flow.
	currentHost := "auth.stable.example.com"
	te := newRelayTestEnv(
		&Identity{Subject: "sub", Email: "a@b.com", Provider: "fake"},
		"https://auth.stable.example.com",
		[]string{"*.up.railway.app"},
	)

	// State embeds the same host we're running on.
	state := composeState("csrf-token", currentHost)
	r := httptest.NewRequest("GET", "/auth/fake/callback?code=c&state="+state, nil)
	r.Host = currentHost
	r.AddCookie(&http.Cookie{Name: "oauth_state", Value: state})
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	// Should proceed to normal auth flow, not relay.
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (normal auth completion); body: %s", w.Code, http.StatusTemporaryRedirect, w.Body.String())
	}
}
