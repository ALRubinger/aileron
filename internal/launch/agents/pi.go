package agents

import (
	"encoding/json"
	"fmt"

	"github.com/ALRubinger/aileron/internal/launch"
)

// Pi is the agent definition for the pi.dev coding agent.
type Pi struct{}

func (p Pi) Name() string          { return "pi" }
func (p Pi) BinaryNames() []string { return []string{"pi"} }

// Args returns the flags needed to make Pi auto-approve its built-in
// tools. Per ADR-0015, Aileron is not the trust surface for the agent's
// local shell commands; --tools just suppresses Pi's per-tool prompt
// surface so the user is not double-prompted.
func (p Pi) Args() []string {
	return []string{"--tools", "bash,read,edit,write,grep,find,ls"}
}

func (p Pi) Env() map[string]string { return nil }

// LLMEndpointEnv returns "" — Pi resolves its API endpoint via its
// settings file rather than an environment variable, so launch does
// not route Pi's LLM calls through the embedded gateway. Gateway
// routing for Pi would require ConfigureMCP to also write the
// gateway URL into .pi/settings.json.
func (p Pi) LLMEndpointEnv() string { return "" }

// ConfigureMCP returns the CLI flags that register aileron-mcp with
// Pi. Pi accepts Claude-Code-compatible --mcp-config flag handling, so
// the wire shape matches Claude's. Mode is irrelevant; --mcp-config
// travels with the exec command in both host and sandbox modes.
func (p Pi) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string, _ launch.Mode) ([]string, []launch.MCPMount, error) {
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

// AuthSpec returns Pi's vault-backed credential descriptor. Pi keeps
// its provider credentials in a dedicated `auth.json` under
// `~/.pi/agent/`, written by `/login` and loaded on startup. That is a
// rotatable on-disk credential file (separate from settings.json), so
// the binding is a FileBinding with byte-identity Render/Capture.
// Required is false so an unseeded vault falls through to Pi's
// in-container `/login` bootstrap, which Capture then snapshots on
// clean exit. MountAsFile is false (the default): Pi's ConfigureMCP
// passes MCP config via a CLI flag, not a file mount, so no sibling
// mount needs to coexist with auth.json.
func (p Pi) AuthSpec() launch.AuthSpec {
	return launch.AuthSpec{
		FileBindings: []launch.FileBinding{{
			VaultPath:     piVaultPath,
			ContainerPath: piAuthContainerPath,
			Mode:          0o600,
			Required:      false,
			Render:        piRender,
			Capture:       piCapture,
		}},
	}
}
