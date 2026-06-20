package agents

import (
	"encoding/json"
	"fmt"

	"github.com/ALRubinger/aileron/internal/launch"
)

// ClaudeAuthMode selects which credential-binding set Claude's AuthSpec
// renders. The zero value is subscription mode so a bare Claude{} (e.g.
// the registry registration in cmd/aileron/main.go) keeps its existing
// behavior; #1340 swaps that registration to NewClaude(selectedMode).
type ClaudeAuthMode int

const (
	// ClaudeAuthModeSubscription is the Pro/Max OAuth flow: a FileBinding
	// at agents/claude/oauth renders .credentials.json. This is the zero
	// value so Claude{} behaves identically to the pre-api-key agent.
	ClaudeAuthModeSubscription ClaudeAuthMode = iota
	// ClaudeAuthModeAPIKey is the raw-key flow: an EnvBinding at
	// agents/claude/apikey renders ANTHROPIC_API_KEY. Selecting this mode
	// never references the oauth slot.
	ClaudeAuthModeAPIKey
)

// Claude is the agent definition for Claude Code.
type Claude struct {
	// authMode selects the credential-binding set AuthSpec returns. The
	// zero value (ClaudeAuthModeSubscription) keeps a bare Claude{} in
	// subscription mode.
	authMode ClaudeAuthMode
}

// NewClaude returns a Claude configured for the given auth mode.
// NewClaude(ClaudeAuthModeSubscription) is equivalent to Claude{}.
func NewClaude(mode ClaudeAuthMode) Claude { return Claude{authMode: mode} }

func (c Claude) Name() string          { return "claude" }
func (c Claude) BinaryNames() []string { return []string{"claude"} }

// AuthMode returns the configured Claude auth mode. This is a display-only
// read-back of the value passed to NewClaude (the zero value is
// ClaudeAuthModeSubscription). It exists so the CLI can confirm which mode
// it threaded onto the launched Claude and so the launcher can surface a
// per-mode banner; it does NOT participate in AuthSpec selection (that
// branching lives in AuthSpec on the unexported authMode field).
func (c Claude) AuthMode() ClaudeAuthMode { return c.authMode }

// AuthModeDisplay maps Claude's auth mode onto the launch package's
// agent-agnostic display enum. The launch package cannot import this
// package (agents already imports launch), so the launcher reads the mode
// back through a launch-local interface that returns launch.AuthModeDisplay
// rather than the agents-defined ClaudeAuthMode. This is display-only and
// never feeds AuthSpec selection.
func (c Claude) AuthModeDisplay() launch.AuthModeDisplay {
	if c.authMode == ClaudeAuthModeAPIKey {
		return launch.AuthModeDisplayAPIKey
	}
	return launch.AuthModeDisplaySubscription
}

// Args tells Claude Code how aggressively to auto-approve, branching on
// the launch trust boundary.
//
// Under ModeSandbox the container is the trust boundary (ADR-0015): the
// agent runs inside Aileron's sandbox, so we pass
// --dangerously-skip-permissions alone to give Claude full YOLO
// autonomy. This mirrors Codex's container posture
// (approval_policy="never" + sandbox_mode="danger-full-access" in
// mergeCodexSandboxConfig). The flag is passed alone, not alongside
// --allowedTools, because --dangerously-skip-permissions subsumes all
// per-tool gating; combining them would be redundant.
//
// Under ModeHost the agent runs directly on the host, so we keep the
// narrower --allowedTools posture that auto-approves only:
//   - Bash, so Claude Code's per-command prompt is suppressed. Per
//     ADR-0015, Aileron is no longer the trust surface for the agent's
//     local shell commands; Claude Code's own approval suppression
//     keeps the agent productive without re-introducing a per-command
//     prompt at the CLI layer.
//   - The Aileron MCP server's tools (`mcp__<launch.MCPServerName>`),
//     so Claude Code does not double-prompt for tools whose execution
//     the daemon already mediates per ADR-0009/0010.
//
// `--allowedTools` accepts a single value with space-separated
// patterns; the bare `mcp__<server>` form whitelists every tool from
// that server (including ones registered later in the session).
func (c Claude) Args(mode launch.Mode) []string {
	if mode == launch.ModeSandbox {
		return []string{"--dangerously-skip-permissions"}
	}
	return []string{"--allowedTools", "Bash(*) mcp__" + launch.MCPServerName}
}

// Env returns Claude-specific environment variables. None today —
// the wrapper-shell dance (CLAUDE_CODE_SHELL pointing at ~/.aileron/bash)
// is gone per ADR-0015.
func (c Claude) Env() map[string]string { return nil }

// LLMEndpointEnv returns the env var Claude Code reads to override the
// Anthropic API base URL. Setting this routes Claude's LLM calls through
// Aileron's embedded gateway.
func (c Claude) LLMEndpointEnv() string { return "ANTHROPIC_BASE_URL" }

