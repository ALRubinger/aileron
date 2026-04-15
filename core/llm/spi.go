// Package llm defines a provider-agnostic interface for LLM interactions.
//
// The Client interface supports tool-use loops: the LLM can request tool
// calls, the caller provides results, and the loop continues until the LLM
// produces a final text response. Tool execution logic is injected via
// a callback — the LLM client handles the protocol, the caller handles
// the business logic.
package llm

import (
	"context"

	"github.com/ALRubinger/aileron/core/source"
)

// Client generates text using a language model with optional tool access.
type Client interface {
	// GenerateWithTools sends a prompt with tool definitions and handles
	// tool call/result cycles until the LLM produces a final text response.
	// If no tools are provided, it performs a simple text generation.
	GenerateWithTools(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
}

// ToolExecutor is called when the LLM wants to invoke a tool.
// The pipeline provides this — it handles credential lookup and connector execution.
type ToolExecutor func(ctx context.Context, tool string, params map[string]any) (map[string]any, error)

// GenerateRequest contains everything needed to generate a draft.
type GenerateRequest struct {
	// SystemPrompt sets the LLM's behavior and context.
	SystemPrompt string

	// UserMessage is the user-facing content to respond to.
	UserMessage string

	// Tools available for the LLM to call during generation.
	Tools []source.ToolDefinition

	// ToolExecutor is called when the LLM requests a tool invocation.
	// May be nil if no tools are provided.
	ToolExecutor ToolExecutor

	// MaxToolRounds limits the number of tool-use round trips (default 5).
	MaxToolRounds int
}

// GenerateResponse contains the LLM's final output.
type GenerateResponse struct {
	// Text is the final generated content (the draft).
	Text string

	// ToolCalls records which tools were called during generation.
	ToolCalls []ToolCall
}

// ToolCall records a single tool invocation during generation.
type ToolCall struct {
	Tool   string         `json:"tool"`
	Params map[string]any `json:"params"`
}
