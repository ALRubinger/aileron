package agents_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// Claude host-acquire contract (#1270):
//
//   - Hosted-callback happy path: browser opened with the consent URL,
//     code pasted on the host, code exchanged → Secret round-trips the
//     binding's own Render; expiresAt is in milliseconds.
//   - setup-token shortcut: when `claude` is on PATH and prints a bare
//     token, the shortcut path seeds an accessToken-only envelope
//     (no refresh/expiry).
//   - CLI absent: hosted-callback path taken (browser + prompter
//     consulted).
//   - Acquire failure (token 400 / prompter error / state mismatch):
//     returns an error so the launcher falls back to in-container login.
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

// emptyPATH points PATH at a directory with no `claude` binary so the
// setup-token shortcut's LookPath misses and the hosted-callback path
// is taken.
func emptyPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// stubClaudeOnPATH writes a fake `claude` executable that, for
// `setup-token`, prints stdoutLine and exits 0. Skips on Windows where
// the shell-script stub is not executable.
func stubClaudeOnPATH(t *testing.T, stdoutLine string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is not executable on windows")
	}
	dir := t.TempDir()
	// Escape single quotes (' -> '\'') so an stdoutLine carrying one
	// cannot break the single-quoted shell literal.
	escaped := strings.ReplaceAll(stdoutLine, "'", `'\''`)
	script := "#!/bin/sh\nif [ \"$1\" = \"setup-token\" ]; then printf '%s\\n' '" + escaped + "'; exit 0; fi\nexit 1\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)
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
	emptyPATH(t)

	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"acc-tok","refresh_token":"ref-tok","expires_in":28800,"token_type":"Bearer","scope":"user:inference"}`)
	}))
	defer srv.Close()

	opener := &recordingOpener{}
	fb := claudeHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		HTTPClient:   testHTTPClient(srv.URL),
		Browser: opener,
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
	// Token exchange used authorization_code + PKCE verifier, no secret.
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
	// expiresAt in the seeded envelope must be milliseconds.
	var env struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
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
}

func TestClaudeHostAcquire_SetupTokenShortcut(t *testing.T) {
	stubClaudeOnPATH(t, "sk-ant-bare-token")

	opener := &recordingOpener{}
	fb := claudeHostAcquireBinding(t)
	// HTTPClient/CodePrompter must NOT be consulted on the shortcut
	// path; pass a prompter that fails the test if called.
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:     context.Background(),
		Browser: opener,
		CodePrompter: func(context.Context, io.Writer) (string, error) {
			t.Fatal("CodePrompter consulted on the setup-token shortcut path")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("HostAcquire (shortcut): %v", err)
	}
	if opener.opened != "" {
		t.Errorf("browser opened on shortcut path: %q", opener.opened)
	}
	var env struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(secret.Value, &env); err != nil {
		t.Fatalf("unmarshal shortcut envelope: %v", err)
	}
	if env.ClaudeAiOauth.AccessToken != "sk-ant-bare-token" {
		t.Errorf("accessToken = %q, want sk-ant-bare-token", env.ClaudeAiOauth.AccessToken)
	}
	if env.ClaudeAiOauth.RefreshToken != "" {
		t.Errorf("refreshToken = %q, want empty on setup-token path", env.ClaudeAiOauth.RefreshToken)
	}
	if env.ClaudeAiOauth.ExpiresAt != 0 {
		t.Errorf("expiresAt = %d, want 0 on setup-token path", env.ClaudeAiOauth.ExpiresAt)
	}
}

func TestClaudeHostAcquire_CLIAbsentTakesHostedCallback(t *testing.T) {
	emptyPATH(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"acc","expires_in":100}`)
	}))
	defer srv.Close()

	opener := &recordingOpener{}
	prompterCalled := false
	fb := claudeHostAcquireBinding(t)
	_, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: testHTTPClient(srv.URL),
		Browser:    opener,
		CodePrompter: func(context.Context, io.Writer) (string, error) {
			prompterCalled = true
			return "bare-code-no-state", nil
		},
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	if opener.opened == "" {
		t.Error("browser not opened on the hosted-callback path")
	}
	if !prompterCalled {
		t.Error("code prompter not consulted on the hosted-callback path")
	}
}

func TestClaudeHostAcquire_TokenEndpoint400ReturnsError(t *testing.T) {
	emptyPATH(t)

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
	emptyPATH(t)

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
	emptyPATH(t)

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
	emptyPATH(t)

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
	emptyPATH(t)

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
