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

func TestClaude_Args_AllowsBashAndAileronMCP(t *testing.T) {
	args := agents.Claude{}.Args()
	if len(args) != 2 {
		t.Fatalf("Args() = %v, want 2 entries", args)
	}
	if args[0] != "--allowedTools" {
		t.Errorf("Args()[0] = %q, want --allowedTools", args[0])
	}
	if !strings.Contains(args[1], "Bash(*)") {
		t.Errorf("Args()[1] = %q, should allow Bash(*)", args[1])
	}
	if !strings.Contains(args[1], "mcp__"+launch.MCPServerName) {
		t.Errorf("Args()[1] = %q, should allow mcp__%s", args[1], launch.MCPServerName)
	}
}

func TestClaude_ConfigureMCP_EmitsMCPConfigFlag(t *testing.T) {
	mcpEnv := map[string]string{
		"AILERON_URL":        "http://127.0.0.1:7000",
		"AILERON_SESSION_ID": "sess-abc",
	}
	args, err := agents.Claude{}.ConfigureMCP("/usr/local/bin/aileron-mcp", mcpEnv, "")
	if err != nil {
		t.Fatalf("ConfigureMCP returned error: %v", err)
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
