// Package mcp defines shared types and interfaces for MCP tool execution.
//
// The ToolExecutor interface abstracts tool discovery and invocation across
// transports, enabling both local (stdio subprocess) and remote (HTTP)
// downstream MCP servers behind a common contract.
package mcp

import "context"

// ToolDef describes a tool exposed by a downstream MCP server.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolResult is the result from a tools/call invocation.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent represents a single content block within a tool result.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolExecutor abstracts tool discovery and invocation across transports.
// Both local subprocess clients (stdio) and remote HTTP clients implement
// this interface, allowing the gateway to route tool calls without knowing
// the underlying transport.
type ToolExecutor interface {
	// Name returns the human-readable name of this downstream server.
	Name() string

	// Tools returns the cached list of tools discovered from the server.
	Tools() []ToolDef

	// CallTool invokes a tool on the downstream MCP server by name.
	CallTool(ctx context.Context, name string, arguments map[string]any) (*ToolResult, error)

	// Close terminates the connection to the downstream server.
	Close() error
}
