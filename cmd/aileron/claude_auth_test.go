package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// stubClaudeVaultPresence installs a fixed vault-presence probe result for
// the duration of a test and restores the original on cleanup. It lets the
// resolveClaudeAuthMode tests exercise every slot state without a running
// daemon.
func stubClaudeVaultPresence(t *testing.T, p claudeVaultPresence) {
	t.Helper()
	orig := claudeVaultPresenceFn
	t.Cleanup(func() { claudeVaultPresenceFn = orig })
	claudeVaultPresenceFn = func(context.Context) claudeVaultPresence { return p }
}

// failClaudeVaultPresence installs a probe that fails the test if it is ever
// invoked. It guards the explicit-flag short-circuit: an explicit
// --claude-auth value must resolve before any vault probe runs.
func failClaudeVaultPresence(t *testing.T) {
	t.Helper()
	orig := claudeVaultPresenceFn
	t.Cleanup(func() { claudeVaultPresenceFn = orig })
	claudeVaultPresenceFn = func(context.Context) claudeVaultPresence {
		t.Helper()
		t.Fatal("vault presence probe ran despite an explicit --claude-auth value")
		return claudeVaultPresence{}
	}
}

// failingReader fails the test if Read is ever called. It guards the P2
// stdin contract: the non-interactive resolution path must never touch
// stdin (so a piped stdin destined for the agent is preserved).
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("stdin was read on the non-interactive path; the supplied reader must be left untouched")
	return 0, io.EOF
}

// capturedAuthMode runs `run` with the launchFn seam installed and returns
// the auth mode threaded onto the launched claude agent. It asserts the
// launch succeeded and that config.Agent is an agents.Claude value (the swap
// stores a value, not a pointer).
func capturedAuthMode(t *testing.T, args []string) agents.ClaudeAuthMode {
	t.Helper()
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })

	var captured launch.LaunchConfig
	launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
		captured = cfg
		return launch.LaunchResult{ExitCode: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	code := run(args, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(%v) exit = %d (stderr=%q)", args, code, stderr.String())
	}
	cl, ok := captured.Agent.(agents.Claude)
	if !ok {
		t.Fatalf("LaunchConfig.Agent = %T, want agents.Claude", captured.Agent)
	}
	return cl.AuthMode()
}

