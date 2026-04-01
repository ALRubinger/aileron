package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/mcp"
)

// Compile-time check that *Client satisfies mcp.ToolExecutor.
var _ mcp.ToolExecutor = (*Client)(nil)

// ---------------------------------------------------------------------------
// Mock MCP server helper process
//
// When the test binary is re-invoked with MCPCLIENT_TEST_HELPER=1, it acts
// as a minimal MCP server that reads newline-delimited JSON-RPC from stdin
// and writes responses to stdout. This follows the pattern used by the
// standard library's os/exec tests.
// ---------------------------------------------------------------------------

func TestHelperProcess(t *testing.T) {
	if os.Getenv("MCPCLIENT_TEST_HELPER") != "1" {
		return
	}

	// We're acting as a mock MCP server. Read requests from stdin, write
	// responses to stdout. Exit cleanly on EOF.
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "mock server: bad request: %s\n", err)
			continue
		}

		resp := handleMockRequest(req)
		if resp == nil {
			// Notification — no response.
			continue
		}

		data, _ := json.Marshal(resp)
		fmt.Fprintf(os.Stdout, "%s\n", data)
	}
	os.Exit(0)
}

func handleMockRequest(req jsonrpcRequest) *jsonrpcResponse {
	// Notifications have no ID and expect no response.
	if req.ID == nil {
		return nil
	}

	idRaw, _ := json.Marshal(req.ID)

	switch req.Method {
	case "initialize":
		result, _ := json.Marshal(map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"serverInfo": map[string]any{
				"name":    "mock-mcp-server",
				"version": "0.0.1",
			},
		})
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      idRaw,
			Result:  result,
		}

	case "tools/list":
		result, _ := json.Marshal(map[string]any{
			"tools": []mcp.ToolDef{
				{
					Name:        "echo",
					Description: "Echoes the input back",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"message": map[string]any{"type": "string"},
						},
					},
				},
				{
					Name:        "greet",
					Description: "Returns a greeting",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]any{"type": "string"},
						},
					},
				},
			},
		})
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      idRaw,
			Result:  result,
		}

	case "tools/call":
		paramsRaw, _ := json.Marshal(req.Params)
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(paramsRaw, &params)

		switch params.Name {
		case "echo":
			msg, _ := params.Arguments["message"].(string)
			result, _ := json.Marshal(mcp.ToolResult{
				Content: []mcp.ToolContent{{Type: "text", Text: msg}},
			})
			return &jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      idRaw,
				Result:  result,
			}
		case "greet":
			name, _ := params.Arguments["name"].(string)
			result, _ := json.Marshal(mcp.ToolResult{
				Content: []mcp.ToolContent{{Type: "text", Text: "Hello, " + name + "!"}},
			})
			return &jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      idRaw,
				Result:  result,
			}
		case "fail":
			result, _ := json.Marshal(mcp.ToolResult{
				Content: []mcp.ToolContent{{Type: "text", Text: "something went wrong"}},
				IsError: true,
			})
			return &jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      idRaw,
				Result:  result,
			}
		default:
			return &jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      idRaw,
				Error:   &jsonrpcError{Code: -32601, Message: "unknown tool: " + params.Name},
			}
		}

	default:
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      idRaw,
			Error:   &jsonrpcError{Code: -32601, Message: "method not found"},
		}
	}
}

// helperCommand returns the command slice to re-invoke the test binary as a
// mock MCP server.
func helperCommand() ([]string, []string) {
	cmd := []string{os.Args[0], "-test.run=TestHelperProcess"}
	env := []string{"MCPCLIENT_TEST_HELPER=1"}
	return cmd, env
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewClient_InitializesAndDiscoversTools(t *testing.T) {
	ctx := context.Background()
	cmd, env := helperCommand()

	client, err := NewClient(ctx, "test-server", cmd, env)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	if client.Name() != "test-server" {
		t.Errorf("Name() = %q, want %q", client.Name(), "test-server")
	}

	tools := client.Tools()
	if len(tools) != 2 {
		t.Fatalf("len(Tools()) = %d, want 2", len(tools))
	}

	expected := map[string]bool{"echo": false, "greet": false}
	for _, tool := range tools {
		if _, ok := expected[tool.Name]; ok {
			expected[tool.Name] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("tool %q not found in discovered tools", name)
		}
	}
}

func TestCallTool_Success(t *testing.T) {
	ctx := context.Background()
	cmd, env := helperCommand()

	client, err := NewClient(ctx, "test-server", cmd, env)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	// Test echo tool.
	result, err := client.CallTool(ctx, "echo", map[string]any{"message": "hello world"})
	if err != nil {
		t.Fatalf("CallTool(echo): %v", err)
	}
	if result.IsError {
		t.Error("expected IsError=false for echo tool")
	}
	if len(result.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(result.Content))
	}
	if result.Content[0].Text != "hello world" {
		t.Errorf("Content[0].Text = %q, want %q", result.Content[0].Text, "hello world")
	}

	// Test greet tool.
	result, err = client.CallTool(ctx, "greet", map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("CallTool(greet): %v", err)
	}
	if result.IsError {
		t.Error("expected IsError=false for greet tool")
	}
	if len(result.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(result.Content))
	}
	if result.Content[0].Text != "Hello, Alice!" {
		t.Errorf("Content[0].Text = %q, want %q", result.Content[0].Text, "Hello, Alice!")
	}
}

func TestCallTool_Error(t *testing.T) {
	ctx := context.Background()
	cmd, env := helperCommand()

	client, err := NewClient(ctx, "test-server", cmd, env)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	result, err := client.CallTool(ctx, "fail", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool(fail): %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for fail tool")
	}
	if len(result.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(result.Content))
	}
	if result.Content[0].Text != "something went wrong" {
		t.Errorf("Content[0].Text = %q, want %q", result.Content[0].Text, "something went wrong")
	}
}

func TestClose_TerminatesSubprocess(t *testing.T) {
	ctx := context.Background()
	cmd, env := helperCommand()

	client, err := NewClient(ctx, "test-server", cmd, env)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// The subprocess should be running.
	if client.cmd.Process == nil {
		t.Fatal("expected subprocess to be running")
	}

	pid := client.cmd.Process.Pid

	// Close may return a "signal: terminated" error from Wait, which is
	// expected when we SIGTERM the subprocess. That's not a failure.
	_ = client.Close()

	// Give the OS a moment to clean up the process.
	time.Sleep(50 * time.Millisecond)

	// Verify the process is no longer running. Sending signal 0 checks
	// existence without actually signaling.
	proc, err := os.FindProcess(pid)
	if err != nil {
		// Process not found — good.
		return
	}
	// On Unix, FindProcess always succeeds; use Signal(0) to probe.
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Error("expected subprocess to be terminated, but it's still running")
	}
}

func TestNewClient_InvalidCommand(t *testing.T) {
	ctx := context.Background()

	_, err := NewClient(ctx, "bad", []string{"this-command-does-not-exist-12345"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid command")
	}
}
