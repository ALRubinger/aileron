package mcp

import (
	"context"
	"testing"
)

// stubExecutor is a minimal ToolExecutor for verifying the interface contract.
type stubExecutor struct {
	name  string
	tools []ToolDef
}

func (s *stubExecutor) Name() string         { return s.name }
func (s *stubExecutor) Tools() []ToolDef     { return s.tools }
func (s *stubExecutor) Close() error          { return nil }
func (s *stubExecutor) CallTool(_ context.Context, _ string, _ map[string]any) (*ToolResult, error) {
	return &ToolResult{
		Content: []ToolContent{{Type: "text", Text: "ok"}},
	}, nil
}

// Compile-time check that stubExecutor satisfies ToolExecutor.
var _ ToolExecutor = (*stubExecutor)(nil)

func TestToolExecutor_InterfaceContract(t *testing.T) {
	exec := &stubExecutor{
		name: "test",
		tools: []ToolDef{
			{Name: "echo", Description: "echoes input", InputSchema: map[string]any{"type": "object"}},
		},
	}

	if exec.Name() != "test" {
		t.Errorf("Name() = %q, want %q", exec.Name(), "test")
	}
	if len(exec.Tools()) != 1 {
		t.Fatalf("Tools() length = %d, want 1", len(exec.Tools()))
	}

	result, err := exec.CallTool(context.Background(), "echo", map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatalf("CallTool() error: %v", err)
	}
	if result.Content[0].Text != "ok" {
		t.Errorf("CallTool() text = %q, want %q", result.Content[0].Text, "ok")
	}

	if err := exec.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}
