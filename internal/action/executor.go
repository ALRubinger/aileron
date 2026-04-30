package action

import (
	"context"
	"encoding/json"
)

// Executor runs an installed action with the given call-time arguments
// and returns a Result whose Content becomes the tool-result message
// the LLM observes.
//
// Per [ADR-0010], action-side failures are returned as Results with
// IsError=true rather than as Go errors — the LLM sees the failure as
// a tool result and can decide how to proceed (retry, fall back to a
// different action, ask the user). A returned `error` is reserved for
// gateway-fatal conditions that should terminate the conversation
// turn (e.g. action not found, executor misconfigured).
//
// [ADR-0010]: https://docs.withaileron.ai/adr/0010-failure-handling
type Executor interface {
	Execute(ctx context.Context, name string, args map[string]any) (Result, error)
}

// Result is the synthesized tool-result content surfaced to the LLM.
// Both OpenAI's `tool` message and Anthropic's `tool_result` content
// block use a string body; the IsError flag is mapped to Anthropic's
// `is_error` field and prepended to OpenAI's content as a marker.
type Result struct {
	// Content is the JSON-encoded tool result body. Implementations
	// SHOULD return JSON the LLM can parse, but plain prose is
	// acceptable when the action's output is naturally a sentence.
	Content string

	// IsError reports whether the action failed in a way the LLM
	// should be informed of. Per ADR-0010, this surfaces as
	// `is_error: true` on Anthropic tool_result blocks and is encoded
	// into the OpenAI tool-message body as a sentinel JSON shape.
	IsError bool
}

// StubExecutor returns a placeholder JSON result describing what
// would have been executed. It exists so the interception machinery
// can be exercised end-to-end before the connector-orchestration
// runtime is wired up. Real action execution lands in a follow-up
// issue.
type StubExecutor struct{}

// Execute returns a Result whose Content is a small JSON object
// summarising the call. Always succeeds; never returns a Go error.
func (StubExecutor) Execute(_ context.Context, name string, args map[string]any) (Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{
		"executed": false,
		"stub":     true,
		"action":   name,
		"args":     args,
		"note":     "Action execution is not yet implemented; this is a placeholder result.",
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Content: string(body)}, nil
}
