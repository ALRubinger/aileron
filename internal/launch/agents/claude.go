package agents

import (
	"encoding/json"
	"fmt"

	"github.com/ALRubinger/aileron/internal/launch"
)

// Claude is the agent definition for Claude Code.
type Claude struct{}

func (c Claude) Name() string          { return "claude" }
func (c Claude) BinaryNames() []string { return []string{"claude"} }

// Args tells Claude Code to auto-approve:
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
func (c Claude) Args() []string {
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
func (c Claude) AuthSpec() launch.AuthSpec {
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
		}},
		StaticFiles: []launch.StaticFile{{
			ContainerPath: claudeOnboardingContainerPath,
			Mode:          0o644,
			Content:       claudeOnboardingStub,
		}},
	}
}
