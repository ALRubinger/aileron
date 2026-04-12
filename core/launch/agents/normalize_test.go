package agents_test

import (
	"testing"

	"github.com/ALRubinger/aileron/core/launch/agents"
)

func TestNormalizeCommand_Claude(t *testing.T) {
	wrapped := "shopt -u extglob 2>/dev/null || true && eval 'rm -rf /tmp' < /dev/null && pwd -P >| /tmp/cwd"
	cmd, eval := agents.NormalizeCommand("claude", wrapped)
	if !eval {
		t.Fatal("expected evaluate=true for claude eval-wrapped command")
	}
	if cmd != "rm -rf /tmp" {
		t.Errorf("expected 'rm -rf /tmp', got %q", cmd)
	}
}

func TestNormalizeCommand_ClaudeInfrastructure(t *testing.T) {
	infra := `SNAPSHOT_FILE=/tmp/test.sh; echo done`
	cmd, eval := agents.NormalizeCommand("claude", infra)
	if eval {
		t.Fatal("expected evaluate=false for claude infrastructure command")
	}
	if cmd != infra {
		t.Errorf("expected command unchanged, got %q", cmd)
	}
}

func TestNormalizeCommand_Pi(t *testing.T) {
	cmd, eval := agents.NormalizeCommand("pi", "echo hello")
	if !eval {
		t.Fatal("expected evaluate=true for pi")
	}
	if cmd != "echo hello" {
		t.Errorf("expected 'echo hello', got %q", cmd)
	}
}

func TestNormalizeCommand_UnknownAgent(t *testing.T) {
	cmd, eval := agents.NormalizeCommand("unknown-agent", "echo hello")
	if !eval {
		t.Fatal("expected evaluate=true for unknown agent")
	}
	if cmd != "echo hello" {
		t.Errorf("expected 'echo hello', got %q", cmd)
	}
}
