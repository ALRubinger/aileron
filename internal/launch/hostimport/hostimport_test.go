package hostimport

import (
	"errors"
	"strings"
	"testing"
)

// TestExtract_UnknownAgentRejected pins the contract that only claude
// and codex are supported; every other registry agent is rejected with
// ErrUnsupportedAgent before any host read happens.
func TestExtract_UnknownAgentRejected(t *testing.T) {
	for _, agent := range []string{"goose", "opencode", "pi", "", "Claude", "CODEX"} {
		_, err := Extract(agent, Options{})
		if !errors.Is(err, ErrUnsupportedAgent) {
			t.Errorf("Extract(%q): err = %v, want ErrUnsupportedAgent", agent, err)
		}
	}
}

// TestErrLinuxKeyringUnsupported_NamesRecovery pins that the deferral
// sentinel's message names both recovery paths (file mode + interactive
// in-container login) so the CLI can surface it verbatim.
func TestErrLinuxKeyringUnsupported_NamesRecovery(t *testing.T) {
	msg := ErrLinuxKeyringUnsupported.Error()
	if !strings.Contains(msg, "file mode") {
		t.Errorf("ErrLinuxKeyringUnsupported = %q, want it to name file mode", msg)
	}
	if !strings.Contains(msg, "--sandbox=docker") {
		t.Errorf("ErrLinuxKeyringUnsupported = %q, want it to name interactive in-container login", msg)
	}
}

// TestErrNotAuthenticated_Distinct guards that the sentinels are
// distinct errors, so callers can branch on them independently.
func TestErrNotAuthenticated_Distinct(t *testing.T) {
	if errors.Is(ErrNotAuthenticated, ErrUnsupportedAgent) ||
		errors.Is(ErrUnsupportedAgent, ErrLinuxKeyringUnsupported) ||
		errors.Is(ErrNotAuthenticated, ErrLinuxKeyringUnsupported) {
		t.Fatal("sentinels must be distinct errors")
	}
}

// TestExtract_SupportedAgentsReadHost confirms the supported agents
// reach the platform extractor. With no real host install and no
// fixture override, the extractor surfaces ErrNotAuthenticated (file
// absent / keychain item missing) rather than ErrUnsupportedAgent. The
// override points at a guaranteed-absent path so the test does not
// depend on the developer's actual ~/.claude or ~/.codex.
func TestExtract_SupportedAgentsReadHost(t *testing.T) {
	missing := t.TempDir() + "/does-not-exist.json"
	for _, agent := range []string{AgentClaude, AgentCodex} {
		_, err := Extract(agent, Options{FilePathOverride: missing})
		if !errors.Is(err, ErrNotAuthenticated) {
			t.Errorf("Extract(%q, missing override): err = %v, want ErrNotAuthenticated", agent, err)
		}
	}
}
