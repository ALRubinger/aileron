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

	if c.Env() != nil {
		t.Errorf("expected nil Env, got %v", c.Env())
	}
}
