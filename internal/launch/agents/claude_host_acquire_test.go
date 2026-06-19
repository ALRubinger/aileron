package agents_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// Claude host-acquire contract (#1270, #1304):
//
//   - Hosted-callback happy path: browser opened with the consent URL,
//     code pasted on the host, code exchanged, profile fetched → Secret
//     round-trips the binding's own Render; expiresAt is in
//     milliseconds; subscriptionType is stamped from the profile so
//     Claude Code recognizes a Max/Pro session (#1304).
//   - Acquire failure (token 400 / prompter error / state mismatch /
//     profile fetch yields no tier): returns an error so the launcher
//     falls back to in-container login.
//   - Provider error body is not leaked into the returned error.

// redirectTransport rewrites every outbound request to the test
// server, so the acquirer's hardcoded claudeTokenURL const reaches the
// httptest endpoint without exposing a URL seam in production code.
type redirectTransport struct{ base *url.URL }

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.base.Scheme
	req.URL.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(req)
}

func testHTTPClient(serverURL string) *http.Client {
	u, _ := url.Parse(serverURL)
	return &http.Client{Transport: redirectTransport{base: u}}
}

// recordingOpener records the URL it was asked to open and reports a
// configurable error.
type recordingOpener struct {
	opened string
	err    error
}

func (o *recordingOpener) Open(rawURL string) error {
	o.opened = rawURL
	return o.err
}

// claudeOAuthServer is an httptest server that answers both legs of the
// host flow the redirectTransport rewrites onto it: the token exchange
// (path /v1/oauth/token) and the profile fetch (path
// /api/oauth/profile). orgType is echoed back as the profile's
// organization_type so a test can drive the tier-mapping. A blank
// orgType yields a profile with no recognizable tier.
func claudeOAuthServer(t *testing.T, orgType string) (*httptest.Server, *url.Values) {
	t.Helper()
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/profile":
			_, _ = io.WriteString(w, `{"organization":{"organization_type":"`+orgType+`","rate_limit_tier":"default_claude_max_20x"}}`)
		default: // token exchange
			_ = r.ParseForm()
			gotForm = r.PostForm
			_, _ = io.WriteString(w, `{"access_token":"acc-tok","refresh_token":"ref-tok","expires_in":28800,"token_type":"Bearer","scope":"user:inference"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gotForm
}

func scriptedPrompter(value string, err error) func(context.Context, io.Writer) (string, error) {
	return func(_ context.Context, _ io.Writer) (string, error) {
		return value, err
	}
}

func claudeHostAcquireBinding(t *testing.T) launch.FileBinding {
	t.Helper()
	fb := agents.Claude{}.AuthSpec().FileBindings[0]
	if fb.HostAcquire == nil {
		t.Fatal("Claude binding must declare HostAcquire for host-side seeding")
	}
	return fb
}

func TestClaudeHostAcquire_HostedCallbackHappyPath(t *testing.T) {
	srv, gotFormP := claudeOAuthServer(t, "claude_max")

	opener := &recordingOpener{}
	var out bytes.Buffer
	fb := claudeHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: testHTTPClient(srv.URL),
		Browser:    opener,
		Out:        &out,
		// Paste a bare code (no #state). The acquirer generates the
		// state internally and tolerates a bare-code paste since PKCE
		// already binds the exchange; a state-carrying paste is covered
		// by TestClaudeHostAcquire_StateMismatchReturnsError.
		CodePrompter: scriptedPrompter("the-code", nil),
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	if len(secret.Value) == 0 {
		t.Fatal("HostAcquire returned empty Secret on the happy path")
	}
	// The acquired Secret must round-trip the binding's own Render,
	// proving the launcher will accept it.
	if _, err := fb.Render(secret); err != nil {
		t.Fatalf("acquired Secret rejected by Render: %v", err)
	}
	// Browser was opened with the consent URL carrying the client id.
	if !strings.Contains(opener.opened, "client_id=9d1c250a") {
		t.Errorf("browser opened %q, want consent URL with client_id", opener.opened)
	}
	// The authorize URL is surfaced on deps.Out so the user has a
	// copyable record of what was launched, mirroring the Codex flow.
	if !strings.Contains(out.String(), "client_id=9d1c250a") {
		t.Errorf("Out missing the authorize URL; got %q", out.String())
	}
	// Token exchange used authorization_code + PKCE verifier, no secret.
	gotForm := *gotFormP
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", gotForm.Get("grant_type"))
	}
	if gotForm.Get("code") != "the-code" {
		t.Errorf("code = %q, want the-code (fragment split on #)", gotForm.Get("code"))
	}
	if gotForm.Get("code_verifier") == "" {
		t.Error("code_verifier missing from token exchange (PKCE required)")
	}
	if gotForm.Get("client_secret") != "" {
		t.Error("client_secret sent; Claude Code is a public client (PKCE only)")
	}
	// expiresAt in the seeded envelope must be milliseconds, and the
	// subscription tier must be stamped from the profile so Claude Code
	// recognizes a Max/Pro session (#1304). Assert presence + shape
	// rather than a hardcoded guess: subscriptionType is non-empty and
	// equals the mapped value for the profile's organization_type.
	var env struct {
		ClaudeAiOauth struct {
			AccessToken      string `json:"accessToken"`
			ExpiresAt        int64  `json:"expiresAt"`
			SubscriptionType string `json:"subscriptionType"`
			RateLimitTier    string `json:"rateLimitTier"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(secret.Value, &env); err != nil {
		t.Fatalf("unmarshal seeded envelope: %v", err)
	}
	if env.ClaudeAiOauth.AccessToken != "acc-tok" {
		t.Errorf("seeded accessToken = %q, want acc-tok", env.ClaudeAiOauth.AccessToken)
	}
	if env.ClaudeAiOauth.ExpiresAt <= 1_000_000_000_000 {
		t.Errorf("seeded expiresAt = %d, want milliseconds magnitude", env.ClaudeAiOauth.ExpiresAt)
	}
	if env.ClaudeAiOauth.SubscriptionType == "" {
		t.Error("seeded subscriptionType is empty; Claude Code will treat this as a raw API key (#1304)")
	}
	if env.ClaudeAiOauth.SubscriptionType != "max" {
		t.Errorf("seeded subscriptionType = %q, want max (mapped from organization_type=claude_max)",
			env.ClaudeAiOauth.SubscriptionType)
	}
	if env.ClaudeAiOauth.RateLimitTier == "" {
		t.Error("seeded rateLimitTier is empty; want the profile's rate_limit_tier carried through")
	}
}

// TestClaudeHostAcquire_ProfileTierMissingReturnsError covers a clean
// token exchange whose profile fetch yields no recognizable
// subscription tier (#1304): the acquirer must NOT seed a tier-less
// envelope; it returns an empty Secret + error so the launcher falls
// back to the in-container login.
func TestClaudeHostAcquire_ProfileTierMissingReturnsError(t *testing.T) {
	srv, _ := claudeOAuthServer(t, "claude_unknown")

	fb := claudeHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		HTTPClient:   testHTTPClient(srv.URL),
		Browser:      &recordingOpener{},
		CodePrompter: scriptedPrompter("the-code", nil),
	})
	if err == nil {
		t.Fatal("expected error when the profile fetch yields no subscription tier")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty when no subscription tier could be obtained")
	}
}

