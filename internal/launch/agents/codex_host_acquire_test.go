package agents_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// Codex host-acquire contract (#1269):
//
//   - Full device-authorization poll-to-token happy path: device-code →
//     authorization_pending (403 N times) → success → PKCE exchange →
//     functional chatgpt envelope. The produced Secret round-trips the
//     binding's own Render AND carries non-empty id_token + account_id +
//     refresh_token (P0: functional, not merely parseable).
//   - The user_code + verification URL are surfaced via the injected
//     deps.Out (P2: the device flow's only headless-host path).
//   - A 429 increases the poll interval by the RFC 8628 §3.5 +5s floor.
//   - expired/denied poll responses → clean terminal error, no raw body.
//   - Browser-open failure is non-fatal-shaped (acquire error so the
//     launcher falls back), but the verification URL is surfaced first.
//   - Token-exchange error redaction: a 400 with error_description
//     surfaces status + error code only (mirrors claudeExchangeCode).
//   - codexAccountIDFromIDToken parses the chatgpt_account_id claim from a
//     hand-built JWT and errors on malformed input (verified via the
//     envelope's account_id, since the helper is unexported).

// codexRedirectTransport rewrites every outbound request's scheme+host to
// the test server while preserving the PATH, so the acquirer's three
// hardcoded auth.openai.com endpoints (usercode / token-poll / oauth-token)
// all reach a single path-dispatching httptest server without exposing a
// URL seam in production code. Mirrors the Claude test's redirectTransport.
type codexRedirectTransport struct{ base *url.URL }

func (rt codexRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.base.Scheme
	req.URL.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(req)
}

func codexTestHTTPClient(serverURL string) *http.Client {
	u, _ := url.Parse(serverURL)
	return &http.Client{Transport: codexRedirectTransport{base: u}}
}

// codexDeviceServerState configures the fake device-auth server's
// behavior across the three endpoints. Zero value yields a clean happy
// path with one pending poll.
type codexDeviceServerState struct {
	// pendingPolls is how many times the token-poll endpoint replies 403
	// (authorization_pending) before returning the success body.
	pendingPolls int

	// pollTerminalStatus, when non-zero, makes the FIRST poll return this
	// status instead of pending/success (models expired/denied).
	pollTerminalStatus int

	// slowDownOnFirstPoll makes the first poll return 429 (drives the
	// +5s interval increase) before falling through to the pending/success
	// sequence.
	slowDownOnFirstPoll bool

	// idToken is what the OAuth token exchange returns as id_token. When
	// empty, a JWT carrying chatgpt_account_id="acct-xyz" is used.
	idToken string

	// refreshToken is what the token exchange returns. When empty, "rt"
	// is used. Set to "-" to force an empty refresh_token in the response.
	refreshToken string

	// tokenStatus, when non-zero, makes the OAuth token exchange return
	// this status with tokenErrorBody instead of a success bundle.
	tokenStatus    int
	tokenErrorBody string

	// userCodeInterval is the interval (seconds) the usercode endpoint
	// advertises. 0 means the launcher's default applies.
	userCodeInterval int

	// usercodeRawBody, when non-empty, is written verbatim as the
	// usercode 200 response (used to exercise malformed/empty-field
	// parsing).
	usercodeRawBody string

	// pollRawBody, when non-empty, is written verbatim as the poll
	// success 200 response (used to exercise malformed/empty-field
	// parsing).
	pollRawBody string

	// noAccessToken makes the token exchange return a 200 body without an
	// access_token field.
	noAccessToken bool

	// missingIDToken makes the token exchange omit the id_token entirely.
	missingIDToken bool

	mu        sync.Mutex
	pollCount int
}

