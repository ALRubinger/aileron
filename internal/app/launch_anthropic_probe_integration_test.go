//go:build integration_sandbox

// Composed regression guard for the launch -> gateway -> Anthropic-probe
// seam that #1696 (PR #1698) repaired.
//
// In api-key mode `aileron launch claude` points Claude Code's
// ANTHROPIC_BASE_URL at the Aileron daemon. Claude Code's login-state
// probe is `GET /api/oauth/profile` carrying the raw key in `x-api-key`.
// The #1696 bug: the daemon registered no `/api/*` route, so that probe
// fell through to the webapp catch-all and Claude Code reported "not
// logged in". The fix (#1698) registers a scoped `/api/` reverse proxy
// to the configured Anthropic upstream, BEFORE the webapp catch-all,
// forwarding `x-api-key` unchanged.
//
// The handler-level guard
// (TestAnthropicAPIFallthrough_ReachesUpstream in
// handlers_gateway_test.go) pins the routing, but it has two named
// weaknesses this composed test closes:
//
//  1. Its upstream is a bare echo with no auth semantics: it returns 200
//     regardless of whether `x-api-key` is present, so it cannot tell a
//     forwarded credential from a dropped one. This test stands up a
//     SPEC-COMPLIANT stub Anthropic upstream that returns 401 when
//     `x-api-key` is absent and 200 + `{"organization":{...}}` only when
//     the key is forwarded — so an "authenticated 200 with an org body"
//     assertion actually proves the credential rode through.
//
//  2. It never starts from the real rendered launch env: it hand-picks a
//     literal `sk-ant-test-xyz`, so the ANTHROPIC_BASE_URL -> daemon and
//     ANTHROPIC_API_KEY -> probe wiring the launcher actually produces is
//     untested. This test drives the REAL
//     agents.NewClaude(ClaudeAuthModeAPIKey).AuthSpec() Render +
//     RenderContent hooks — the exact functions the launcher invokes — to
//     render ANTHROPIC_API_KEY and the approved-suffix onboarding doc, and
//     then uses that rendered key against the daemon.
//
// Composed end to end, the test renders the launch env exactly as the
// launcher does, confirms the onboarding doc carries the suffix of the
// rendered key (the #1695 custom-API-key pre-approval), then points
// ANTHROPIC_BASE_URL at the daemon and issues the real
// `GET /api/oauth/profile` probe with the rendered key. It asserts an
// authenticated 200 + org object.
//
// FAIL-BEFORE / PASS-AFTER #1698: before the fix the `/api/` route does
// not exist, so the probe falls through to the webapp catch-all and the
// body is HTML rather than the org JSON the assertion requires.
//
// The test is gated behind the `integration_sandbox` build tag so it does
// not run during the normal `task test:go` suite. It is pure in-process
// (no Docker, no host subprocess): it composes the agents package's real
// AuthSpec render hooks with the app package's real mux registration, so
// there is no external prerequisite and, per repo policy, it never
// self-skips. Run with:
//
//	task test:integration:launch-anthropic-probe
package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/vault"
)

// renderedClaudeAPIKeyEnv is the env var Claude Code reads its raw key
// from. Spelled here rather than imported because the constant is
// unexported in the agents package; the AuthSpec render output is the
// contract we assert against.
const renderedClaudeAPIKeyEnv = "ANTHROPIC_API_KEY"

// rawTestAPIKey is the credential the test seeds into the (faked) vault.
// It is longer than Claude Code's 20-char approval suffix so the
// suffix-vs-whole-key branch in the onboarding render is exercised, and
// carries a leading/trailing space the Render hook must trim so the
// rendered env value and the onboarding suffix agree.
const rawTestAPIKey = "  sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA-zzzzzzzzzzzzzzzzzzzz  "