func TestClaudeHostAcquire_TokenEndpoint400ReturnsError(t *testing.T) {
	const secretLeak = "super-secret-token-hint-DO-NOT-LEAK"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"`+secretLeak+`"}`)
	}))
	defer srv.Close()

	fb := claudeHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		HTTPClient:   testHTTPClient(srv.URL),
		Browser:      &recordingOpener{},
		CodePrompter: scriptedPrompter("bare-code", nil),
	})
	if err == nil {
		t.Fatal("expected error on token endpoint 400 so the launcher falls back")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty on a failed exchange")
	}
	// The standard error code is allowed; the free-text description (and
	// any token hint it carries) must NOT leak into the user-facing
	// error string.
	if strings.Contains(err.Error(), secretLeak) {
		t.Errorf("provider error_description leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err = %v, want the standard error code invalid_grant surfaced", err)
	}
}

func TestClaudeHostAcquire_PrompterErrorReturnsError(t *testing.T) {
	fb := claudeHostAcquireBinding(t)
	_, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		Browser:      &recordingOpener{},
		CodePrompter: scriptedPrompter("", io.ErrUnexpectedEOF),
	})
	if err == nil {
		t.Fatal("expected error when the code prompter fails")
	}
}

func TestClaudeHostAcquire_StateMismatchReturnsError(t *testing.T) {
	// A token server that would succeed, to prove the abort happens
	// BEFORE the exchange when the pasted state does not match.
	exchangeHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchangeHit = true
		_, _ = io.WriteString(w, `{"access_token":"acc","expires_in":100}`)
	}))
	defer srv.Close()

	fb := claudeHostAcquireBinding(t)
	_, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		HTTPClient:   testHTTPClient(srv.URL),
		Browser:      &recordingOpener{},
		CodePrompter: scriptedPrompter("code#deadbeef-not-the-real-state", nil),
	})
	if err == nil {
		t.Fatal("expected error on state mismatch")
	}
	if exchangeHit {
		t.Error("token exchange ran despite state mismatch; the code may be cross-session")
	}
}

func TestClaudeHostAcquire_BrowserOpenFailureIsNonFatal(t *testing.T) {
	fb := claudeHostAcquireBinding(t)
	prompterCalled := false
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:     context.Background(),
		Browser: &recordingOpener{err: io.ErrClosedPipe},
		CodePrompter: func(context.Context, io.Writer) (string, error) {
			prompterCalled = true
			return "code", nil
		},
	})
	if err == nil {
		t.Fatal("expected error when the browser cannot be opened")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty when the consent URL cannot be opened")
	}
	// The flow aborts at the browser-open step; the prompter is never
	// reached because there is no consent page for the user to act on.
	if prompterCalled {
		t.Error("code prompter consulted after the browser failed to open")
	}
}

func TestClaudeHostAcquire_NilPrompterIsNonFatal(t *testing.T) {
	fb := claudeHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:     context.Background(),
		Browser: &recordingOpener{},
		// CodePrompter nil: the acquirer has no way to read the code.
	})
	if err == nil {
		t.Fatal("expected error when no CodePrompter is provided")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty when the paste flow cannot run")
	}
}
