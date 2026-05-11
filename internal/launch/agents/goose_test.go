package agents_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func TestGoose_Identity(t *testing.T) {
	g := agents.Goose{}
	if g.Name() != "goose" {
		t.Errorf("Name() = %q, want %q", g.Name(), "goose")
	}
	if got := g.BinaryNames(); len(got) != 1 || got[0] != "goose" {
		t.Errorf("BinaryNames() = %v, want [\"goose\"]", got)
	}
	if g.LLMEndpointEnv() != "" {
		t.Errorf("LLMEndpointEnv() = %q, want empty (Goose configures provider URL per-provider in config.yaml)", g.LLMEndpointEnv())
	}
}

func TestGoose_Env_ForcesAutonomousMode(t *testing.T) {
	env := agents.Goose{}.Env()
	if env["GOOSE_MODE"] != "auto" {
		t.Errorf("Env()[GOOSE_MODE] = %q, want \"auto\"", env["GOOSE_MODE"])
	}
}

func TestGoose_ConfigureMCP_WritesConfigYAML(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test; Windows path resolution covered separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	args, err := agents.Goose{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{
		"AILERON_URL":        "http://127.0.0.1:7000",
		"AILERON_SESSION_ID": "sess-goose",
	}, "")
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}
	if args != nil {
		t.Errorf("Args = %v, want nil for Goose (config-file integration)", args)
	}

	configPath := filepath.Join(home, ".config", "goose", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("config.yaml not valid YAML: %v\n%s", err, data)
	}
	extensions, _ := root["extensions"].(map[string]any)
	if extensions == nil {
		t.Fatalf("extensions key missing: %+v", root)
	}
	entry, _ := extensions["aileron"].(map[string]any)
	if entry == nil {
		t.Fatalf("extensions.aileron missing: %+v", extensions)
	}
	if entry["cmd"] != "/usr/local/bin/aileron-mcp" {
		t.Errorf("extensions.aileron.cmd = %v, want /usr/local/bin/aileron-mcp", entry["cmd"])
	}
	if entry["type"] != "stdio" {
		t.Errorf("extensions.aileron.type = %v, want \"stdio\"", entry["type"])
	}
	envs, _ := entry["envs"].(map[string]any)
	if envs == nil {
		t.Fatalf("extensions.aileron.envs missing: %+v", entry)
	}
	if envs["AILERON_URL"] != "http://127.0.0.1:7000" {
		t.Errorf("envs.AILERON_URL = %v, want http://127.0.0.1:7000", envs["AILERON_URL"])
	}
}

func TestGoose_ConfigureMCP_PreservesOtherExtensions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test; Windows path resolution covered separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	configDir := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seed := map[string]any{
		"GOOSE_PROVIDER": "openai",
		"extensions": map[string]any{
			"developer": map[string]any{"enabled": true, "type": "builtin"},
			"aileron":   map[string]any{"cmd": "/old/path"},
		},
	}
	seedYAML, _ := yaml.Marshal(seed)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), seedYAML, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := agents.Goose{}.ConfigureMCP("/new/path", map[string]string{}, "")
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	var root map[string]any
	_ = yaml.Unmarshal(data, &root)
	if root["GOOSE_PROVIDER"] != "openai" {
		t.Error("top-level GOOSE_PROVIDER not preserved")
	}
	exts, _ := root["extensions"].(map[string]any)
	if exts["developer"] == nil {
		t.Error("developer extension not preserved")
	}
	aileronEntry, _ := exts["aileron"].(map[string]any)
	if aileronEntry["cmd"] != "/new/path" {
		t.Errorf("aileron extension not overwritten: %v", aileronEntry)
	}
}
