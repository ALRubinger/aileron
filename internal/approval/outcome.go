package approval

import "github.com/ALRubinger/aileron/internal/failure"

// OutcomeStatus is the closed lifecycle of an action-approval entry
// from registration through terminal state. Reported by the queue to
// the `/v1/action-approvals/{id}/result` handler and surfaced to the
// agent via the `check_action_status` MCP tool.
//
// The five states cover what the agent / user cares about; intermediate
// internal transitions (e.g. "decided but executor not yet started")
// are not modeled separately because the user-visible answer is the
// same either way.
type OutcomeStatus string

const (
	// OutcomePendingApproval — the entry is registered; the user has
	// not yet decided.
	OutcomePendingApproval OutcomeStatus = "pending_approval"

	// OutcomeRunning — the user approved; the daemon's background
	// executor is running the action. Transient.
	OutcomeRunning OutcomeStatus = "running"

	// OutcomeCompleted — the action ran successfully. [Outcome.AuditID]
	// and [Outcome.Result] are populated.
	OutcomeCompleted OutcomeStatus = "completed"

	// OutcomeDenied — the user denied the approval. [Outcome.DenyReason]
	// carries any commentary the user attached.
	OutcomeDenied OutcomeStatus = "denied"

	// OutcomeFailed — the action was approved but its execution errored
	// or returned an ADR-0010 failure envelope. [Outcome.Failure]
	// carries the structured failure when one is available; for
	// executor-level errors (no envelope) [Outcome.ErrorMessage]
	// carries the plain-text reason.
	OutcomeFailed OutcomeStatus = "failed"
)

// Outcome is the terminal-or-transient state of an action-approval
// entry. The zero value is invalid; use [ActionApprovalQueue.Outcome]
// to read it. Fields beyond Status are populated according to Status:
//
//   - OutcomePendingApproval, OutcomeRunning — no other fields.
//   - OutcomeCompleted — AuditID, Result.
//   - OutcomeDenied — DenyReason (may be empty).
//   - OutcomeFailed — exactly one of Failure or ErrorMessage.
//
// Stored on the queue rather than on [ActionApproval] directly so the
// queue's mutex guards every read and write — callers can't race with
// background executors recording results.
type Outcome struct {
	Status       OutcomeStatus
	AuditID      string
	Result       string
	DenyReason   string
	Failure      *failure.Failure
	ErrorMessage string
}
