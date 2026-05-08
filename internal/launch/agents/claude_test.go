package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func TestClaude(t *testing.T) {
	c := agents.Claude{}

	if c.Name() != "claude" {
		t.Errorf("expected name 'claude', got %q", c.Name())
	}

	names := c.BinaryNames()
	if len(names) != 1 || names[0] != "claude" {
		t.Errorf("expected BinaryNames [\"claude\"], got %v", names)
	}

	args := c.Args()
	if len(args) < 2 {
		t.Fatal("expected Args with --allowedTools")
	}
	var allowedToolsValue string
	for i, a := range args {
		if a == "--allowedTools" && i+1 < len(args) {
			allowedToolsValue = args[i+1]
			break
		}
	}
	if allowedToolsValue == "" {
		t.Fatalf("expected --allowedTools <value> in Args, got %v", args)
	}
	if !strings.Contains(allowedToolsValue, "Bash(*)") {
		t.Errorf("expected Bash(*) in --allowedTools value, got %q", allowedToolsValue)
	}
	// Finding #6 of #532: the Aileron MCP server's tools must be
	// pre-approved so Claude Code does not double-prompt for tools
	// the daemon already gates. The bare `mcp__<server>` form covers
	// every tool from that server.
	wantMCP := "mcp__" + launch.MCPServerName
	if !strings.Contains(allowedToolsValue, wantMCP) {
		t.Errorf("expected %q in --allowedTools value to suppress per-tool prompts for Aileron MCP tools, got %q",
			wantMCP, allowedToolsValue)
	}

	env := c.Env()
	if env == nil {
		t.Fatal("expected non-nil Env")
	}
	if env["AILERON_REAL_SHELL"] != "/bin/bash" {
		t.Errorf("expected AILERON_REAL_SHELL=/bin/bash, got %q", env["AILERON_REAL_SHELL"])
	}
	if env["CLAUDE_CODE_SHELL"] == "" {
		t.Error("expected CLAUDE_CODE_SHELL to be set")
	}
	if !strings.Contains(env["CLAUDE_CODE_SHELL"], "bash") {
		t.Errorf("CLAUDE_CODE_SHELL path must contain 'bash', got %q", env["CLAUDE_CODE_SHELL"])
	}
}

func TestClaude_NormalizeCommand_EvalWrapper(t *testing.T) {
	c := agents.Claude{}

	wrapped := "shopt -u extglob 2>/dev/null || true && eval 'echo hello' < /dev/null && pwd -P >| /tmp/cwd"
	cmd, eval := c.NormalizeCommand(wrapped)
	if !eval {
		t.Fatal("expected evaluate=true for eval-wrapped command")
	}
	if cmd != "echo hello" {
		t.Errorf("expected 'echo hello', got %q", cmd)
	}
}

func TestClaude_NormalizeCommand_UnquotedEval(t *testing.T) {
	c := agents.Claude{}

	wrapped := `shopt -u extglob 2>/dev/null || true && eval echo hello \< /dev/null && pwd -P >| /tmp/cwd`
	cmd, eval := c.NormalizeCommand(wrapped)
	if !eval {
		t.Fatal("expected evaluate=true for unquoted eval command")
	}
	if cmd != "echo hello" {
		t.Errorf("expected 'echo hello', got %q", cmd)
	}
}

func TestClaude_NormalizeCommand_Infrastructure(t *testing.T) {
	c := agents.Claude{}

	infra := `SNAPSHOT_FILE=/tmp/test.sh; echo "# snapshot" > "$SNAPSHOT_FILE"`
	cmd, eval := c.NormalizeCommand(infra)
	if eval {
		t.Fatal("expected evaluate=false for infrastructure command")
	}
	if cmd != infra {
		t.Errorf("expected command unchanged, got %q", cmd)
	}
}

func TestClaude_ConfigureShell_InstallsWrapper(t *testing.T) {
	c := agents.Claude{}
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := c.ConfigureShell("/usr/local/bin/aileron-sh", t.TempDir()); err != nil {
		t.Fatalf("ConfigureShell failed: %v", err)
	}

	wrapperPath := filepath.Join(dir, ".aileron", "bash")
	info, err := os.Stat(wrapperPath)
	if err != nil {
		t.Fatalf("expected wrapper at %s: %v", wrapperPath, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("wrapper should be executable")
	}
}

func TestClaude_NormalizeCommand_UnquotedEvalNoSeparator(t *testing.T) {
	c := agents.Claude{}

	// Unquoted eval with no separator — the entire rest is the command.
	wrapped := "shopt -u extglob 2>/dev/null || true && eval ls"
	cmd, eval := c.NormalizeCommand(wrapped)
	if !eval {
		t.Fatal("expected evaluate=true")
	}
	if cmd != "ls" {
		t.Errorf("expected 'ls', got %q", cmd)
	}
}

func TestClaude_NormalizeCommand_EvalWithEscapedQuotes(t *testing.T) {
	c := agents.Claude{}

	wrapped := `shopt -u extglob 2>/dev/null || true && eval 'echo '\''hello world'\''' < /dev/null && pwd -P >| /tmp/cwd`
	cmd, eval := c.NormalizeCommand(wrapped)
	if !eval {
		t.Fatal("expected evaluate=true")
	}
	if cmd != "echo 'hello world'" {
		t.Errorf("expected \"echo 'hello world'\", got %q", cmd)
	}
}