// codexUnsignedJWT builds a header.payload.sig JWT whose payload nests the
// given chatgpt_account_id under "https://api.openai.com/auth", matching
// the live OpenAI id_token shape. The signature segment is a non-empty
// placeholder (claim extraction does not verify it).
func codexUnsignedJWT(t *testing.T, accountID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := map[string]any{
		"sub": "user-123",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	payloadSeg := base64.RawURLEncoding.EncodeToString(pb)
	return header + "." + payloadSeg + ".c2ln"
}

// fakeDeviceAuthServer dispatches the three device-auth endpoints by path.
func fakeDeviceAuthServer(t *testing.T, st *codexDeviceServerState) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/deviceauth/usercode"):
			w.Header().Set("Content-Type", "application/json")
			if st.usercodeRawBody != "" {
				_, _ = io.WriteString(w, st.usercodeRawBody)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "dev-1",
				"user_code":      "WXYZ-1234",
				"interval":       st.userCodeInterval,
			})

		case strings.HasSuffix(r.URL.Path, "/deviceauth/token"):
			st.mu.Lock()
			n := st.pollCount
			st.pollCount++
			st.mu.Unlock()

			if st.pollTerminalStatus != 0 && n == 0 {
				w.WriteHeader(st.pollTerminalStatus)
				_, _ = io.WriteString(w, `{"error":"expired or denied"}`)
				return
			}
			if st.slowDownOnFirstPoll && n == 0 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			// Account for a consumed slow-down attempt when counting the
			// pending sequence.
			effective := n
			if st.slowDownOnFirstPoll {
				effective = n - 1
			}
			if effective < st.pendingPolls {
				w.WriteHeader(http.StatusForbidden) // authorization_pending
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if st.pollRawBody != "" {
				_, _ = io.WriteString(w, st.pollRawBody)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_code": "auth-code-1",
				"code_challenge":     "chal",
				"code_verifier":      "verifier-1",
			})

		case strings.HasSuffix(r.URL.Path, "/oauth/token"):
			if st.tokenStatus != 0 {
				w.WriteHeader(st.tokenStatus)
				_, _ = io.WriteString(w, st.tokenErrorBody)
				return
			}
			idToken := st.idToken
			if idToken == "" {
				idToken = codexUnsignedJWT(t, "acct-xyz")
			}
			refresh := st.refreshToken
			switch refresh {
			case "":
				refresh = "rt"
			case "-":
				refresh = ""
			}
			access := "acc-tok"
			if st.noAccessToken {
				access = ""
			}
			body := map[string]any{
				"access_token":  access,
				"refresh_token": refresh,
				"token_type":    "Bearer",
				"expires_in":    3600,
			}
			if !st.missingIDToken {
				body["id_token"] = idToken
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)

		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

func codexHostAcquireBinding(t *testing.T) launch.FileBinding {
	t.Helper()
	fb := agents.Codex{}.AuthSpec().FileBindings[0]
	if fb.HostAcquire == nil {
		t.Fatal("Codex binding must declare HostAcquire for host-side seeding")
	}
	return fb
}

func TestCodexHostAcquire_PollToTokenHappyPath(t *testing.T) {
	st := &codexDeviceServerState{pendingPolls: 2, userCodeInterval: 1}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()

	// Drive the poll loop deterministically: no real sleeps.
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	var out bytes.Buffer
	opener := &recordingOpener{}
	fb := codexHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    opener,
		Out:        &out,
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	if len(secret.Value) == 0 {
		t.Fatal("HostAcquire returned empty Secret on the happy path")
	}
	// The acquired Secret must round-trip the binding's own Render.
	if _, err := fb.Render(secret); err != nil {
		t.Fatalf("acquired Secret rejected by Render: %v", err)
	}
	// P0: the envelope must be FUNCTIONAL, not merely parseable.
	var env struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(secret.Value, &env); err != nil {
		t.Fatalf("unmarshal seeded envelope: %v", err)
	}
	if env.AuthMode != "chatgpt" {
		t.Errorf("auth_mode = %q, want chatgpt", env.AuthMode)
	}
	if env.Tokens.RefreshToken == "" {
		t.Error("seeded refresh_token is empty (P1: needed for codexPreLaunchRefresh)")
	}
	if env.Tokens.IDToken == "" {
		t.Error("seeded id_token is empty (P0: non-functional for in-container ChatGPT mode)")
	}
	if env.Tokens.AccountID != "acct-xyz" {
		t.Errorf("seeded account_id = %q, want acct-xyz (P0: parsed from id_token claim)", env.Tokens.AccountID)
	}
}

