package mcpremote

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/mcp"
)

// Compile-time check that *Client satisfies mcp.ToolExecutor.
var _ mcp.ToolExecutor = (*Client)(nil)

func TestNewClient_ReturnsClient(t *testing.T) {
	ctx := context.Background()
	c, err := NewClient(ctx, "github", "http://localhost:8080/mcp/servers/github")
	if err != nil {
		t.Fatalf("NewClient() unexpected error: %v", err)
	}
	if c.Name() != "github" {
		t.Errorf("Name() = %q, want %q", c.Name(), "github")
	}
	if len(c.Tools()) != 0 {
		t.Errorf("Tools() length = %d, want 0 (stub)", len(c.Tools()))
	}
}

func TestCallTool_ReturnsNotImplemented(t *testing.T) {
	ctx := context.Background()
	c, err := NewClient(ctx, "github", "http://localhost:8080/mcp/servers/github")
	if err != nil {
		t.Fatalf("NewClient() unexpected error: %v", err)
	}

	_, err = c.CallTool(ctx, "create_pr", map[string]any{"title": "test"})
	if err == nil {
		t.Fatal("CallTool() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error = %q, want mention of not yet implemented", err.Error())
	}
}

func TestClose_NoError(t *testing.T) {
	ctx := context.Background()
	c, err := NewClient(ctx, "github", "http://localhost:8080/mcp/servers/github")
	if err != nil {
		t.Fatalf("NewClient() unexpected error: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close() unexpected error: %v", err)
	}
}
