// Package source defines the SPI for read-only source connectors.
//
// A source connector retrieves context from an external system (Slack,
// GitHub, Google Calendar, etc.) for use in draft generation. Unlike the
// connector package (which executes approved, irreversible actions), source
// connectors are read-only and require no approval flow. Credentials come
// from connected accounts.
//
// Each source connector exposes a set of tools that an LLM can call during
// draft generation. Tool definitions are LLM-agnostic — they can be converted
// to any LLM's function calling format by a thin adapter layer.
package source

import "context"

// SourceConnector retrieves context from an external system.
type SourceConnector interface {
	// Provider returns the provider name (e.g. "slack", "github").
	Provider() string

	// Tools returns the tool definitions this connector exposes.
	Tools() []ToolDefinition

	// Execute runs a tool by name with the given parameters and credential.
	// The token is the user's OAuth token retrieved from the vault.
	Execute(ctx context.Context, tool string, params map[string]any, token []byte) (map[string]any, error)
}

// ToolDefinition describes a tool that an LLM can call for context retrieval.
type ToolDefinition struct {
	// Name is the tool identifier (e.g. "slack_channel_history").
	Name string `json:"name"`

	// Description is a human-readable explanation for the LLM.
	Description string `json:"description"`

	// Parameters describes the expected input parameters.
	Parameters []ToolParam `json:"parameters"`
}

// ToolParam describes a single parameter for a tool.
type ToolParam struct {
	// Name is the parameter name.
	Name string `json:"name"`

	// Type is the parameter type ("string", "integer", "boolean").
	Type string `json:"type"`

	// Description explains the parameter to the LLM.
	Description string `json:"description"`

	// Required indicates whether the parameter must be provided.
	Required bool `json:"required"`
}
