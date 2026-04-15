package source

import (
	"context"
	"fmt"
)

// Registry holds registered source connectors and routes tool calls
// to the correct connector. It is safe for concurrent read access after
// initial registration.
type Registry struct {
	connectors map[string]SourceConnector
	toolIndex  map[string]SourceConnector // tool name → connector
}

// NewRegistry creates an empty source connector registry.
func NewRegistry() *Registry {
	return &Registry{
		connectors: make(map[string]SourceConnector),
		toolIndex:  make(map[string]SourceConnector),
	}
}

// Register adds a source connector and indexes its tools.
func (r *Registry) Register(sc SourceConnector) {
	r.connectors[sc.Provider()] = sc
	for _, t := range sc.Tools() {
		r.toolIndex[t.Name] = sc
	}
}

// Get returns the source connector for the given provider.
func (r *Registry) Get(provider string) (SourceConnector, bool) {
	sc, ok := r.connectors[provider]
	return sc, ok
}

// ResolveToolProvider returns the provider name for a given tool.
func (r *Registry) ResolveToolProvider(tool string) (string, error) {
	sc, ok := r.toolIndex[tool]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", tool)
	}
	return sc.Provider(), nil
}

// ExecuteTool resolves a tool to its connector and executes it.
func (r *Registry) ExecuteTool(ctx context.Context, tool string, params map[string]any, token []byte) (map[string]any, error) {
	sc, ok := r.toolIndex[tool]
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", tool)
	}
	return sc.Execute(ctx, tool, params, token)
}

// AllTools returns tool definitions across all registered connectors.
func (r *Registry) AllTools() []ToolDefinition {
	var tools []ToolDefinition
	for _, sc := range r.connectors {
		tools = append(tools, sc.Tools()...)
	}
	return tools
}

// ToolsForProvider returns tool definitions for a specific provider.
func (r *Registry) ToolsForProvider(provider string) []ToolDefinition {
	sc, ok := r.connectors[provider]
	if !ok {
		return nil
	}
	return sc.Tools()
}

// Providers returns all registered provider names.
func (r *Registry) Providers() []string {
	out := make([]string, 0, len(r.connectors))
	for p := range r.connectors {
		out = append(out, p)
	}
	return out
}
