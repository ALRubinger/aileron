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
func (c Claude) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string) ([]string, error) {
	envJSON, err := json.Marshal(mcpEnv)
	if err != nil {
		return nil, fmt.Errorf("marshaling MCP env: %w", err)
	}
	mcpConfig := fmt.Sprintf(
		`{"mcpServers":{%q:{"command":%q,"env":%s}}}`,
		launch.MCPServerName, mcpBin, string(envJSON),
	)
	return []string{"--mcp-config", mcpConfig}, nil
}