// renderClaudeAPIKeyLaunchEnv drives the REAL api-key-mode AuthSpec the
// launcher uses: it renders the EnvBinding (ANTHROPIC_API_KEY) and the
// StaticFile (the key-aware .claude.json onboarding doc) exactly as
// prepareAuthSpec does — EnvBinding.Render first, then
// StaticFile.RenderContent over the resulting env additions. It returns
// the rendered env additions and the rendered onboarding bytes so the
// caller can assert the daemon-facing key and the onboarding suffix come
// from one composed render, not two independently-chosen literals.
func renderClaudeAPIKeyLaunchEnv(t *testing.T, rawKey string) (env map[string]string, onboarding []byte) {
	t.Helper()

	spec := agents.NewClaude(agents.ClaudeAuthModeAPIKey).AuthSpec()
	if len(spec.EnvBindings) != 1 {
		t.Fatalf("api-key AuthSpec EnvBindings = %d, want 1 (the ANTHROPIC_API_KEY binding)", len(spec.EnvBindings))
	}
	if len(spec.StaticFiles) != 1 {
		t.Fatalf("api-key AuthSpec StaticFiles = %d, want 1 (the .claude.json onboarding doc)", len(spec.StaticFiles))
	}

	eb := spec.EnvBindings[0]
	if eb.Render == nil {
		t.Fatal("api-key EnvBinding has nil Render; the launcher cannot render ANTHROPIC_API_KEY")
	}
	// The vault stores the raw key bytes (no envelope), as the daemon
	// returns them; Render trims and maps to ANTHROPIC_API_KEY.
	env, err := eb.Render(vault.Secret{Value: []byte(rawKey)})
	if err != nil {
		t.Fatalf("EnvBinding.Render: %v", err)
	}
	if _, ok := env[renderedClaudeAPIKeyEnv]; !ok {
		t.Fatalf("rendered env = %v, want a %s entry", env, renderedClaudeAPIKeyEnv)
	}

	sf := spec.StaticFiles[0]
	if sf.RenderContent == nil {
		t.Fatal("api-key StaticFile has nil RenderContent; the onboarding doc cannot be made key-aware (#1695)")
	}
	// RenderContent runs AFTER the EnvBinding loop, over the rendered env
	// additions — mirror that ordering exactly.
	onboarding, err = sf.RenderContent(env)
	if err != nil {
		t.Fatalf("StaticFile.RenderContent: %v", err)
	}
	return env, onboarding
}

// assertOnboardingApprovesRenderedKey asserts the rendered .claude.json
// carries customApiKeyResponses.approved holding the last 20 characters
// of the rendered ANTHROPIC_API_KEY. This is the #1695 contract that
// suppresses Claude Code's interactive "Detected a custom API key"
// prompt; if the approval suffix drifts from the key the agent actually
// sees, the hands-off launch blocks.
func assertOnboardingApprovesRenderedKey(t *testing.T, renderedKey string, onboarding []byte) {
	t.Helper()

	var doc struct {
		CustomApiKeyResponses *struct {
			Approved []string `json:"approved"`
		} `json:"customApiKeyResponses"`
	}
	if err := json.Unmarshal(onboarding, &doc); err != nil {
		t.Fatalf("onboarding doc is not valid JSON: %v\n%s", err, onboarding)
	}
	if doc.CustomApiKeyResponses == nil || len(doc.CustomApiKeyResponses.Approved) != 1 {
		t.Fatalf("onboarding customApiKeyResponses.approved missing or not length 1: %s", onboarding)
	}
	got := doc.CustomApiKeyResponses.Approved[0]

	// Claude Code keys the approval on the LAST 20 runes of the raw key.
	r := []rune(renderedKey)
	want := renderedKey
	if len(r) > 20 {
		want = string(r[len(r)-20:])
	}
	if got != want {
		t.Fatalf("onboarding approval suffix = %q, want %q (last 20 chars of the rendered key)", got, want)
	}
}

// specCompliantAnthropicUpstream stands up a stub Anthropic upstream with
// real auth semantics for the `/api/oauth/profile` login-state probe:
//
//   - No `x-api-key` header -> 401 + an error body, as Anthropic returns
//     for an unauthenticated request. A bare-echo upstream (the
//     handler-level guard's) cannot distinguish this from a forwarded
//     key, so it is the weakness this stub closes.
//   - `x-api-key` present -> 200 + `{"organization":{...}}`, the profile
//     payload Claude Code reads to learn the signed-in org's tier.
//
// expectedKey, when non-empty, is additionally asserted: the forwarded
// `x-api-key` must equal it verbatim, proving the launcher-rendered key
// (not some rewritten value) reached upstream.
func specCompliantAnthropicUpstream(t *testing.T, expectedKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := r.Header.Get("x-api-key")
		if key == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"missing x-api-key"}}`))
			return
		}
		if expectedKey != "" && key != expectedKey {
			// Authenticated but with the WRONG key: a rewrite/leak would
			// surface here. 403 so the test fails on a mismatch with a
			// distinct status from the unauthenticated 401.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"unexpected x-api-key"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"organization":{"organization_type":"claude_max","rate_limit_tier":"tier_4"}}`))
	}))
}

