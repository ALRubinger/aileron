package approval

import (
	"time"

	"github.com/ALRubinger/aileron/internal/failure"
)

// PersistedRecord is the on-disk snapshot of one action-approval queue
// entry. A new record is appended on every state transition; loaders
// replay last-write-wins per ID. Stable wire shape — additions are
// non-breaking (encoding/json ignores unknown fields).
//
// All fields except ID, Status, and RequestedAt are optional and are
// populated as the entry progresses through its lifecycle.
type PersistedRecord struct {
	// --- Registration fields (set on Register, immutable afterwards). ---

	ID           string                        `json:"id"`
	Kind         ApprovalKind                  `json:"kind,omitempty"`
	ActionName   string                        `json:"action_name"`
	ConnectorFQN string                        `json:"connector_fqn,omitempty"`
	SessionID    string                        `json:"session_id,omitempty"`
	Args         map[string]any                `json:"args,omitempty"`
	Preview      *ActionApprovalPreview        `json:"preview,omitempty"`
	InputFields  []ActionApprovalPreviewField  `json:"input_fields,omitempty"`
	RequestedAt  time.Time                     `json:"requested_at"`

	// --- Outcome fields (mutated on each transition). ---

	Status        OutcomeStatus  `json:"status"`
	DecidedAt     *time.Time     `json:"decided_at,omitempty"`
	DenyReason    string         `json:"deny_reason,omitempty"`
	EditedPayload map[string]any `json:"edited_payload,omitempty"`
	AuditID       string         `json:"audit_id,omitempty"`
	Result        string         `json:"result,omitempty"`
	// Failure carries the structured failure envelope when Status is
	// [OutcomeFailed] and the action returned an ADR-0010 envelope.
	// Stored as the wire-format Envelope so the queue does not need to
	// reach into [failure.Failure]'s unexported fields at unmarshal time.
	Failure      *failure.Envelope `json:"failure,omitempty"`
	ErrorMessage string            `json:"error_message,omitempty"`
}

// Persister is the durable backing store for [ActionApprovalQueue]. The
// queue calls Append on every state transition (Register, Decide,
// SetAwaitingVault, SetRunning, SetCompleted, SetFailed) so a daemon
// restart can replay the file and resume in-flight approvals.
//
// Implementations must serialize concurrent Append calls — the queue
// holds its own mutex during the call, but a Persister may be shared
// across queues in tests. Errors are surfaced to the caller but the
// queue logs and continues; persistence is best-effort relative to
// in-memory correctness (a write error means the next restart loses
// the most recent transition, not that the queue's invariants are
// violated).
type Persister interface {
	// Append writes one record to the durable backing. Implementations
	// should flush before returning so a crash immediately after
	// Append doesn't lose the transition.
	Append(rec PersistedRecord) error
}

// noopPersister is the default Persister installed when none is
// configured — the queue operates purely in-memory, matching the
// pre-persistence behavior. Returning this from a helper keeps every
// queue method's persist hook a simple unconditional call.
type noopPersister struct{}

func (noopPersister) Append(PersistedRecord) error { return nil }

// SetPersister installs a Persister on the queue. Every subsequent
// state transition appends a record. Pass nil to revert to the no-op
// persister (the historic in-memory behavior). Safe to call
// concurrently with the queue's other methods; the swap is mutex-
// protected.
func (q *ActionApprovalQueue) SetPersister(p Persister) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p == nil {
		q.persister = noopPersister{}
		return
	}
	q.persister = p
}

// recordFor builds a PersistedRecord snapshot of the queue entry for
// id. Caller MUST hold q.mu — the snapshot reads multiple fields that
// other methods mutate under the same lock.
func (q *ActionApprovalQueue) recordFor(id string) (PersistedRecord, bool) {
	a, ok := q.pending[id]
	if !ok {
		return PersistedRecord{}, false
	}
	o := q.outcomes[id]
	rec := PersistedRecord{
		ID:           a.ID,
		Kind:         a.Kind,
		ActionName:   a.ActionName,
		ConnectorFQN: a.ConnectorFQN,
		SessionID:    a.SessionID,
		Args:         a.Args,
		Preview:      a.Preview,
		InputFields:  a.InputFields,
		RequestedAt:  a.RequestedAt,
	}
	if a.DecidedAt != nil {
		t := *a.DecidedAt
		rec.DecidedAt = &t
	}
	rec.EditedPayload = a.EditedPayload
	if o != nil {
		rec.Status = o.Status
		rec.DenyReason = o.DenyReason
		rec.AuditID = o.AuditID
		rec.Result = o.Result
		rec.ErrorMessage = o.ErrorMessage
		if o.Failure != nil {
			env := failure.ToEnvelope(o.Failure)
			rec.Failure = &env
		}
	}
	return rec, true
}