func TestCodexHostAcquire_DeviceCodeRequestFailureIsNonFatal(t *testing.T) {
	// A usercode endpoint that 404s (device-code login not enabled for
	// this server) must surface a clean error so the launcher falls back
	// to the in-container login, never the raw body.
	const leak = "usercode-secret-hint-DO-NOT-LEAK"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deviceauth/usercode") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, leak)
			return
		}
		t.Errorf("unexpected path after a failed usercode request: %s", r.URL.Path)
	}))
	defer srv.Close()

	fb := codexHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected an error when the device-code request fails")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty when the device-code request fails")
	}
	if strings.Contains(err.Error(), leak) {
		t.Errorf("raw device-code body leaked into error: %v", err)
	}
}

func TestCodexHostAcquire_UserCodeSurfacedViaOut(t *testing.T) {
	st := &codexDeviceServerState{}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	var out bytes.Buffer
	fb := codexHostAcquireBinding(t)
	_, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &out,
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	if !strings.Contains(out.String(), "WXYZ-1234") {
		t.Errorf("Out missing the user_code; got %q", out.String())
	}
	if !strings.Contains(out.String(), "auth.openai.com/codex/device") {
		t.Errorf("Out missing the verification URL; got %q", out.String())
	}
}

func TestCodexHostAcquire_SlowDownIncreasesInterval(t *testing.T) {
	st := &codexDeviceServerState{slowDownOnFirstPoll: true, pendingPolls: 1, userCodeInterval: 2}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()

	var mu sync.Mutex
	var waits []time.Duration
	restore := agents.SetCodexDeviceSleepForTest(func(_ context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return nil
	})
	defer restore()

	fb := codexHostAcquireBinding(t)
	_, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) < 2 {
		t.Fatalf("recorded %d waits, want >=2 (slow-down + pending)", len(waits))
	}
	// First wait is after the 429: base interval (2s) + the RFC 8628 +5s
	// floor = 7s. The wait must be strictly greater than the base interval.
	if waits[0] <= 2*time.Second {
		t.Errorf("first wait = %v, want > 2s (the +5s slow-down floor applied)", waits[0])
	}
	if waits[0] != 7*time.Second {
		t.Errorf("first wait = %v, want 7s (2s base + 5s floor)", waits[0])
	}
}

func TestCodexHostAcquire_ExpiredTokenCleanError(t *testing.T) {
	const leak = "expired-secret-hint-DO-NOT-LEAK"
	st := &codexDeviceServerState{pollTerminalStatus: http.StatusGone, tokenErrorBody: leak}
	// pollTerminalStatus is handled before the body write in the fake, so
	// reuse tokenErrorBody only conceptually; assert the status leak path.
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	fb := codexHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected a terminal error when the device poll returns an expired/denied status")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty on a terminal poll error")
	}
	if strings.Contains(err.Error(), "expired or denied") {
		t.Errorf("raw poll body leaked into error: %v", err)
	}
}

func TestCodexHostAcquire_AccessDeniedCleanError(t *testing.T) {
	st := &codexDeviceServerState{pollTerminalStatus: http.StatusUnauthorized}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	fb := codexHostAcquireBinding(t)
	_, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected a terminal error when the device authorization is denied")
	}
}

func TestCodexHostAcquire_BrowserOpenFailureNonFatalButSurfacesURLFirst(t *testing.T) {
	st := &codexDeviceServerState{}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	var out bytes.Buffer
	fb := codexHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{err: io.ErrClosedPipe},
		Out:        &out,
	})
	if err == nil {
		t.Fatal("expected an acquire error when the browser cannot be opened so the launcher falls back")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty when the consent URL cannot be opened")
	}
	// The verification URL + user_code must have been surfaced BEFORE the
	// browser open was attempted, so a headless user can still proceed.
	if !strings.Contains(out.String(), "WXYZ-1234") {
		t.Errorf("user_code not surfaced before browser open; Out = %q", out.String())
	}
}

