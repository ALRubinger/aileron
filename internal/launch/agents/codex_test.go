package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func TestCodex_Identity(t *testing.T) {
	c := agents.Codex{}
	if c.Name() != "codex" {
		t.Errorf("Name() = %q, want %q", c.Name(), "codex")
	}
	if got := c.BinaryNames(); len(got) != 1 || got[0] != "codex" {
		t.Errorf("BinaryNames() = %v, want [\"codex\"]", got)
	}
	if c.LLMEndpointEnv() != "OPENAI_BASE_URL" {
		t.Errorf("LLMEndpointEnv() = %q, want OPENAI_BASE_URL", c.LLMEndpointEnv())
	}
	if c.Args() != nil {
		t.Errorf("Args() = %v, want nil", c.Args())
	}
	if c.Env() != nil {
		t.Errorf("Env() = %v, want nil", c.Env())
	}
}

func TestCodex_ConfigureMCP_WritesConfigTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	args, err := agents.Codex{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{
		"AILERON_URL":        "http://127.0.0.1:7000",
		"AILERON_SESSION_ID": "sess-codex",
	}, "")
	if err != nil {
		t.Fatalf("ConfigureMCP returned error: %v", err)
	}
	if args != nil {
		t.Errorf("Args = %v, want nil for Codex (config-file integration)", args)
	}

	configPath := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config.toml not written: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "[mcp_servers.aileron]") {
		t.Errorf("config.toml missing [mcp_servers.aileron] header:\n%s", content)
	}
	if !strings.Contains(content, `command = "/usr/local/bin/aileron-mcp"`) {
		t.Errorf("config.toml missing command line:\n%s", content)
	}
	if !strings.Contains(content, "[mcp_servers.aileron.env]") {
		t.Errorf("config.toml missing env table:\n%s", content)
	}
	if !strings.Contains(content, `AILERON_URL = "http://127.0.0.1:7000"`) {
		t.Errorf("config.toml missing AILERON_URL env line:\n%s", content)
	}
	if !strings.Contains(content, `AILERON_SESSION_ID = "sess-codex"`) {
		t.Errorf("config.toml missing AILERON_SESSION_ID env line:\n%s", content)
	}
}

func TestCodex_ConfigureMCP_PreservesOtherSections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	configDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := `# user comment
approval_policy = "on-request"

[model_provider]
name = "openai"

[mcp_servers.aileron]
command = "/old/path/aileron-mcp"

[mcp_servers.aileron.env]
STALE = "yes"

[mcp_servers.other]
command = "/usr/local/bin/other-mcp"
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(existing), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	_, err := agents.Codex{}.ConfigureMCP("/new/path/aileron-mcp", map[string]string{
		"AILERON_URL": "http://127.0.0.1:7000",
	}, "")
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, `approval_policy = "on-request"`) {
		t.Error("approval_policy line not preserved")
	}
	if !strings.Contains(content, "[model_provider]") {
		t.Error("[model_provider] section not preserved")
	}
	if !strings.Contains(content, "[mcp_servers.other]") {
		t.Error("[mcp_servers.other] section not preserved")
	}
	if strings.Contains(content, "/old/path/aileron-mcp") {
		t.Error("old aileron-mcp command not replaced")
	}
	if !strings.Contains(content, "/new/path/aileron-mcp") {
		t.Error("new aileron-mcp command not written")
	}
	if strings.Contains(content, `STALE = "yes"`) {
		t.Error("stale env entry not removed")
	}
}
