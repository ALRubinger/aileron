package runtime

// This file defines the public suspend/resume vocabulary the runtime returns
// and consumes (#2100). A run SUSPENDS at the first step it cannot complete
// in-band, for either of two reasons:
//
//   - an unfulfilled llm-seam step (no memo entry and no wired seam value able
//     to produce its output), or
//   - a gated action-call whose Approver returned Decision.Pending (no decision
//     yet).
//
// Both suspend through the same memoized-replay path. On resume, the caller
// supplies the accumulated step outputs (Options.ResumeOutputs); the runtime
// re-walks the DAG from the top, injects every memoized step's output WITHOUT
// re-executing it (exactly-once for effects), and continues from the first
// un-memoized step. This is Assumption B from the readiness brief: memoize ALL
// step outputs by step id, not just the seam, because topoSort orders by data
// edges only, so an effectful step can sit before a gated action and would
// otherwise re-run on resume.
//
// The memo is caller-owned, in-memory, and session-scoped. The runtime stays
// stateless with respect to cross-call storage; the daemon (#2101) owns the
// session store, mints run ids across process boundaries, drives the LLM
// provider for a suspended seam, and presents the pending approval.

// SuspendKind discriminates why a run suspended: an unfulfilled seam or a
// pending approval. It is a small closed enum mirroring StepKind /
// AuditRecordKind style.
type SuspendKind int

const (
	// SuspendKindSeam means the run suspended at an unfulfilled llm-seam step:
	// no memo entry carries its outputs and no seam value is wired to produce
	// them. The caller fulfills it by producing the seam's declared outputs (via
	// #2101's provider) and resuming with them added to the memo.
	SuspendKindSeam SuspendKind = iota
	// SuspendKindApproval means the run suspended at a gated action-call whose
	// Approver returned Decision.Pending. The effect did NOT fire. The caller
	// presents the approval, and on approve resumes with the same memo and an
	// Approver that now returns Decision.Approved for the step.
	SuspendKindApproval
)

// SuspendResult carries everything a caller needs to fulfill a suspended step
// and resume the run. It is surfaced on RunResult.Pending; a completed run has
// RunResult.Pending == nil. A suspend is NOT a failure: Run returns a nil error
// alongside a non-nil Pending.
type SuspendResult struct {
	// RunID is stable across the whole suspend/resume sequence. The runtime mints
	// one when Options.RunID is empty and echoes the provided one on resume, so
	// every call in a sequence shares the same id.
	RunID string
	// Kind discriminates the suspend reason (seam vs approval).
	Kind SuspendKind
	// StepID is the id of the step the run suspended at.
	StepID string

	// Seam is populated for a SuspendKindSeam suspend: the SeamRequest-shaped
	// payload the caller's provider (#2101) needs to produce the seam's outputs.
	// Nil for a SuspendKindApproval suspend.
	Seam *SeamRequest
	// Approval is populated for a SuspendKindApproval suspend: the
	// ApprovalRequest-shaped payload the caller's approval surface (#2101)
	// presents. Nil for a SuspendKindSeam suspend.
	Approval *ApprovalRequest

	// StepOutputs is the accumulated memo AT THE POINT OF SUSPENSION: every step
	// whose output was already memoized (injected this call) plus every step that
	// actually executed this call, keyed by step id. On resume the caller passes
	// this back as Options.ResumeOutputs (with the newly-fulfilled step's output
	// added, for a seam) so the runtime replays the completed prefix without
	// re-executing it.
	StepOutputs map[string]map[string]any
	// AuditIDs are the audit record ids emitted DURING THIS CALL only. Steps
	// injected from the memo re-audit nothing, so a multi-resume sequence records
	// each real execution exactly once. A suspend emits no per-launch summary
	// record; the terminal (completing) resume emits it.
	AuditIDs []string
}

// suspendSignal is the internal, pre-public struct execute returns to runPlan
// when the walk cannot complete a step in-band. runPlan converts it into a
// public SuspendResult (attaching the accumulated memo and the audit ids this
// call emitted). A nil suspendSignal from execute means the walk completed.
type suspendSignal struct {
	kind   SuspendKind
	stepID string
	// seam is set for SuspendKindSeam: the SeamRequest for the suspended step.
	seam *SeamRequest
	// approval is set for SuspendKindApproval: the ApprovalRequest for the
	// suspended step.
	approval *ApprovalRequest
}
