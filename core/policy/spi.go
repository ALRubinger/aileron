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
}
