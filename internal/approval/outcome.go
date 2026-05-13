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

	// OutcomeApprovedNotStarted — the user approved but the daemon's
	// background executor has not yet called the action's Execute. The
	// queue persists this state on every Decide(approved=true) so a
	// crash between approval and execution is observable on replay
	// (the entry replays as approved, the executor goroutine is
	// respawned, and it proceeds to call Execute). Without this state,
	// a crash in the same window would lose the approval entirely.
	//
	// Transient: production callers never see this on /result because
	// the executor goroutine flips to [OutcomeRunning] before invoking
	// Execute. Surfaced on the wire as `pending_approval` so external
	// consumers don't need to special-case it.
	OutcomeApprovedNotStarted OutcomeStatus = "approved_not_started"

	// OutcomeAwaitingVault — the user approved but the daemon's local
	// vault is locked, so the executor goroutine is waiting on the
	// vault-unlock signal before invoking Execute. Only reached on
	// replay: a daemon restart picks up the entry as approved, finds
	// the vault locked, and parks the executor until the user unlocks
	// the vault via the webapp.
	//
	// Distinct from [OutcomeApprovedNotStarted] so the agent's poll on
	// `/v1/action-approvals/{id}/result` can surface "waiting on you
	// to unlock the vault" rather than the more generic "still
	// pending". Flips to [OutcomeRunning] when the executor wakes.
	OutcomeAwaitingVault OutcomeStatus = "awaiting_vault"

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
	//
	// Also reached at replay time when a `running` entry is found on
	// disk: the daemon crashed mid-execution, so the action may have
	// produced side-effects (e.g. send-email is not idempotent). The
	// runtime refuses to auto-rerun and stamps a reconciliation
	// message in [Outcome.ErrorMessage] for the user to investigate.
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
