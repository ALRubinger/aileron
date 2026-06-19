package agents_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/launch"
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

// TestGoose_Args_StartsSessionSubcommand pins the contract that
// Aileron invokes `goose session …` rather than `goose` with no
// subcommand. `--with-extension` is a session-subcommand flag in
// Goose's clap config; without an explicit subcommand the flag
// isn't recognized and our extension wiring silently disappears.
func TestGoose_Args_StartsSessionSubcommand(t *testing.T) {
	args := agents.Goose{}.Args(launch.ModeHost)
	if len(args) != 1 || args[0] != "session" {
		t.Errorf("Args() = %v, want [\"session\"]", args)
	}
}

// TestGoose_ConfigureMCP_ReturnsWithExtensionFlag verifies the
// returned args are the `--with-extension <ENV=val ENV=val cmd>`
// pair Goose's parse_stdio_extension consumes. We assert on the
// shape rather than the exact string so future env-var additions
// don't churn this test.
func TestGoose_ConfigureMCP_ReturnsWithExtensionFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test; Windows path resolution covered separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	args, _, err := agents.Goose{}.ConfigureMCP("/usr/local/bin/aileron-mcp", map[string]string{
		"AILERON_URL":        "http://127.0.0.1:7000",
		"AILERON_SESSION_ID": "sess-goose",
	}, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}
	if len(args) != 2 || args[0] != "--with-extension" {
		t.Fatalf("Args = %v, want [\"--with-extension\", <value>]", args)
	}
	value := args[1]
	if !strings.HasSuffix(value, "/usr/local/bin/aileron-mcp") {
		t.Errorf("--with-extension value = %q, want suffix \"/usr/local/bin/aileron-mcp\"", value)
	}
	if !strings.Contains(value, "AILERON_URL=http://127.0.0.1:7000") {
		t.Errorf("--with-extension value = %q, missing AILERON_URL", value)
	}
	if !strings.Contains(value, "AILERON_SESSION_ID=sess-goose") {
		t.Errorf("--with-extension value = %q, missing AILERON_SESSION_ID", value)
	}
}

// TestGoose_ConfigureMCP_EnvVarOrderIsDeterministic prevents
// flakiness from Go's randomized map iteration. The encoded value
// must be stable across calls so test assertions and any cache
// keys downstream don't churn.
func TestGoose_ConfigureMCP_EnvVarOrderIsDeterministic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	envs := map[string]string{
		"AILERON_URL":          "http://127.0.0.1:7000",
		"AILERON_COMMS_URL":    "http://127.0.0.1:7000",
		"AILERON_SESSION_ID":   "sess",
		"AILERON_APPROVAL_URL": "http://127.0.0.1:7000/approvals",
	}
	first, _, err := agents.Goose{}.ConfigureMCP("/bin/aileron-mcp", envs, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP first: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, _, err := agents.Goose{}.ConfigureMCP("/bin/aileron-mcp", envs, "", launch.ModeHost)
		if err != nil {
			t.Fatalf("ConfigureMCP repeat: %v", err)
		}
		if next[1] != first[1] {
			t.Fatalf("--with-extension value not deterministic across calls\nfirst = %q\nrepeat = %q", first[1], next[1])
		}
	}
}

// TestGoose_ConfigureMCP_RemovesStaleAileronEntry covers the
// cleanup of any pre-existing `extensions.aileron` block left by
// older Aileron versions that wrote to config.yaml. The other
// extensions and top-level keys must survive untouched — the user
// (and Goose itself) may rely on them.
func TestGoose_ConfigureMCP_RemovesStaleAileronEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
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
			"aileron":   map[string]any{"cmd": "/old/path", "type": "stdio"},
		},
	}
	seedYAML, _ := yaml.Marshal(seed)
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, seedYAML, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := (agents.Goose{}).ConfigureMCP("/new/path", map[string]string{}, "", launch.ModeHost); err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if root["GOOSE_PROVIDER"] != "openai" {
		t.Error("top-level GOOSE_PROVIDER not preserved")
	}
	exts, _ := root["extensions"].(map[string]any)
	if exts == nil {
		t.Fatal("extensions block dropped")
	}
	if exts["developer"] == nil {
		t.Error("developer extension not preserved")
	}
	if _, present := exts["aileron"]; present {
		t.Errorf("stale aileron entry should have been removed; got: %v", exts["aileron"])
	}
}

// TestGoose_ConfigureMCP_NoConfigYAMLIsFine covers a fresh install
// where the user has never run Goose. ConfigureMCP must succeed
// without trying to create or modify any file in that case — the
// `--with-extension` flag carries everything Goose needs.
func TestGoose_ConfigureMCP_NoConfigYAMLIsFine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	if _, _, err := (agents.Goose{}).ConfigureMCP("/bin/aileron-mcp", map[string]string{}, "", launch.ModeHost); err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}
	configPath := filepath.Join(home, ".config", "goose", "config.yaml")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("ConfigureMCP should not create config.yaml when none exists; stat err = %v", err)
	}
}

// TestGoose_ConfigureMCP_ShellQuotesValuesWithSpaces guards the
// encoder against the case where a path or env value contains
// whitespace — without quoting, Goose's split_quoted would slice
// it in half and either point at a nonexistent binary or drop
// half the value.
func TestGoose_ConfigureMCP_ShellQuotesValuesWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	args, _, err := agents.Goose{}.ConfigureMCP("/Users/me/Application Support/aileron-mcp", map[string]string{
		"WITH SPACE": "a b",
	}, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}
	value := args[1]
	if !strings.Contains(value, `"a b"`) {
		t.Errorf("env value with space not quoted; value = %q", value)
	}
	if !strings.Contains(value, `"/Users/me/Application Support/aileron-mcp"`) {
		t.Errorf("cmd path with space not quoted; value = %q", value)
	}
}