// ConfigureMCP returns the CLI flags that register aileron-mcp with
// Claude Code. Claude Code accepts a `--mcp-config <json>` flag whose
// value is a JSON object of MCP servers; we render the one aileron-mcp
// entry with its required env so the daemon's session-id / approval-url
// are visible to the MCP server.
//
// Mode is irrelevant for Claude: the --mcp-config JSON travels with the
// exec command in both host and sandbox modes, and the in-container
// agent reads mcpBin (a container-side path under ModeSandbox)
// directly.
func (c Claude) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string, _ launch.Mode) ([]string, []launch.MCPMount, error) {
	envJSON, err := json.Marshal(mcpEnv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling MCP env: %w", err)
	}
	mcpConfig := fmt.Sprintf(
		`{"mcpServers":{%q:{"command":%q,"env":%s}}}`,
		launch.MCPServerName, mcpBin, string(envJSON),
	)
	return []string{"--mcp-config", mcpConfig}, nil, nil
}

// AuthSpec returns Claude's vault-backed credential descriptor per
// ADR-0025.
//
//   - One FileBinding at agents/claude/oauth renders
//     /home/agent/.claude/.credentials.json (mode 0600). Render is
//     byte-identity over the vault payload; Capture validates the
//     envelope before persisting back. Claude rotates the access
//     token mid-session via a tmpfile + rename dance inside
//     /home/agent/.claude/, so the launcher bind-mounts the parent
//     dir writable.
//   - One StaticFile at /home/agent/.claude.json (mode 0644) writes
//     the onboarding stub so Claude does not paint the theme picker
//     on every launch. The launcher mounts this as an individual
//     file mount so the workspace at /home/agent/ is not masked.
//
// v1 ships FileBinding only — no EnvBinding (R19). v4 milestone
// scope (#747) lists env-var credential injection as out of scope;
// the file path delivers equivalent silent-first-launch UX without
// breaching the scope boundary.
//
// AuthSpec is mode-aware: ClaudeAuthModeAPIKey returns a single
// EnvBinding at agents/claude/apikey (ANTHROPIC_API_KEY) instead of the
// oauth FileBinding. The two modes route to physically distinct vault
// slots, so a single launch never references both: api-key never
// touches the oauth slot and subscription never touches the apikey
// slot. The onboarding StaticFile is vault-independent and shared by
// both modes so an api-key launch is silent first-run too (it suppresses
// the theme picker / trust dialog regardless of which credential the
// agent ends up using).
func (c Claude) AuthSpec() launch.AuthSpec {
	if c.authMode == ClaudeAuthModeAPIKey {
		return launch.AuthSpec{
			EnvBindings: []launch.EnvBinding{{
				VaultPath: claudeAPIKeyVaultPath,
				// Required=false so an empty vault triggers the host-paste
				// acquirer (and, failing that, the in-container login)
				// rather than failing the launch.
				Required: false,
				Render:   claudeAPIKeyRender,
				// HostAcquire runs a host-terminal paste of the raw
				// ANTHROPIC_API_KEY when the apikey slot is empty and
				// host-login is enabled. The launcher owns the vault PUT
				// (to agents/claude/apikey) and the validating Render; a
				// cancel/empty paste is non-fatal and falls back to the
				// in-container login.
				HostAcquire: claudeAPIKeyHostAcquire,
			}},
			StaticFiles: []launch.StaticFile{{
				ContainerPath: claudeOnboardingContainerPath,
				Mode:          0o644,
				Content:       claudeOnboardingStub,
			}},
		}
	}
	return launch.AuthSpec{
		FileBindings: []launch.FileBinding{{
			VaultPath:     claudeVaultPath,
			ContainerPath: claudeCredentialsContainerPath,
			Mode:          0o600,
			// Required=false so an empty vault triggers the in-
			// container-login bootstrap path documented in ADR-0025
			// rather than failing the launch.
			Required: false,
			Render:   claudeRender,
			Capture:  claudeCapture,
			Fresher:  claudeFresher,
			// HostAcquire runs a host-side OAuth flow (hosted-callback
			// paste with PKCE, then a profile fetch that stamps the
			// subscription tier) when the vault is empty and host-login is
			// enabled, so the user authenticates in their own
			// browser/terminal rather than inside the container TTY. The
			// launcher owns the vault PUT and the validating Render; a
			// cancel/failure is non-fatal and falls back to the
			// in-container login.
			HostAcquire: claudeHostAcquire,
		}},
		StaticFiles: []launch.StaticFile{{
			ContainerPath: claudeOnboardingContainerPath,
			Mode:          0o644,
			Content:       claudeOnboardingStub,
		}},
	}
}
