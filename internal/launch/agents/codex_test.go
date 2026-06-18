package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
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

	args, mounts, err := agents.Codex{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{
		"AILERON_URL":        "http://127.0.0.1:7000",
		"AILERON_SESSION_ID": "sess-codex",
	}, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP returned error: %v", err)
	}
	if args != nil {
		t.Errorf("Args = %v, want nil for Codex (config-file integration)", args)
	}
	if len(mounts) != 0 {
		t.Errorf("Mounts = %v, want 0 under ModeHost", mounts)
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

	_, _, err := agents.Codex{}.ConfigureMCP("/new/path/aileron-mcp", map[string]string{
		"AILERON_URL": "http://127.0.0.1:7000",
	}, "", launch.ModeHost)
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

// --- Sandbox mode (U3 / #953) ---

// TestCodex_ConfigureMCP_SandboxMode_WritesTempAndReturnsMount pins
// the contract: under ModeSandbox the host ~/.codex/config.toml is
// never created; instead a temp config.toml is written and a Volume
// mount is returned targeting /home/agent/.codex/config.toml.
//
// The returned mount target is intentionally nested under the AuthSpec's
// /home/agent/.codex/ directory mount: the launcher
// (collapseNestedMCPMounts) relocates the file into that directory's
// host-side source instead of emitting a separate nested bind mount, so
// no file-inside-dir bind reaches runc. See issue #1143 and the launcher
// regression test TestCollapseNestedMCPMounts_CollapsesFileIntoParentDir.
func TestCodex_ConfigureMCP_SandboxMode_WritesTempAndReturnsMount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	args, mounts, err := agents.Codex{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{
		"AILERON_URL":        "http://host.docker.internal:7000",
		"AILERON_SESSION_ID": "sess-codex-sandbox",
	}, "", launch.ModeSandbox)
	if err != nil {
		t.Fatalf("ConfigureMCP sandbox: %v", err)
	}
	if args != nil {
		t.Errorf("Args = %v, want nil for Codex sandbox (config-file integration)", args)
	}
	if len(mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1", len(mounts))
	}
	mount := mounts[0]
	if mount.Target != "/home/agent/.codex/config.toml" {
		t.Errorf("mount.Target = %q, want /home/agent/.codex/config.toml", mount.Target)
	}
	if !mount.ReadOnly {
		t.Errorf("mount.ReadOnly = false, want true under sandbox mode")
	}
	if _, err := os.Stat(mount.Source); err != nil {
		t.Fatalf("temp config.toml not written at mount.Source: %v", err)
	}
	body, err := os.ReadFile(mount.Source)
	if err != nil {
		t.Fatalf("read temp config.toml: %v", err)
	}
	content := string(body)
	for _, want := range []string{
		"[mcp_servers.aileron]",
		`command = "/usr/local/bin/aileron-mcp"`,
		`AILERON_URL = "http://host.docker.internal:7000"`,
		`AILERON_SESSION_ID = "sess-codex-sandbox"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("temp config.toml missing %q:\n%s", want, content)
		}
	}
}

// TestCodex_ConfigureMCP_SandboxMode_NonInteractiveKeys pins the
// non-interactive contract for an ephemeral sandbox run (feedback #1162):
// the generated config.toml must suppress Codex's per-tool approval
// prompts, defer isolation to the outer container, and pre-accept the
// folder-trust prompt for the bind-mounted workspace. Without these the
// operator hits an approval prompt on each MCP tool call
// (list_recent_emails / get_email) and a "trust the current folder?"
// dialog at launch. The sandbox container is the trust boundary
// (ADR-0015), so auto-approving inside it is safe.
func TestCodex_ConfigureMCP_SandboxMode_NonInteractiveKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, mounts, err := agents.Codex{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{
		"AILERON_URL": "http://host.docker.internal:7000",
	}, "", launch.ModeSandbox)
	if err != nil {
		t.Fatalf("ConfigureMCP sandbox: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1", len(mounts))
	}
	body, err := os.ReadFile(mounts[0].Source)
	if err != nil {
		t.Fatalf("read temp config.toml: %v", err)
	}
	content := string(body)
	for _, want := range []string{
		`approval_policy = "never"`,
		`sandbox_mode = "danger-full-access"`,
		`[projects."/home/agent/workspace"]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("sandbox config.toml missing non-interactive key %q:\n%s", want, content)
		}
	}
}

// TestCodex_ConfigureMCP_SandboxMode_DoesNotTouchHostConfig is the
// load-bearing safety guarantee: a sandbox launch must not mutate the
// host ~/.codex/config.toml. A user running both host-mode and
// sandbox-mode Codex against the same home must see a stable host
// config file regardless of any number of sandbox launches.
func TestCodex_ConfigureMCP_SandboxMode_DoesNotTouchHostConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	configDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	hostPath := filepath.Join(configDir, "config.toml")
	original := []byte("# user config\napproval_policy = \"on-request\"\n\n[mcp_servers.userserver]\ncommand = \"/usr/local/bin/userthing-mcp\"\n")
	if err := os.WriteFile(hostPath, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hostStat, _ := os.Stat(hostPath)
	hostMtime := hostStat.ModTime()

	_, _, err := agents.Codex{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{
		"AILERON_URL": "http://host.docker.internal:7000",
	}, "", launch.ModeSandbox)
	if err != nil {
		t.Fatalf("ConfigureMCP sandbox: %v", err)
	}

	gotStat, err := os.Stat(hostPath)
	if err != nil {
		t.Fatalf("host config disappeared: %v", err)
	}
	if !gotStat.ModTime().Equal(hostMtime) {
		t.Errorf("sandbox mode mutated host config (mtime changed)")
	}
	got, _ := os.ReadFile(hostPath)
	if string(got) != string(original) {
		t.Errorf("sandbox mode mutated host config content:\nwant: %q\ngot:  %q", original, got)
	}
}

// TestCodex_ConfigureMCP_SandboxMode_FreshFileNoUserEntries documents
// the design tradeoff: sandbox mode emits ONLY the [mcp_servers.aileron]
// block. Any user-defined [mcp_servers.foo] entries on the host don't
// leak in. The manual recipe (R8) covers the workaround.
func TestCodex_ConfigureMCP_SandboxMode_FreshFileNoUserEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	configDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	hostPath := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(hostPath, []byte("[mcp_servers.userserver]\ncommand = \"/usr/local/bin/userthing-mcp\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, mounts, err := agents.Codex{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{}, "", launch.ModeSandbox)
	if err != nil {
		t.Fatalf("ConfigureMCP sandbox: %v", err)
	}
	body, _ := os.ReadFile(mounts[0].Source)
	if strings.Contains(string(body), "userserver") {
		t.Errorf("sandbox temp config inherited user-server entry; isolation broken:\n%s", body)
	}
}
