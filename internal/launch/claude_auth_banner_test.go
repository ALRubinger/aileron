package launch

import (
	"bytes"
	"strings"
	"testing"
)

// claudeModeAgent is a test agent that reports a name and a display auth
// mode, satisfying both Agent and claudeAuthModeReader.
type claudeModeAgent struct {
	name string
	mode AuthModeDisplay
}

func (a claudeModeAgent) Name() string           { return a.name }
func (a claudeModeAgent) BinaryNames() []string  { return []string{a.name} }
func (a claudeModeAgent) Args(_ Mode) []string   { return nil }
func (a claudeModeAgent) Env() map[string]string { return nil }
func (a claudeModeAgent) LLMEndpointEnv() string { return "" }
func (a claudeModeAgent) ConfigureMCP(string, map[string]string, string, Mode) ([]string, []MCPMount, error) {
	return nil, nil, nil
}
func (a claudeModeAgent) AuthSpec() AuthSpec               { return AuthSpec{} }
func (a claudeModeAgent) AuthModeDisplay() AuthModeDisplay { return a.mode }

func TestPrintClaudeAuthBanner_SandboxClaude(t *testing.T) {
	cases := []struct {
		name string
		mode AuthModeDisplay
		want string
	}{
		{"subscription", AuthModeDisplaySubscription, "Claude auth mode: subscription (Pro/Max)"},
		{"api-key", AuthModeDisplayAPIKey, "Claude auth mode: API key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printClaudeAuthBanner(&buf, claudeModeAgent{name: "claude", mode: tc.mode}, true)
			got := strings.TrimRight(buf.String(), "\n")
			if got != tc.want {
				t.Errorf("banner = %q, want %q", got, tc.want)
			}
			// Exactly one line.
			if n := strings.Count(buf.String(), "\n"); n != 1 {
				t.Errorf("banner emitted %d newlines, want exactly 1: %q", n, buf.String())
			}
		})
	}
}

// TestPrintClaudeAuthBanner_HostSuppressed is the P0 false-signal guard: on
// host launch (sandboxEnabled=false) the mode is inert, so no banner.
func TestPrintClaudeAuthBanner_HostSuppressed(t *testing.T) {
	var buf bytes.Buffer
	printClaudeAuthBanner(&buf, claudeModeAgent{name: "claude", mode: AuthModeDisplayAPIKey}, false)
	if buf.Len() != 0 {
		t.Errorf("host launch emitted a banner: %q", buf.String())
	}
}

// TestPrintClaudeAuthBanner_NonClaudeSkipped covers decision #5's guard: a
// non-claude agent never gets the banner even on the sandbox path.
func TestPrintClaudeAuthBanner_NonClaudeSkipped(t *testing.T) {
	var buf bytes.Buffer
	printClaudeAuthBanner(&buf, claudeModeAgent{name: "codex", mode: AuthModeDisplayAPIKey}, true)
	if buf.Len() != 0 {
		t.Errorf("non-claude agent emitted a banner: %q", buf.String())
	}
}

// TestPrintClaudeAuthBanner_ClaudeWithoutReaderSkipped covers the case where
// the claude-named agent does not expose AuthModeDisplay: the type assertion
// fails and the banner is silently skipped rather than panicking.
func TestPrintClaudeAuthBanner_ClaudeWithoutReaderSkipped(t *testing.T) {
	var buf bytes.Buffer
	printClaudeAuthBanner(&buf, emptyClaudeAgent{}, true)
	if buf.Len() != 0 {
		t.Errorf("claude agent without AuthModeDisplay emitted a banner: %q", buf.String())
	}
}

// emptyClaudeAgent is named "claude" but does not implement claudeAuthModeReader.
type emptyClaudeAgent struct{}

func (emptyClaudeAgent) Name() string           { return "claude" }
func (emptyClaudeAgent) BinaryNames() []string  { return []string{"claude"} }
func (emptyClaudeAgent) Args(_ Mode) []string   { return nil }
func (emptyClaudeAgent) Env() map[string]string { return nil }
func (emptyClaudeAgent) LLMEndpointEnv() string { return "" }
func (emptyClaudeAgent) ConfigureMCP(string, map[string]string, string, Mode) ([]string, []MCPMount, error) {
	return nil, nil, nil
}
func (emptyClaudeAgent) AuthSpec() AuthSpec { return AuthSpec{} }
