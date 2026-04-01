// Package mcpremote provides a remote MCP client that communicates with
// downstream MCP servers over HTTP using the MCP Streamable HTTP transport.
//
// This is a skeleton implementation. The HTTP transport will be implemented
// incrementally; for now, the client satisfies the mcp.ToolExecutor interface
// but returns an error for tool calls.
package mcpremote

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/mcp"
)

// Client connects to a remote MCP server managed by the Aileron control plane.
type Client struct {
	name     string
	endpoint string
	tools    []mcp.ToolDef
}

// NewClient creates a remote MCP client for the given endpoint.
//
// TODO: perform HTTP-based MCP initialize handshake and tool discovery.
func NewClient(_ context.Context, name string, endpoint string) (*Client, error) {
	return &Client{
		name:     name,
		endpoint: endpoint,
	}, nil
}

// Name returns the human-readable name of this remote server.
func (c *Client) Name() string { return c.name }

// Tools returns the cached list of tools discovered from the remote server.
func (c *Client) Tools() []mcp.ToolDef { return c.tools }

// CallTool invokes a tool on the remote MCP server.
//
// TODO: implement HTTP-based tool call via MCP Streamable HTTP transport.
func (c *Client) CallTool(_ context.Context, name string, _ map[string]any) (*mcp.ToolResult, error) {
	return nil, fmt.Errorf("mcpremote: tool %q: remote execution not yet implemented (endpoint: %s)", name, c.endpoint)
}

// Close releases any resources held by the client.
//
// TODO: close HTTP connection pool when implemented.
func (c *Client) Close() error {
	return nil
}