// TestGoose_ConfigureMCP_QuotesEmptyEnvValues prevents an empty
// env value from being emitted as bare `KEY=` followed by a
// space — Goose's split_quoted would then consume the next
// token as the value. `KEY=""` is the unambiguous form.
func TestGoose_ConfigureMCP_QuotesEmptyEnvValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	args, _, err := agents.Goose{}.ConfigureMCP("/bin/aileron-mcp", map[string]string{
		"EMPTY":  "",
		"NORMAL": "value",
	}, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}
	value := args[1]
	if !strings.Contains(value, `EMPTY=""`) {
		t.Errorf("empty env value not quoted as \"\"; value = %q", value)
	}
	if !strings.Contains(value, "NORMAL=value") {
		t.Errorf("normal env value mangled; value = %q", value)
	}
}

// TestGoose_ConfigureMCP_LeavesConfigUntouched_NoExtensionsKey
// covers a config.yaml that exists but has no `extensions:`
// block at all (older Goose installations, or users who never
// added any). The cleanup must be a no-op — don't fabricate an
// empty extensions key just to write one back.
func TestGoose_ConfigureMCP_LeavesConfigUntouched_NoExtensionsKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	configDir := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	original := []byte("GOOSE_PROVIDER: openai\nGOOSE_MODEL: gpt-4o\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := (agents.Goose{}).ConfigureMCP("/bin/aileron-mcp", map[string]string{}, "", launch.ModeHost); err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("config.yaml mutated when no extensions key was present:\nwant: %q\ngot:  %q", original, got)
	}
}

// TestGoose_ConfigureMCP_TolratesMalformedConfigYAML pins the
// design decision that a pre-existing malformed config.yaml
// must NOT fail the launch. The user's config is theirs to
// manage; Goose itself will surface the parse error. Aileron's
// MCP wiring still has to go through.
func TestGoose_ConfigureMCP_ToleratesMalformedConfigYAML(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	configDir := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// `:`-prefixed value at top level isn't valid YAML.
	bad := []byte("extensions:\n  aileron:\n    cmd: :::not yaml\n  : whoops\n")
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), bad, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	args, _, err := (agents.Goose{}).ConfigureMCP("/bin/aileron-mcp", map[string]string{"AILERON_URL": "http://x"}, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP should tolerate malformed user config, got: %v", err)
	}
	if len(args) != 2 || args[0] != "--with-extension" {
		t.Errorf("Args = %v, want [\"--with-extension\", <value>]", args)
	}
}

// TestGoose_ConfigureMCP_EscapesQuotesAndBackslashes covers
// the inner switch in the quoter — values containing a literal
// double-quote or backslash need to be escaped, not just
// wrapped. An unescaped `"` would terminate the quoted string
// early and Goose's split_quoted would consume the rest as a
// separate token (silently losing data); an unescaped `\` could
// be interpreted as an escape introducer.
func TestGoose_ConfigureMCP_EscapesQuotesAndBackslashes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	args, _, err := agents.Goose{}.ConfigureMCP("/bin/aileron-mcp", map[string]string{
		"QUOTE": `a"b`,
		"SLASH": `a\b`,
	}, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}
	value := args[1]
	if !strings.Contains(value, `QUOTE="a\"b"`) {
		t.Errorf("double-quote not escaped; value = %q", value)
	}
	if !strings.Contains(value, `SLASH="a\\b"`) {
		t.Errorf("backslash not escaped; value = %q", value)
	}
}

// TestGoose_ConfigureMCP_PreservesOtherExtensionsWhenNoAileronEntry
// covers the cleanup's early-return when an existing config has
// other extensions but no `aileron` key. The file must not be
// rewritten — that would churn formatting/comments unnecessarily
// for every launch.
func TestGoose_ConfigureMCP_PreservesOtherExtensionsWhenNoAileronEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	configDir := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	original := []byte("extensions:\n  developer:\n    enabled: true\n    type: builtin\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	beforeMtime, _ := os.Stat(configPath)

	if _, _, err := (agents.Goose{}).ConfigureMCP("/bin/aileron-mcp", map[string]string{}, "", launch.ModeHost); err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}

	afterMtime, _ := os.Stat(configPath)
	if !afterMtime.ModTime().Equal(beforeMtime.ModTime()) {
		// File was rewritten despite having nothing to clean — a
		// no-op cleanup should not touch the file.
		got, _ := os.ReadFile(configPath)
		t.Errorf("config.yaml rewritten when no aileron entry existed to clean\nbefore: %q\nafter:  %q", original, got)
	}
}

// TestGoose_ConfigureMCP_HonorsXDGConfigHome covers the
// non-default XDG path. Users with a custom XDG_CONFIG_HOME
// (common on Linux setups) need the cleanup to target their
// actual config directory, not the implicit ~/.config fallback.
func TestGoose_ConfigureMCP_HonorsXDGConfigHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG-based path test")
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	configDir := filepath.Join(xdg, "goose")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seed := map[string]any{
		"extensions": map[string]any{
			"aileron": map[string]any{"cmd": "/old/path", "type": "stdio"},
		},
	}
	seedYAML, _ := yaml.Marshal(seed)
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, seedYAML, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := (agents.Goose{}).ConfigureMCP("/new/path", map[string]string{}, "", launch.ModeHost); err != nil {
		t.Fatalf("ConfigureMCP: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	var root map[string]any
	_ = yaml.Unmarshal(data, &root)
	exts, _ := root["extensions"].(map[string]any)
	if _, present := exts["aileron"]; present {
		t.Errorf("stale aileron entry under XDG_CONFIG_HOME not cleaned up: %v", exts)
	}
}