// TestLaunchAnthropicProbe_RenderedKeyAuthenticatesThroughDaemon is the
// composed regression: render the api-key launch env with the real
// AuthSpec hooks, confirm the onboarding doc pre-approves the rendered
// key, then issue the real `GET /api/oauth/profile` probe with that key
// through the daemon's real mux order against a spec-compliant upstream,
// asserting an authenticated 200 + org object.
//
// Fails before #1698 (no `/api/` route -> webapp HTML, not the org JSON);
// passes after.
func TestLaunchAnthropicProbe_RenderedKeyAuthenticatesThroughDaemon(t *testing.T) {
	// (a) Render the launch env exactly as the launcher does.
	env, onboarding := renderClaudeAPIKeyLaunchEnv(t, rawTestAPIKey)
	renderedKey := env[renderedClaudeAPIKeyEnv]
	if renderedKey != strings.TrimSpace(rawTestAPIKey) {
		t.Fatalf("rendered key = %q, want the trimmed raw key %q", renderedKey, strings.TrimSpace(rawTestAPIKey))
	}
	assertOnboardingApprovesRenderedKey(t, renderedKey, onboarding)

	// (b) Spec-compliant stub Anthropic upstream with real auth semantics,
	// asserting the forwarded key is the launcher-rendered one verbatim.
	upstream := specCompliantAnthropicUpstream(t, renderedKey)
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	// The real daemon mux order: scoped `/api/` proxy registered BEFORE the
	// webapp catch-all (muxWithAPIFallthrough mirrors New()'s registration).
	s := &apiServer{log: slog.Default()}
	s.anthropicAPIProxy = newGatewayProxy(upstreamURL, "anthropic-api", s.log)
	daemon := httptest.NewServer(muxWithAPIFallthrough(s))
	t.Cleanup(daemon.Close)

	// (c) Point ANTHROPIC_BASE_URL at the daemon and issue Claude Code's
	// real login-state probe with the rendered key. The base URL is the
	// env var Claude Code reads (agents.Claude.LLMEndpointEnv()); we follow
	// the same join the agent does: ANTHROPIC_BASE_URL + /api/oauth/profile.
	base := daemon.URL
	if got := agents.NewClaude(agents.ClaudeAuthModeAPIKey).LLMEndpointEnv(); got != "ANTHROPIC_BASE_URL" {
		t.Fatalf("Claude LLMEndpointEnv = %q, want ANTHROPIC_BASE_URL", got)
	}
	probeURL := strings.TrimRight(base, "/") + "/api/oauth/profile"

	req, err := http.NewRequest(http.MethodGet, probeURL, nil)
	if err != nil {
		t.Fatalf("build probe request: %v", err)
	}
	req.Header.Set("x-api-key", renderedKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d, want 200 (authenticated profile)\nbody: %s\n"+
			"a 401 means the rendered key did not reach upstream; a non-JSON/HTML body means the probe fell "+
			"through to the webapp catch-all — the #1696 bug PR #1698 fixed", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("probe Content-Type = %q, want application/json (HTML means the webapp catch-all served it, the #1696 bug)", ct)
	}

	var profile struct {
		Organization struct {
			OrganizationType string `json:"organization_type"`
			RateLimitTier    string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		t.Fatalf("profile body is not the org JSON: %v\nbody: %s", err, body)
	}
	if profile.Organization.OrganizationType == "" {
		t.Fatalf("profile organization.organization_type empty; want the upstream org payload, got: %s", body)
	}
}

// TestLaunchAnthropicProbe_UnauthenticatedProbeIs401 pins the auth
// semantics of the spec-compliant upstream from the daemon's vantage: a
// probe with NO `x-api-key` reaches the upstream through the real mux
// order and gets the upstream's 401 propagated, NOT a webapp 200. This
// proves the composed harness's authenticated-200 assertion above is
// meaningful (the upstream really does gate on the header) and that the
// daemon forwards an upstream 4xx rather than masking it with the webapp
// catch-all.
func TestLaunchAnthropicProbe_UnauthenticatedProbeIs401(t *testing.T) {
	upstream := specCompliantAnthropicUpstream(t, "")
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	s := &apiServer{log: slog.Default()}
	s.anthropicAPIProxy = newGatewayProxy(upstreamURL, "anthropic-api", s.log)
	daemon := httptest.NewServer(muxWithAPIFallthrough(s))
	t.Cleanup(daemon.Close)

	resp, err := http.Get(strings.TrimRight(daemon.URL, "/") + "/api/oauth/profile")
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated probe status = %d, want 401 propagated from upstream\nbody: %s", resp.StatusCode, body)
	}
}
