package agents_test

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// TestClaude_AuthMode_ReadsBack pins the display-only read-back contract
// (#1340): NewClaude(mode).AuthMode() returns the mode it was built with,
// and a bare Claude{} reports the zero-value subscription mode.
func TestClaude_AuthMode_ReadsBack(t *testing.T) {
	if got := agents.NewClaude(agents.ClaudeAuthModeAPIKey).AuthMode(); got != agents.ClaudeAuthModeAPIKey {
		t.Errorf("NewClaude(APIKey).AuthMode() = %v, want APIKey", got)
	}
	if got := agents.NewClaude(agents.ClaudeAuthModeSubscription).AuthMode(); got != agents.ClaudeAuthModeSubscription {
		t.Errorf("NewClaude(Subscription).AuthMode() = %v, want Subscription", got)
	}
	if got := (agents.Claude{}).AuthMode(); got != agents.ClaudeAuthModeSubscription {
		t.Errorf("Claude{}.AuthMode() = %v, want zero-value Subscription", got)
	}
}

// TestClaude_AuthModeDisplay maps the agent mode onto the launch package's
// display enum the launcher reads for the active-mode banner.
func TestClaude_AuthModeDisplay(t *testing.T) {
	if got := agents.NewClaude(agents.ClaudeAuthModeAPIKey).AuthModeDisplay(); got != launch.AuthModeDisplayAPIKey {
		t.Errorf("APIKey display = %v, want AuthModeDisplayAPIKey", got)
	}
	if got := agents.NewClaude(agents.ClaudeAuthModeSubscription).AuthModeDisplay(); got != launch.AuthModeDisplaySubscription {
		t.Errorf("Subscription display = %v, want AuthModeDisplaySubscription", got)
	}
	if got := (agents.Claude{}).AuthModeDisplay(); got != launch.AuthModeDisplaySubscription {
		t.Errorf("zero-value display = %v, want AuthModeDisplaySubscription", got)
	}
}
