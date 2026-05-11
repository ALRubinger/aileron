package agents_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func TestPi_Identity(t *testing.T) {
	p := agents.Pi{}
	if p.Name() != "pi" {
		t.Errorf("Name() = %q, want %q", p.Name(), "pi")
	}
	if got := p.BinaryNames(); len(got) != 1 || got[0] != "pi" {
		t.Errorf("BinaryNames() = %v, want [\"pi\"]", got)
	}
	if p.LLMEndpointEnv() != "" {
		t.Errorf("LLMEndpointEnv() = %q, want empty", p.LLMEndpointEnv())
	}
	if p.Env() != nil {
		t.Errorf("Env() = %v, want nil", p.Env())
	}
}

func TestPi_Args_AutoApproveTools(t *testing.T) {
	args := agents.Pi{}.Args()
	if len(args) != 2 || args[0] != "--tools" {
		t.Fatalf("Args() = %v, want [--tools <list>]", args)
	}
	for _, tool := range []string{"bash", "read", "edit", "write", "grep", "find", "ls"} {
		if !strings.Contains(args[1], tool) {
			t.Errorf("Args()[1] = %q, missing tool %q", args[1], tool)
		}
	}
}

func TestPi_ConfigureMCP_EmitsMCPConfigFlag(t *testing.T) {
	mcpEnv := map[string]string{
		"AILERON_URL": "http://127.0.0.1:7000",
	}
	args, err := agents.Pi{}.ConfigureMCP("/usr/local/bin/aileron-mcp", mcpEnv, "")
	if err != nil {
		t.Fatalf("ConfigureMCP returned error: %v", err)
	}
	if len(args) != 2 || args[0] != "--mcp-config" {
		t.Fatalf("ConfigureMCP() = %v, want [--mcp-config <json>]", args)
	}
	var payload struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[1]), &payload); err != nil {
		t.Fatalf("ConfigureMCP returned non-JSON value: %v", err)
	}
	if payload.MCPServers[launch.MCPServerName].Command != "/usr/local/bin/aileron-mcp" {
		t.Errorf("expected aileron-mcp command in payload, got %+v", payload)
	}
}
