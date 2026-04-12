package agents_test

import (
	"testing"

	"github.com/ALRubinger/aileron/core/launch/agents"
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
	found := false
	for i, a := range args {
		if a == "--allowedTools" && i+1 < len(args) && args[i+1] == "Bash(*)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --allowedTools Bash(*) in Args, got %v", args)
	}

	env := c.Env()
	if env == nil {
		t.Fatal("expected non-nil Env")
	}
	if env["AILERON_REAL_SHELL"] != "/bin/bash" {
		t.Errorf("expected AILERON_REAL_SHELL=/bin/bash, got %q", env["AILERON_REAL_SHELL"])
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
