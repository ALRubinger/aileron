package launch_test

import (
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
	"github.com/ALRubinger/aileron/core/launch/agents"
)

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := launch.NewRegistry()
	r.Register(agents.Claude{})

	agent, ok := r.Get("claude")
	if !ok {
		t.Fatal("expected to find agent 'claude'")
	}
	if agent.Name() != "claude" {
		t.Errorf("expected name 'claude', got %q", agent.Name())
	}
	if len(agent.BinaryNames()) == 0 || agent.BinaryNames()[0] != "claude" {
		t.Errorf("expected binary name 'claude', got %v", agent.BinaryNames())
	}
}

func TestRegistry_GetMissing(t *testing.T) {
	r := launch.NewRegistry()
	_, ok := r.Get("nonexistent")
	if ok {
		t.Fatal("expected Get to return false for unregistered agent")
	}
}

func TestRegistry_Names(t *testing.T) {
	r := launch.NewRegistry()
	r.Register(agents.Claude{})
	r.Register(testAgent{name: "aider"})
	r.Register(testAgent{name: "goose"})

	names := r.Names()
	expected := []string{"aider", "claude", "goose"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d names, got %d: %v", len(expected), len(names), names)
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, expected[i])
		}
	}
}

type testAgent struct {
	name string
}


func (a testAgent) Name() string                              { return a.name }
func (a testAgent) BinaryNames() []string                     { return []string{a.name} }
func (a testAgent) Args() []string                            { return nil }
func (a testAgent) Env() map[string]string                    { return nil }
func (a testAgent) NormalizeCommand(raw string) (string, bool) { return raw, true }
func (a testAgent) ConfigureShell(_, _ string) error           { return nil }