func TestCodexHostAcquire_TokenErrorRedaction(t *testing.T) {
	const leak = "token-secret-hint-DO-NOT-LEAK"
	st := &codexDeviceServerState{
		tokenStatus:    http.StatusBadRequest,
		tokenErrorBody: `{"error":"invalid_grant","error_description":"` + leak + `"}`,
	}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	fb := codexHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected an error on a 400 token exchange so the launcher falls back")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty on a failed token exchange")
	}
	if strings.Contains(err.Error(), leak) {
		t.Errorf("provider error_description leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err = %v, want the standard error code invalid_grant surfaced", err)
	}
}

func TestCodexHostAcquire_MissingRefreshTokenIsFatalAcquire(t *testing.T) {
	st := &codexDeviceServerState{refreshToken: "-"} // force empty refresh_token
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	fb := codexHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected an error when the token response omits refresh_token (P1: dead-on-arrival envelope)")
	}
	if len(secret.Value) != 0 {
		t.Error("Secret must be empty when no refresh_token is returned")
	}
}

func TestCodexHostAcquire_ContextCancellationStopsPoll(t *testing.T) {
	st := &codexDeviceServerState{pendingPolls: 100} // never completes
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// The sleep seam cancels the context on the first wait, modeling a
	// user killing the launch mid-poll.
	restore := agents.SetCodexDeviceSleepForTest(func(c context.Context, _ time.Duration) error {
		cancel()
		return c.Err()
	})
	defer restore()

	fb := codexHostAcquireBinding(t)
	_, err := fb.HostAcquire(ctx, launch.HostAcquireDeps{
		Ctx:        ctx,
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected a cancellation error when ctx is cancelled mid-poll")
	}
}

// TestCodexHostAcquire_MalformedIDTokenLeavesAccountIDEmpty proves a
// malformed id_token is non-fatal for account_id: the envelope is still
// produced (with the id_token preserved) but account_id stays empty,
// rather than failing the whole acquire.
func TestCodexHostAcquire_MalformedIDTokenLeavesAccountIDEmpty(t *testing.T) {
	st := &codexDeviceServerState{idToken: "not-a-jwt"}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	fb := codexHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v (a malformed id_token must not fail the acquire)", err)
	}
	var env struct {
		Tokens struct {
			IDToken   string `json:"id_token"`
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(secret.Value, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Tokens.IDToken != "not-a-jwt" {
		t.Errorf("id_token = %q, want preserved verbatim", env.Tokens.IDToken)
	}
	if env.Tokens.AccountID != "" {
		t.Errorf("account_id = %q, want empty for a malformed id_token", env.Tokens.AccountID)
	}
}

// TestCodexHostAcquire_MalformedProviderResponses covers the
// API-boundary error branches: a usercode/poll/token response that is
// well-formed HTTP but malformed or missing required fields must surface
// a clean acquire error (the launcher falls back) rather than seeding a
// broken credential. These are the failure modes a misbehaving provider
// would actually produce.
func TestCodexHostAcquire_MalformedProviderResponses(t *testing.T) {
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	cases := []struct {
		name string
		st   *codexDeviceServerState
	}{
		{"usercode body not JSON", &codexDeviceServerState{usercodeRawBody: "not json"}},
		{"usercode missing fields", &codexDeviceServerState{usercodeRawBody: `{"interval":1}`}},
		{"poll body not JSON", &codexDeviceServerState{pollRawBody: "not json"}},
		{"poll missing authorization_code", &codexDeviceServerState{pollRawBody: `{"code_verifier":"v"}`}},
		{"token response missing access_token", &codexDeviceServerState{noAccessToken: true}},
		{"token error without parseable code", &codexDeviceServerState{tokenStatus: http.StatusBadGateway, tokenErrorBody: "upstream is down"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeDeviceAuthServer(t, tc.st)
			defer srv.Close()
			fb := codexHostAcquireBinding(t)
			secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
				Ctx:        context.Background(),
				HTTPClient: codexTestHTTPClient(srv.URL),
				Browser:    &recordingOpener{},
				Out:        &bytes.Buffer{},
			})
			if err == nil {
				t.Fatalf("expected an acquire error for %q so the launcher falls back", tc.name)
			}
			if len(secret.Value) != 0 {
				t.Errorf("Secret must be empty for %q", tc.name)
			}
		})
	}
}

// TestCodexHostAcquire_TokenErrorBareStatus exercises the redaction
// fallback: a non-2xx token response whose body carries no RFC 6749
// `error` field surfaces the bare HTTP status with no body leak.
func TestCodexHostAcquire_TokenErrorBareStatus(t *testing.T) {
	const leak = "bare-status-secret-DO-NOT-LEAK"
	st := &codexDeviceServerState{tokenStatus: http.StatusInternalServerError, tokenErrorBody: leak}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	fb := codexHostAcquireBinding(t)
	_, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected an error on a 500 token exchange")
	}
	if strings.Contains(err.Error(), leak) {
		t.Errorf("raw token error body leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want the bare HTTP status surfaced", err)
	}
}

