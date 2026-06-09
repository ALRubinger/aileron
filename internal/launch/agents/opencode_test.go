package agents_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func TestOpenCode_Identity(t *testing.T) {
	o := agents.OpenCode{}
	if o.Name() != "opencode" {
		t.Errorf("Name() = %q, want opencode", o.Name())
	}
	if got := o.BinaryNames(); len(got) != 1 || got[0] != "opencode" {
		t.Errorf("BinaryNames() = %v, want [opencode]", got)
	}
	if o.LLMEndpointEnv() != "" {
		t.Errorf("LLMEndpointEnv() = %q, want empty (provider URL per-provider)", o.LLMEndpointEnv())
	}
	if o.Args() != nil {
		t.Errorf("Args() = %v, want nil", o.Args())
	}
	if o.Env() != nil {
		t.Errorf("Env() = %v, want nil", o.Env())
	}
}

func TestOpenCode_ConfigureMCP_WritesProjectJSON(t *testing.T) {
	dir := t.TempDir()
	args, _, err := agents.OpenCode{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{
		"AILERON_URL":        "http://127.0.0.1:7000",
		"AILERON_SESSION_ID": "sess-opencode",
	}, dir, launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}
	if args != nil {
		t.Errorf("Args = %v, want nil for OpenCode (config-file integration)", args)
	}

	data, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatalf("opencode.json not written: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v\n%s", err, data)
	}
	mcp, _ := root["mcp"].(map[string]any)
	if mcp == nil {
		t.Fatalf("mcp key missing: %+v", root)
	}
	entry, _ := mcp["aileron"].(map[string]any)
	if entry == nil {
		t.Fatalf("mcp.aileron missing: %+v", mcp)
	}
	cmd, _ := entry["command"].([]any)
	if len(cmd) == 0 || cmd[0] != "/usr/local/bin/aileron-mcp" {
		t.Errorf("mcp.aileron.command = %v, want [\"/usr/local/bin/aileron-mcp\"]", entry["command"])
	}
	if entry["type"] != "local" {
		t.Errorf("mcp.aileron.type = %v, want local", entry["type"])
	}
	if entry["enabled"] != true {
		t.Errorf("mcp.aileron.enabled = %v, want true", entry["enabled"])
	}
	envMap, _ := entry["environment"].(map[string]any)
	if envMap["AILERON_URL"] != "http://127.0.0.1:7000" {
		t.Errorf("mcp.aileron.environment[AILERON_URL] = %v", envMap["AILERON_URL"])
	}
}

func TestOpenCode_ConfigureMCP_PreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	seed := map[string]any{
		"model": "claude-sonnet-4-5",
		"permission": map[string]any{
			"bash": "ask",
		},
		"mcp": map[string]any{
			"otherserver": map[string]any{
				"type":    "local",
				"command": []any{"/usr/local/bin/other-mcp"},
			},
		},
	}
	seedJSON, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), seedJSON, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, _, err := agents.OpenCode{}.ConfigureMCP("/new/aileron-mcp", map[string]string{}, dir, launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "opencode.json"))
	var root map[string]any
	_ = json.Unmarshal(data, &root)
	if root["model"] != "claude-sonnet-4-5" {
		t.Error("top-level model key not preserved")
	}
	perm, _ := root["permission"].(map[string]any)
	if perm["bash"] != "ask" {
		t.Error("permission.bash not preserved (Aileron does not own OpenCode's permission system)")
	}
	mcp, _ := root["mcp"].(map[string]any)
	if mcp["otherserver"] == nil {
		t.Error("mcp.otherserver not preserved")
	}
	if mcp["aileron"] == nil {
		t.Error("mcp.aileron not added")
	}
}