// LoadRecords seeds the queue from a snapshot of previously-persisted
// records. Called once at startup after the JSONL file has been
// replayed (see [internal/approval/jsonl.New]).
//
// Each record's terminal-or-transient Status is preserved verbatim;
// callers reap [OutcomeRunning] entries (mark them failed with a
// reconciliation message) BEFORE passing them here so the queue does
// not load a record whose action may have already produced side
// effects. The load path itself does no reaping — that policy lives
// with the wiring layer that knows the action manifest is still
// resolvable.
//
// Entries loaded in [OutcomeApprovedNotStarted] have their decision
// channel pre-filled with an approved [ActionDecision] so a respawned
// executor goroutine's Wait returns immediately. Entries loaded in
// [OutcomePendingApproval] arrive with an empty decision channel and
// wait for the user to decide via the webapp (the SSE / list surface
// includes them as soon as a subscriber connects).
//
// LoadRecords does NOT broadcast events or fire onRegister — replay
// is silent. Subscribers attached later read the current state via
// the snapshot path on Subscribe.
//
// Safe to call once on a fresh queue. Calling on an already-populated
// queue overwrites entries with matching IDs and leaves other entries
// untouched, but mixing replay and live use is not a supported
// pattern.
func (q *ActionApprovalQueue) LoadRecords(records []PersistedRecord) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, rec := range records {
		if rec.ID == "" {
			continue
		}
		a := &ActionApproval{
			ID:           rec.ID,
			Kind:         rec.Kind,
			ActionName:   rec.ActionName,
			ConnectorFQN: rec.ConnectorFQN,
			SessionID:    rec.SessionID,
			Args:         rec.Args,
			Preview:      rec.Preview,
			InputFields:  rec.InputFields,
			RequestedAt:  rec.RequestedAt,
			decision:     make(chan ActionDecision, 1),
		}
		if rec.DecidedAt != nil {
			t := *rec.DecidedAt
			a.DecidedAt = &t
		}
		a.EditedPayload = rec.EditedPayload
		o := &Outcome{
			Status:       rec.Status,
			AuditID:      rec.AuditID,
			Result:       rec.Result,
			DenyReason:   rec.DenyReason,
			ErrorMessage: rec.ErrorMessage,
		}
		if rec.Failure != nil {
			o.Failure = failure.FromEnvelope(*rec.Failure)
		}
		q.pending[rec.ID] = a
		q.outcomes[rec.ID] = o

		// Pre-fill the decision channel for entries that crossed the
		// Decide boundary before the crash. A respawned executor
		// goroutine's Wait will return this decision immediately so
		// it can proceed to SetRunning + Execute (or to the vault-
		// unlock wait, if the vault is locked at replay).
		if rec.Status == OutcomeApprovedNotStarted || rec.Status == OutcomeAwaitingVault {
			decision := ActionDecision{Approved: true, EditedPayload: rec.EditedPayload}
			if rec.DecidedAt != nil {
				decision.DecidedAt = *rec.DecidedAt
			}
			a.decision <- decision
		}
	}
}

// NonTerminalLoaded returns the IDs of currently-loaded entries whose
// Status is non-terminal — the set for which the wiring layer must
// respawn executor goroutines. Order is stable across calls (oldest
// first by RequestedAt) so test assertions don't flake on map
// iteration order.
//
// Excludes [OutcomeRunning] because the wiring layer is expected to
// reap those before calling [LoadRecords] — callers asking after this
// list have already classified replayed-running entries as
// reconciliation failures.
func (q *ActionApprovalQueue) NonTerminalLoaded() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	type idEntry struct {
		id          string
		requestedAt time.Time
	}
	entries := make([]idEntry, 0, len(q.outcomes))
	for id, o := range q.outcomes {
		switch o.Status {
		case OutcomePendingApproval, OutcomeApprovedNotStarted, OutcomeAwaitingVault:
			a := q.pending[id]
			entries = append(entries, idEntry{id: id, requestedAt: a.RequestedAt})
		}
	}
	// Stable insertion sort — len() is bounded by the daemon's history
	// of un-decided approvals, which is small in practice (handful).
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j-1].requestedAt.After(entries[j].requestedAt); j-- {
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.id
	}
	return out
}
