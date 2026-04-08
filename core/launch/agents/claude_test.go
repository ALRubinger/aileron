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

	env := c.Env()
	if env == nil {
		t.Fatal("expected non-nil Env")
	}
	if env["AILERON_REAL_SHELL"] != "/bin/bash" {
		t.Errorf("expected AILERON_REAL_SHELL=/bin/bash, got %q", env["AILERON_REAL_SHELL"])
	}
}