// TestRun_LaunchThreadsClaudeAuthModeExplicit covers Unit 2: an explicit
// --claude-auth value reaches the swapped agents.Claude in LaunchConfig.Agent
// (in both before-agent and trailing positions, per Unit 1's position
// independence).
func TestRun_LaunchThreadsClaudeAuthModeExplicit(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want agents.ClaudeAuthMode
	}{
		{"api-key before agent", []string{"launch", "--claude-auth=api-key", "claude"}, agents.ClaudeAuthModeAPIKey},
		{"subscription before agent", []string{"launch", "--claude-auth=subscription", "claude"}, agents.ClaudeAuthModeSubscription},
		{"api-key trailing eq", []string{"launch", "claude", "--claude-auth=api-key"}, agents.ClaudeAuthModeAPIKey},
		{"api-key trailing space", []string{"launch", "claude", "--claude-auth", "api-key"}, agents.ClaudeAuthModeAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capturedAuthMode(t, tc.args); got != tc.want {
				t.Errorf("threaded auth mode = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRun_LaunchClaudeAuthFlagNotForwarded locks that --claude-auth is
// consumed by aileron and never leaks into the agent's args.
func TestRun_LaunchClaudeAuthFlagNotForwarded(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })
	var captured launch.LaunchConfig
	launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
		captured = cfg
		return launch.LaunchResult{ExitCode: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "claude", "--claude-auth=api-key", "--", "--foo"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	for _, a := range captured.Args {
		if strings.Contains(a, "claude-auth") {
			t.Errorf("--claude-auth leaked to agent args: %v", captured.Args)
		}
	}
	if len(captured.Args) != 1 || captured.Args[0] != "--foo" {
		t.Errorf("agent args = %v, want [--foo]", captured.Args)
	}
}

// TestRun_LaunchClaudeAuthInvalidFailsFast covers Unit 1's fail-fast: a
// non-empty value that is not a recognized mode exits 1 with a clear error.
func TestRun_LaunchClaudeAuthInvalidFailsFast(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })
	called := false
	launchFn = func(_ context.Context, _ launch.LaunchConfig) (launch.LaunchResult, error) {
		called = true
		return launch.LaunchResult{ExitCode: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "--claude-auth=bogus", "claude"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr=%q)", code, stderr.String())
	}
	if called {
		t.Error("launchFn was called despite invalid --claude-auth")
	}
	if !strings.Contains(stderr.String(), "claude-auth") {
		t.Errorf("error did not mention claude-auth: %q", stderr.String())
	}
}

// TestRun_LaunchNonClaudeAgentNeverResolvesMode covers decision #3: a
// non-claude agent passes through unchanged, never prompts, and never reads
// stdin. We force a TTY and pass a failingReader as os.Stdin would be — but
// since run() reads os.Stdin directly, we assert via the captured agent
// identity that no swap happened, and that the prompt path was not entered
// (isTTYFn true but no claude => resolver never called, no panic on stdin).
func TestRun_LaunchNonClaudeAgentNeverResolvesMode(t *testing.T) {
	origTTY := isTTYFn
	t.Cleanup(func() { isTTYFn = origTTY })
	isTTYFn = func() bool { return true } // would trigger a prompt for claude

	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })
	var captured launch.LaunchConfig
	launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
		captured = cfg
		return launch.LaunchResult{ExitCode: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	// pi is a non-claude agent; with a TTY forced, a claude launch would
	// block reading os.Stdin. pi must not, proving the resolver is skipped.
	code := run([]string{"launch", "--local", "pi"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	if _, ok := captured.Agent.(agents.Pi); !ok {
		t.Fatalf("LaunchConfig.Agent = %T, want agents.Pi (unchanged)", captured.Agent)
	}
}

// --- resolveClaudeAuthMode unit tests (Unit 3) ---

func TestResolveClaudeAuthMode_ExplicitValues(t *testing.T) {
	cases := []struct {
		in   string
		want agents.ClaudeAuthMode
	}{
		{"subscription", agents.ClaudeAuthModeSubscription},
		{"sub", agents.ClaudeAuthModeSubscription},
		{"s", agents.ClaudeAuthModeSubscription},
		{"api-key", agents.ClaudeAuthModeAPIKey},
		{"apikey", agents.ClaudeAuthModeAPIKey},
		{"key", agents.ClaudeAuthModeAPIKey},
		{"API-KEY", agents.ClaudeAuthModeAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			// isTTY true + a failing reader proves an explicit value bypasses
			// the prompt entirely (no stdin read even on a TTY); a failing
			// probe proves it also bypasses the vault entirely.
			failClaudeVaultPresence(t)
			got, err := resolveClaudeAuthMode(tc.in, true, failingReader{t}, io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("mode = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveClaudeAuthMode_InvalidExplicit(t *testing.T) {
	failClaudeVaultPresence(t)
	_, err := resolveClaudeAuthMode("nonsense", false, failingReader{t}, io.Discard)
	if err == nil {
		t.Fatal("expected an error for an invalid explicit value")
	}
	if !strings.Contains(err.Error(), "claude-auth") {
		t.Errorf("error %q should mention the flag", err.Error())
	}
}

// TestResolveClaudeAuthMode_NonInteractiveDefault is the P2 stdin contract:
// empty flag + no TTY + empty vault must return subscription WITHOUT reading
// stdin.
func TestResolveClaudeAuthMode_NonInteractiveDefault(t *testing.T) {
	stubClaudeVaultPresence(t, claudeVaultPresence{})
	var stdout bytes.Buffer
	got, err := resolveClaudeAuthMode("", false, failingReader{t}, &stdout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != agents.ClaudeAuthModeSubscription {
		t.Errorf("mode = %v, want subscription", got)
	}
	if stdout.Len() != 0 {
		t.Errorf("non-interactive path wrote prompt text: %q", stdout.String())
	}
}

// TestResolveClaudeAuthMode_InteractivePrompt covers the TTY first-run prompt:
// scripted stdin yields the chosen mode and prompt text is emitted.
func TestResolveClaudeAuthMode_InteractivePrompt(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		want  agents.ClaudeAuthMode
	}{
		{"api-key word", "api-key\n", agents.ClaudeAuthModeAPIKey},
		{"subscription word", "subscription\n", agents.ClaudeAuthModeSubscription},
		{"numeric 2", "2\n", agents.ClaudeAuthModeAPIKey},
		{"numeric 1", "1\n", agents.ClaudeAuthModeSubscription},
		{"bare enter defaults", "\n", agents.ClaudeAuthModeSubscription},
		{"reask then valid", "huh?\napi-key\n", agents.ClaudeAuthModeAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubClaudeVaultPresence(t, claudeVaultPresence{})
			var stdout bytes.Buffer
			got, err := resolveClaudeAuthMode("", true, strings.NewReader(tc.stdin), &stdout)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("mode = %v, want %v", got, tc.want)
			}
			if !strings.Contains(stdout.String(), "How should Claude authenticate?") {
				t.Errorf("prompt text not emitted: %q", stdout.String())
			}
		})
	}
}

// TestResolveClaudeAuthMode_PromptReasksOnInvalid asserts the re-ask line is
// printed when the first answer is unrecognized.
func TestResolveClaudeAuthMode_PromptReasksOnInvalid(t *testing.T) {
	stubClaudeVaultPresence(t, claudeVaultPresence{})
	var stdout bytes.Buffer
	got, err := resolveClaudeAuthMode("", true, strings.NewReader("xyz\nsubscription\n"), &stdout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != agents.ClaudeAuthModeSubscription {
		t.Errorf("mode = %v, want subscription", got)
	}
	if !strings.Contains(stdout.String(), "unrecognized choice") {
		t.Errorf("expected re-ask message, got %q", stdout.String())
	}
}

// --- vault-aware resolution (#1381) ---

// TestResolveClaudeAuthMode_VaultPresence is the #1381 matrix: with an empty
// --claude-auth flag, the four vault slot states resolve deterministically on
// BOTH the TTY and non-TTY paths. A populated slot must resolve without
// prompting and without reading stdin (failingReader guards the latter); only
// the neither-populated state falls back to prompt (TTY) or subscription
// default (non-TTY).
func TestResolveClaudeAuthMode_VaultPresence(t *testing.T) {
	cases := []struct {
		name     string
		presence claudeVaultPresence
		want     agents.ClaudeAuthMode
	}{
		{"subscription only", claudeVaultPresence{subscription: true}, agents.ClaudeAuthModeSubscription},
		{"api-key only", claudeVaultPresence{apiKey: true}, agents.ClaudeAuthModeAPIKey},
		{"both -> subscription tie-break", claudeVaultPresence{subscription: true, apiKey: true}, agents.ClaudeAuthModeSubscription},
	}
	for _, tc := range cases {
		for _, tty := range []bool{true, false} {
			name := tc.name
			if tty {
				name += " (tty)"
			} else {
				name += " (non-tty)"
			}
			t.Run(name, func(t *testing.T) {
				stubClaudeVaultPresence(t, tc.presence)
				var stdout bytes.Buffer
				// failingReader proves the populated-slot path never reads
				// stdin, even on a TTY where a prompt would otherwise read it.
				got, err := resolveClaudeAuthMode("", tty, failingReader{t}, &stdout)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Errorf("mode = %v, want %v", got, tc.want)
				}
				if stdout.Len() != 0 {
					t.Errorf("populated-slot path wrote output (should not prompt): %q", stdout.String())
				}
			})
		}
	}
}

// TestResolveClaudeAuthMode_PopulatedSlotDoesNotPrompt is the #1381
// regression: an empty flag on a TTY with a populated slot must NOT enter
// the interactive prompt. Before the fix the resolver always prompted on a
// TTY regardless of stored credentials. We force a TTY and supply scripted
// stdin that, if the prompt ran, would select the WRONG mode — proving the
// vault answer (not the stdin) drives the result.
func TestResolveClaudeAuthMode_PopulatedSlotDoesNotPrompt(t *testing.T) {
	t.Run("subscription slot, stdin would pick api-key", func(t *testing.T) {
		stubClaudeVaultPresence(t, claudeVaultPresence{subscription: true})
		var stdout bytes.Buffer
		// "2" / "api-key" is what the prompt would consume; if the prompt
		// ran, the result would be api-key. We assert subscription.
		got, err := resolveClaudeAuthMode("", true, strings.NewReader("api-key\n"), &stdout)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != agents.ClaudeAuthModeSubscription {
			t.Errorf("mode = %v, want subscription (vault should win, prompt must not run)", got)
		}
		if strings.Contains(stdout.String(), "How should Claude authenticate?") {
			t.Errorf("prompt ran despite a populated subscription slot: %q", stdout.String())
		}
	})
	t.Run("api-key slot, stdin would pick subscription", func(t *testing.T) {
		stubClaudeVaultPresence(t, claudeVaultPresence{apiKey: true})
		var stdout bytes.Buffer
		got, err := resolveClaudeAuthMode("", true, strings.NewReader("subscription\n"), &stdout)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != agents.ClaudeAuthModeAPIKey {
			t.Errorf("mode = %v, want api-key (vault should win, prompt must not run)", got)
		}
		if strings.Contains(stdout.String(), "How should Claude authenticate?") {
			t.Errorf("prompt ran despite a populated api-key slot: %q", stdout.String())
		}
	})
}

// TestResolveClaudeAuthMode_ExplicitWinsOverVault locks flag precedence: an
// explicit flag short-circuits before the vault probe, so a stored
// subscription credential does not override an explicit --claude-auth=api-key
// (and vice versa). The probe is stubbed to the OPPOSITE slot to prove the
// flag, not the vault, decides.
func TestResolveClaudeAuthMode_ExplicitWinsOverVault(t *testing.T) {
	cases := []struct {
		name     string
		flag     string
		presence claudeVaultPresence
		want     agents.ClaudeAuthMode
	}{
		{"flag api-key beats stored subscription", "api-key", claudeVaultPresence{subscription: true}, agents.ClaudeAuthModeAPIKey},
		{"flag subscription beats stored api-key", "subscription", claudeVaultPresence{apiKey: true}, agents.ClaudeAuthModeSubscription},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An explicit value must resolve before the probe, so installing a
			// failing probe (not tc.presence) proves the short-circuit.
			failClaudeVaultPresence(t)
			got, err := resolveClaudeAuthMode(tc.flag, true, failingReader{t}, io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("mode = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- probeClaudeVaultPresence (daemon-access path) ---

// TestProbeClaudeVaultPresence_Wiring exercises the real probe against a
// fake daemon: it must GET the per-agent credential endpoint once per
// purpose, send the bearer token, omit the query for the default (oauth)
// purpose and append `?purpose=apikey` for the api-key slot, and map 200 ->
// present / 404 -> absent. The seams over defaultStateDirFn and
// discoveryReadFn let it run without a real ~/.aileron or daemon.json.
func TestProbeClaudeVaultPresence_Wiring(t *testing.T) {
	cases := []struct {
		name         string
		oauthStatus  int
		apikeyStatus int
		wantSub      bool
		wantAPIKey   bool
	}{
		{"both present", http.StatusOK, http.StatusOK, true, true},
		{"subscription only", http.StatusOK, http.StatusNotFound, true, false},
		{"api-key only", http.StatusNotFound, http.StatusOK, false, true},
		{"neither", http.StatusNotFound, http.StatusNotFound, false, false},
		{"locked vault counts as absent", http.StatusLocked, http.StatusLocked, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotOAuthReq, gotAPIKeyReq, gotToken bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/vault/agents/claude/credentials" {
					t.Errorf("unexpected path %q", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if r.Header.Get("Authorization") == "Bearer tok-123" {
					gotToken = true
				}
				switch r.URL.Query().Get("purpose") {
				case "":
					// Default (oauth) purpose omits the query parameter.
					gotOAuthReq = true
					w.WriteHeader(tc.oauthStatus)
				case "apikey":
					gotAPIKeyReq = true
					w.WriteHeader(tc.apikeyStatus)
				default:
					t.Errorf("unexpected purpose %q", r.URL.Query().Get("purpose"))
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			t.Cleanup(srv.Close)

			origState := defaultStateDirFn
			origDisc := discoveryReadFn
			t.Cleanup(func() {
				defaultStateDirFn = origState
				discoveryReadFn = origDisc
			})
			defaultStateDirFn = func() (string, error) { return t.TempDir(), nil }
			discoveryReadFn = func(string) (discovery.Info, error) {
				return discovery.Info{URL: srv.URL, Token: "tok-123"}, nil
			}

			got := probeClaudeVaultPresence(context.Background())
			if got.subscription != tc.wantSub {
				t.Errorf("subscription = %v, want %v", got.subscription, tc.wantSub)
			}
			if got.apiKey != tc.wantAPIKey {
				t.Errorf("apiKey = %v, want %v", got.apiKey, tc.wantAPIKey)
			}
			if !gotOAuthReq {
				t.Error("probe did not request the oauth slot")
			}
			if !gotAPIKeyReq {
				t.Error("probe did not request the apikey slot")
			}
			if !gotToken {
				t.Error("probe did not send the daemon bearer token")
			}
		})
	}
}

// TestProbeClaudeVaultPresence_NoDaemon proves the probe degrades to
// "both slots absent" when the daemon is unreachable (discovery miss), so a
// probe failure never escalates into a launch failure — resolution falls
// back to its prior prompt/default behavior.
func TestProbeClaudeVaultPresence_NoDaemon(t *testing.T) {
	origState := defaultStateDirFn
	origDisc := discoveryReadFn
	t.Cleanup(func() {
		defaultStateDirFn = origState
		discoveryReadFn = origDisc
	})
	defaultStateDirFn = func() (string, error) { return t.TempDir(), nil }
	discoveryReadFn = func(string) (discovery.Info, error) {
		return discovery.Info{}, discovery.ErrNotRunning
	}
	got := probeClaudeVaultPresence(context.Background())
	if got.subscription || got.apiKey {
		t.Errorf("probe with no daemon = %+v, want both absent", got)
	}
}
