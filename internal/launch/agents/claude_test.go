package agents_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func TestClaude_Identity(t *testing.T) {
	c := agents.Claude{}
	if c.Name() != "claude" {
		t.Errorf("Name() = %q, want %q", c.Name(), "claude")
	}
	if got := c.BinaryNames(); len(got) != 1 || got[0] != "claude" {
		t.Errorf("BinaryNames() = %v, want [\"claude\"]", got)
	}
	if c.LLMEndpointEnv() != "ANTHROPIC_BASE_URL" {
		t.Errorf("LLMEndpointEnv() = %q, want %q", c.LLMEndpointEnv(), "ANTHROPIC_BASE_URL")
	}
	if c.Env() != nil {
		t.Errorf("Env() = %v, want nil", c.Env())
	}
}

// TestClaude_Args_HostAllowsBashAndAileronMCP pins the host-launch
// posture: a host launch runs against the user's real machine, so
// Claude keeps the narrower --allowedTools whitelist rather than full
// permission skipping.
func TestClaude_Args_HostAllowsBashAndAileronMCP(t *testing.T) {
	args := agents.Claude{}.Args(launch.ModeHost)
	if len(args) != 2 {
		t.Fatalf("Args(ModeHost) = %v, want 2 entries", args)
	}
	if args[0] != "--allowedTools" {
		t.Errorf("Args(ModeHost)[0] = %q, want --allowedTools", args[0])
	}
	if !strings.Contains(args[1], "Bash(*)") {
		t.Errorf("Args(ModeHost)[1] = %q, should allow Bash(*)", args[1])
	}
	if !strings.Contains(args[1], "mcp__"+launch.MCPServerName) {
		t.Errorf("Args(ModeHost)[1] = %q, should allow mcp__%s", args[1], launch.MCPServerName)
	}
}

// TestClaude_Args_SandboxSkipsPermissions is the regression test for
// the container/sandbox parity fix: under ModeSandbox the container is
// the trust boundary (ADR-0015), so Claude must pass
// --dangerously-skip-permissions alone, mirroring Codex's container
// YOLO posture. The narrower --allowedTools flag must NOT appear: the
// skip flag subsumes it, and combining them would be redundant.
func TestClaude_Args_SandboxSkipsPermissions(t *testing.T) {
	args := agents.Claude{}.Args(launch.ModeSandbox)
	if len(args) != 1 || args[0] != "--dangerously-skip-permissions" {
		t.Fatalf("Args(ModeSandbox) = %v, want [--dangerously-skip-permissions]", args)
	}
	for _, a := range args {
		if a == "--allowedTools" {
			t.Errorf("Args(ModeSandbox) = %v, must not include --allowedTools alongside the skip flag", args)
		}
	}
}

func TestClaude_ConfigureMCP_EmitsMCPConfigFlag(t *testing.T) {
	mcpEnv := map[string]string{
		"AILERON_URL":        "http://127.0.0.1:7000",
		"AILERON_SESSION_ID": "sess-abc",
	}
	args, mounts, err := agents.Claude{}.ConfigureMCP("/usr/local/bin/aileron-mcp", mcpEnv, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP returned error: %v", err)
	}
	if len(mounts) != 0 {
		t.Errorf("ConfigureMCP returned %d mounts under ModeHost; want 0", len(mounts))
	}
	if len(args) != 2 || args[0] != "--mcp-config" {
		t.Fatalf("ConfigureMCP() = %v, want [--mcp-config <json>]", args)
	}
	var payload struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[1]), &payload); err != nil {
		t.Fatalf("ConfigureMCP returned non-JSON value: %v\n%s", err, args[1])
	}
	server, ok := payload.MCPServers[launch.MCPServerName]
	if !ok {
		t.Fatalf("MCP config missing entry %q: %+v", launch.MCPServerName, payload)
	}
	if server.Command != "/usr/local/bin/aileron-mcp" {
		t.Errorf("server.Command = %q, want /usr/local/bin/aileron-mcp", server.Command)
	}
	if server.Env["AILERON_URL"] != "http://127.0.0.1:7000" {
		t.Errorf("server.Env[AILERON_URL] = %q, want http://127.0.0.1:7000", server.Env["AILERON_URL"])
	}
	if server.Env["AILERON_SESSION_ID"] != "sess-abc" {
		t.Errorf("server.Env[AILERON_SESSION_ID] = %q, want sess-abc", server.Env["AILERON_SESSION_ID"])
	}
}