// TestCodexHostAcquire_MissingIDTokenLeavesAccountIDEmpty proves an
// absent id_token is non-fatal: the envelope is produced with empty
// id_token + account_id (the in-container CLI can re-auth if needed)
// rather than aborting the seed.
func TestCodexHostAcquire_MissingIDTokenLeavesAccountIDEmpty(t *testing.T) {
	st := &codexDeviceServerState{missingIDToken: true}
	srv := fakeDeviceAuthServer(t, st)
	defer srv.Close()
	restore := agents.SetCodexDeviceSleepForTest(func(context.Context, time.Duration) error { return nil })
	defer restore()

	fb := codexHostAcquireBinding(t)
	secret, err := fb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:        context.Background(),
		HTTPClient: codexTestHTTPClient(srv.URL),
		Browser:    &recordingOpener{},
		Out:        &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v (a missing id_token must not fail the acquire)", err)
	}
	var env struct {
		Tokens struct {
			IDToken   string `json:"id_token"`
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(secret.Value, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Tokens.AccountID != "" {
		t.Errorf("account_id = %q, want empty when id_token is absent", env.Tokens.AccountID)
	}
}

// TestCodexAccountID_ParsesAndRejects exercises codexAccountIDFromIDToken
// (via the exported test shim) on a well-formed JWT and on malformed
// inputs.
func TestCodexAccountID_ParsesAndRejects(t *testing.T) {
	good := codexUnsignedJWT(t, "acct-123")
	got, err := agents.CodexAccountIDFromIDTokenForTest(good)
	if err != nil {
		t.Fatalf("parse good JWT: %v", err)
	}
	if got != "acct-123" {
		t.Errorf("account_id = %q, want acct-123", got)
	}

	for _, bad := range []string{"", "onlyone", "two.parts", "a.b.c.d"} {
		if _, err := agents.CodexAccountIDFromIDTokenForTest(bad); err == nil {
			t.Errorf("expected error for malformed JWT %q", bad)
		}
	}

	// A well-formed JWT whose payload lacks the claim must error.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))
	noClaim := header + "." + payload + ".sig"
	if _, err := agents.CodexAccountIDFromIDTokenForTest(noClaim); err == nil {
		t.Error("expected error for a JWT missing chatgpt_account_id")
	}

	// A well-formed three-segment JWT whose payload is valid base64url but
	// NOT valid JSON must surface a parse error, not a panic.
	badJSONPayload := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	badJSON := header + "." + badJSONPayload + ".sig"
	if _, err := agents.CodexAccountIDFromIDTokenForTest(badJSON); err == nil {
		t.Error("expected error for a JWT whose payload is not valid JSON")
	}
}
