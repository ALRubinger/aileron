// Package policy defines the SPI for the policy engine.
//
// The policy engine is the decision-making core of the control plane. It
// receives a normalized intent and returns a disposition: allow, deny,
// require approval, or request more evidence. All other components in the
// control plane consume this decision.
//
// The built-in implementation evaluates rule sets stored in the database.
// Alternative implementations (OPA, Cedar, custom DSLs) satisfy this
// interface without modifying the core.
package policy

import (
	"context"

	"github.com/ALRubinger/aileron/core/model"
)

// Engine evaluates whether an intent should be allowed, denied, or escalated.
type Engine interface {
	Evaluate(ctx context.Context, req EvaluationRequest) (model.Decision, error)
}

// EvaluationRequest is the input to policy evaluation.
type EvaluationRequest struct {
	WorkspaceID string
	AgentID     string
	Action      model.ActionIntent
	Context     model.IntentContext

	// ToolCall, when set, provides context about a proxied MCP tool call.
	// The gateway populates this field; the policy engine merges the
	// tool-call fields into the flat field map alongside any action fields.
	ToolCall *ToolCallContext
}

// ToolCallContext carries metadata about a proxied MCP tool call for use
// in policy evaluation. Fields are flattened into the "tool.*" namespace.
type ToolCallContext struct {
	// ServerName is the downstream MCP server name (e.g. "github").
	ServerName string
	// ToolName is the original tool name on the downstream server.
	ToolName string
	// QualifiedName is the namespaced tool name (e.g. "github__create_pull_request").
	QualifiedName string
	// Arguments are the tool call arguments provided by the agent.
	Arguments map[string]any
}
