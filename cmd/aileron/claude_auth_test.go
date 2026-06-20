package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

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
			// the prompt entirely (no stdin read even on a TTY).
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
	_, err := resolveClaudeAuthMode("nonsense", false, failingReader{t}, io.Discard)
	if err == nil {
		t.Fatal("expected an error for an invalid explicit value")
	}
	if !strings.Contains(err.Error(), "claude-auth") {
		t.Errorf("error %q should mention the flag", err.Error())
	}
}

// TestResolveClaudeAuthMode_NonInteractiveDefault is the P2 stdin contract:
// empty flag + no TTY must return subscription WITHOUT reading stdin.
func TestResolveClaudeAuthMode_NonInteractiveDefault(t *testing.T) {
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
