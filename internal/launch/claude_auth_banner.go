package launch

import (
	"fmt"
	"io"
)

// AuthModeDisplay is an agent-agnostic, display-only enumeration of the
// Claude auth mode used to render the active-mode startup banner. It lives
// in the launch package (rather than reusing agents.ClaudeAuthMode) because
// agents imports launch, so the launcher cannot import agents to name that
// type. An agent exposes its mode to the launcher by implementing
// claudeAuthModeReader (an AuthModeDisplay-returning method); this is purely
// for signalling and never feeds AuthSpec selection.
type AuthModeDisplay int

const (
	// AuthModeDisplaySubscription is the Pro/Max OAuth subscription flow.
	// It is the zero value so a bare implementer defaults to subscription.
	AuthModeDisplaySubscription AuthModeDisplay = iota
	// AuthModeDisplayAPIKey is the raw ANTHROPIC_API_KEY flow.
	AuthModeDisplayAPIKey
)

// claudeAuthModeReader is the narrow interface the launcher type-asserts
// config.Agent against to read back its display auth mode. agents.Claude
// satisfies it via AuthModeDisplay(). Keeping the method on a launch-defined
// return type avoids the agents->launch import cycle.
type claudeAuthModeReader interface {
	AuthModeDisplay() AuthModeDisplay
}

// claudeAuthBannerLine renders the active-mode banner line for the given
// display mode. The copy is byte-exact per feedback #1331 Q4.
func claudeAuthBannerLine(mode AuthModeDisplay) string {
	if mode == AuthModeDisplayAPIKey {
		return "Claude auth mode: API key"
	}
	return "Claude auth mode: subscription (Pro/Max)"
}

// printClaudeAuthBanner emits exactly one active-mode banner line to w when
// the launch is a sandbox launch (sandboxEnabled) of the claude agent and
// the agent exposes its mode via claudeAuthModeReader. On host launch the
// auth mode is inert (the launcher never materializes AuthSpec there), so no
// banner is printed to avoid a false signal (P0). Non-claude agents and
// agents that do not expose a mode are silently skipped.
func printClaudeAuthBanner(w io.Writer, agent Agent, sandboxEnabled bool) {
	if !sandboxEnabled || agent == nil || agent.Name() != "claude" {
		return
	}
	reader, ok := agent.(claudeAuthModeReader)
	if !ok {
		return
	}
	fmt.Fprintln(w, claudeAuthBannerLine(reader.AuthModeDisplay()))
}
