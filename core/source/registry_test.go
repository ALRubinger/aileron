package source_test

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/source"
)

// stubConnector is a minimal SourceConnector for testing the registry.
type stubConnector struct {
	provider string
	tools    []source.ToolDefinition
}

func (s *stubConnector) Provider() string                { return s.provider }
func (s *stubConnector) Tools() []source.ToolDefinition  { return s.tools }
func (s *stubConnector) Execute(_ context.Context, tool string, _ map[string]any, _ []byte) (map[string]any, error) {
	return map[string]any{"tool": tool, "provider": s.provider}, nil
}

func newStubConnector(provider string, toolNames ...string) *stubConnector {
	tools := make([]source.ToolDefinition, len(toolNames))
	for i, name := range toolNames {
		tools[i] = source.ToolDefinition{Name: name, Description: "test tool"}
	}
	return &stubConnector{provider: provider, tools: tools}
}

func TestRegistry_Register_And_Get(t *testing.T) {
	r := source.NewRegistry()
	sc := newStubConnector("slack", "slack_channel_history")
	r.Register(sc)

	got, ok := r.Get("slack")
	if !ok {
		t.Fatal("expected slack connector to be registered")
	}
	if got.Provider() != "slack" {
		t.Errorf("expected slack, got %s", got.Provider())
	}
}

func TestRegistry_Get_NotFound(t *testing.T) {
	r := source.NewRegistry()
	_, ok := r.Get("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestRegistry_ResolveToolProvider(t *testing.T) {
	r := source.NewRegistry()
	r.Register(newStubConnector("slack", "slack_channel_history", "slack_search_messages"))

	provider, err := r.ResolveToolProvider("slack_channel_history")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != "slack" {
		t.Errorf("expected slack, got %s", provider)
	}
}

func TestRegistry_ResolveToolProvider_Unknown(t *testing.T) {
	r := source.NewRegistry()
	_, err := r.ResolveToolProvider("nonexistent_tool")
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestRegistry_ExecuteTool(t *testing.T) {
	r := source.NewRegistry()
	r.Register(newStubConnector("slack", "slack_channel_history"))

	result, err := r.ExecuteTool(context.Background(), "slack_channel_history", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["tool"] != "slack_channel_history" {
		t.Errorf("expected tool=slack_channel_history, got %v", result["tool"])
	}
	if result["provider"] != "slack" {
		t.Errorf("expected provider=slack, got %v", result["provider"])
	}
}

func TestRegistry_ExecuteTool_Unknown(t *testing.T) {
	r := source.NewRegistry()
	_, err := r.ExecuteTool(context.Background(), "nonexistent", nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestRegistry_AllTools(t *testing.T) {
	r := source.NewRegistry()
	r.Register(newStubConnector("slack", "slack_channel_history", "slack_search_messages"))
	r.Register(newStubConnector("github", "github_search_code"))

	tools := r.AllTools()
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}
}

func TestRegistry_ToolsForProvider(t *testing.T) {
	r := source.NewRegistry()
	r.Register(newStubConnector("slack", "slack_channel_history", "slack_search_messages"))
	r.Register(newStubConnector("github", "github_search_code"))

	tools := r.ToolsForProvider("slack")
	if len(tools) != 2 {
		t.Fatalf("expected 2 slack tools, got %d", len(tools))
	}

	tools = r.ToolsForProvider("nonexistent")
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools for nonexistent, got %d", len(tools))
	}
}

func TestRegistry_Providers(t *testing.T) {
	r := source.NewRegistry()
	r.Register(newStubConnector("slack", "slack_channel_history"))
	r.Register(newStubConnector("github", "github_search_code"))

	providers := r.Providers()
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
}

func TestRegistry_Empty(t *testing.T) {
	r := source.NewRegistry()
	if len(r.AllTools()) != 0 {
		t.Error("expected no tools in empty registry")
	}
	if len(r.Providers()) != 0 {
		t.Error("expected no providers in empty registry")
	}
}
