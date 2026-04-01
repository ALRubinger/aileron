// Package mcpclient provides a Go client that speaks Model Context Protocol (MCP)
// over stdio to downstream MCP servers running as subprocesses.
//
// Communication uses newline-delimited JSON-RPC 2.0 over the subprocess's
// stdin (requests) and stdout (responses).
package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/core/mcp"
	"github.com/ALRubinger/aileron/core/version"
)

// Client manages a connection to a single downstream MCP server subprocess.
type Client struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex // protects writes to stdin and reads from stdout
	tools  []mcp.ToolDef
	nextID int64
}

// JSON-RPC 2.0 wire types (internal).

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonrpcError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// NewClient spawns a downstream MCP server as a subprocess and performs the
// MCP initialize handshake. The command slice must contain at least the
// program name; additional elements are passed as arguments. The env slice
// is merged with os.Environ() so the subprocess inherits the current
// environment plus any overrides.
func NewClient(ctx context.Context, name string, command []string, env []string) (*Client, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("mcpclient: command must not be empty")
	}

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcpclient: stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcpclient: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcpclient: start %q: %w", command[0], err)
	}

	c := &Client{
		name:   name,
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: bufio.NewReader(stdoutPipe),
		nextID: 1, // 1 is reserved for the initialize request
	}

	// --- MCP initialize handshake ---

	initParams := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "aileron-gateway",
			"version": version.Version,
		},
	}

	_, err = c.sendRequest("initialize", initParams)
	if err != nil {
		// Best-effort cleanup on failure.
		_ = c.Close()
		return nil, fmt.Errorf("mcpclient: initialize handshake: %w", err)
	}

	// Send the initialized notification (no ID — it's a notification).
	if err := c.sendNotification("notifications/initialized"); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcpclient: initialized notification: %w", err)
	}

	// Discover available tools.
	if _, err := c.DiscoverTools(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcpclient: discover tools: %w", err)
	}

	return c, nil
}

// DiscoverTools sends a tools/list request and caches the result.
func (c *Client) DiscoverTools(_ context.Context) ([]mcp.ToolDef, error) {
	raw, err := c.sendRequest("tools/list", nil)
	if err != nil {
		return nil, err
	}

	var envelope struct {
		Tools []mcp.ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("mcpclient: parse tools/list result: %w", err)
	}

	c.tools = envelope.Tools
	return c.tools, nil
}

// CallTool invokes a tool on the downstream MCP server by name.
func (c *Client) CallTool(_ context.Context, name string, arguments map[string]any) (*mcp.ToolResult, error) {
	params := map[string]any{
		"name":      name,
		"arguments": arguments,
	}

	raw, err := c.sendRequest("tools/call", params)
	if err != nil {
		return nil, err
	}

	var result mcp.ToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcpclient: parse tools/call result: %w", err)
	}

	return &result, nil
}

// Tools returns the cached list of tools discovered from the server.
func (c *Client) Tools() []mcp.ToolDef {
	return c.tools
}

// Name returns the human-readable name of this client connection.
func (c *Client) Name() string {
	return c.name
}

// Close terminates the subprocess. It sends SIGTERM first, then SIGKILL
// after a 5-second timeout if the process hasn't exited.
func (c *Client) Close() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}

	// Close stdin so the subprocess sees EOF.
	_ = c.stdin.Close()

	// Send SIGTERM.
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// Process may have already exited; just wait.
		_ = c.cmd.Wait()
		return nil
	}

	// Wait up to 5 seconds for graceful exit.
	done := make(chan error, 1)
	go func() {
		done <- c.cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		return <-done
	}
}

// nextRequestID returns a monotonically increasing request ID.
func (c *Client) nextRequestID() int64 {
	return atomic.AddInt64(&c.nextID, 1)
}

// sendRequest sends a JSON-RPC request and waits for the corresponding
// response. It returns the raw "result" field on success.
func (c *Client) sendRequest(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextRequestID()
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: marshal request: %w", err)
	}

	// Write newline-delimited JSON.
	if _, err := fmt.Fprintf(c.stdin, "%s\n", data); err != nil {
		return nil, fmt.Errorf("mcpclient: write request: %w", err)
	}

	resp, err := c.readResponse()
	if err != nil {
		return nil, err
	}

	if resp.Error != nil {
		return nil, resp.Error
	}

	return resp.Result, nil
}

// sendNotification sends a JSON-RPC notification (no ID, no response expected).
func (c *Client) sendNotification(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  method,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("mcpclient: marshal notification: %w", err)
	}

	if _, err := fmt.Fprintf(c.stdin, "%s\n", data); err != nil {
		return fmt.Errorf("mcpclient: write notification: %w", err)
	}

	return nil
}

// readResponse reads one newline-delimited JSON line from the subprocess
// stdout and parses it as a JSON-RPC response.
func (c *Client) readResponse() (*jsonrpcResponse, error) {
	line, err := c.stdout.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("mcpclient: read response: %w", err)
	}

	var resp jsonrpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("mcpclient: parse response: %w (raw: %s)", err, string(line))
	}

	return &resp, nil
}
