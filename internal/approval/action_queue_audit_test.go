package approval

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// Audit-emission contract for the queue:
//   - Register fires an approval.requested event with the OTel-shaped
//     attribute keys (`aileron.approval.id`, `aileron.approval.kind`,
//     `aileron.approval.action`, plus connector / session when present).
//     Actor is the agent — the agent is what triggered the gate.
//   - Decide(approved=true) fires approval.approved; Decide(approved=false)
//     fires approval.denied. The approval.id matches the requested event
//     so consumers can correlate the two; wait_ms reflects request → decide.
//     Actor is the human user who decided.
//   - When EditedPayload is non-nil on Decide, the resolved event carries
//     `aileron.approval.edited = true`.
//   - SetAuditRecorder(nil) disables emission without breaking the queue.

func newRecorderWithStore(t *testing.T) (audit.Recorder, *audit.MemStore) {
	t.Helper()
	store := audit.NewMemStore()
	rec := audit.NewRecorder(store, nil, nil)
	return rec, store
}

func eventByType(events []audit.Event, evt model.EventType) (audit.Event, bool) {
	for _, e := range events {
		if e.EventType == evt {
			return e, true
		}
	}
	return audit.Event{}, false
}

func TestActionApprovalQueue_RegisterEmitsApprovalRequested(t *testing.T) {
	rec, store := newRecorderWithStore(t)
	q := NewActionApprovalQueue(nil, nil)
	q.SetAuditRecorder(rec)

	a := q.Register("send-email", "github://x/aileron-connector-google", "sess-42", map[string]any{"to": "alice"})

	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	got, ok := eventByType(events, model.EventTypeApprovalRequested)
	if !ok {
		t.Fatalf("expected approval.requested event; got %d events", len(events))
	}
	if got.Actor.Type != model.ActorTypeAgent {
		t.Errorf("Actor.Type = %q, want %q", got.Actor.Type, model.ActorTypeAgent)
	}
	if got.Payload["aileron.approval.id"] != a.ID {
		t.Errorf("aileron.approval.id = %v, want %q", got.Payload["aileron.approval.id"], a.ID)
	}
	if got.Payload["aileron.approval.action"] != "send-email" {
		t.Errorf("aileron.approval.action = %v, want send-email", got.Payload["aileron.approval.action"])
	}
	if got.Payload["aileron.approval.kind"] != string(ApprovalKindAction) {
		t.Errorf("aileron.approval.kind = %v, want %q", got.Payload["aileron.approval.kind"], ApprovalKindAction)
	}
	if got.Payload["aileron.connector.fqn"] != "github://x/aileron-connector-google" {
		t.Errorf("aileron.connector.fqn = %v", got.Payload["aileron.connector.fqn"])
	}
	if got.Payload["aileron.session.id"] != "sess-42" {
		t.Errorf("aileron.session.id = %v, want sess-42", got.Payload["aileron.session.id"])
	}
}

func TestActionApprovalQueue_RegisterOmitsAbsentOptionalAttrs(t *testing.T) {
	// When ConnectorFQN / SessionID are empty, the corresponding
	// attribute keys must not appear — empty strings on a span /
	// audit event are noise that downstream tooling has to filter out.
	rec, store := newRecorderWithStore(t)
	q := NewActionApprovalQueue(nil, nil)
	q.SetAuditRecorder(rec)

	q.Register("send-email", "", "", nil)

	events, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	got, ok := eventByType(events, model.EventTypeApprovalRequested)
	if !ok {
		t.Fatal("expected approval.requested event")
	}
	if _, present := got.Payload["aileron.connector.fqn"]; present {
		t.Errorf("aileron.connector.fqn should be absent when empty; got %v", got.Payload["aileron.connector.fqn"])
	}
	if _, present := got.Payload["aileron.session.id"]; present {
		t.Errorf("aileron.session.id should be absent when empty; got %v", got.Payload["aileron.session.id"])
	}
}

func TestActionApprovalQueue_ApproveEmitsApprovalApproved(t *testing.T) {
	rec, store := newRecorderWithStore(t)
	q := NewActionApprovalQueue(nil, nil)
	q.SetAuditRecorder(rec)

	a := q.Register("send-email", "github://x/y", "sess-1", nil)
	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	events, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	got, ok := eventByType(events, model.EventTypeApprovalApproved)
	if !ok {
		t.Fatalf("expected approval.approved event; events=%d", len(events))
	}
	if got.Actor.Type != model.ActorTypeHuman {
		t.Errorf("Actor.Type = %q, want %q", got.Actor.Type, model.ActorTypeHuman)
	}
	if got.Payload["aileron.approval.id"] != a.ID {
		t.Errorf("aileron.approval.id = %v, want %q (must match the requested event)", got.Payload["aileron.approval.id"], a.ID)
	}
	if got.Payload["aileron.approval.decision"] != "approved" {
		t.Errorf("aileron.approval.decision = %v, want approved", got.Payload["aileron.approval.decision"])
	}
	if got.Payload["aileron.approval.edited"] != false {
		t.Errorf("aileron.approval.edited = %v, want false", got.Payload["aileron.approval.edited"])
	}
}

func TestActionApprovalQueue_DenyEmitsApprovalDenied(t *testing.T) {
	rec, store := newRecorderWithStore(t)
	q := NewActionApprovalQueue(nil, nil)
	q.SetAuditRecorder(rec)

	a := q.Register("send-email", "github://x/y", "", nil)
	if err := q.Decide(a.ID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	events, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	got, ok := eventByType(events, model.EventTypeApprovalDenied)
	if !ok {
		t.Fatal("expected approval.denied event")
	}
	if got.Payload["aileron.approval.decision"] != "denied" {
		t.Errorf("aileron.approval.decision = %v, want denied", got.Payload["aileron.approval.decision"])
	}
	if got.Payload["aileron.approval.reason"] != "wrong recipient" {
		t.Errorf("aileron.approval.reason = %v, want %q", got.Payload["aileron.approval.reason"], "wrong recipient")
	}
}

func TestActionApprovalQueue_EditedDecisionFlagsEdited(t *testing.T) {
	rec, store := newRecorderWithStore(t)
	q := NewActionApprovalQueue(nil, nil)
	q.SetAuditRecorder(rec)

	a := q.RegisterCommsDraft("slack", "C123", "alice", "ping?", "pong", "ts-1", "")
	if err := q.Decide(a.ID, true, "", map[string]any{"body": "pong (edited)"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	events, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	got, ok := eventByType(events, model.EventTypeApprovalApproved)
	if !ok {
		t.Fatal("expected approval.approved event")
	}
	if got.Payload["aileron.approval.edited"] != true {
		t.Errorf("aileron.approval.edited = %v, want true", got.Payload["aileron.approval.edited"])
	}
	if got.Payload["aileron.approval.kind"] != string(ApprovalKindCommsDraft) {
		t.Errorf("aileron.approval.kind = %v, want %q", got.Payload["aileron.approval.kind"], ApprovalKindCommsDraft)
	}
}

func TestActionApprovalQueue_WaitMsReflectsRegisterToDecide(t *testing.T) {
	// Inject a clock that advances by a known delta between register
	// and decide so we can assert wait_ms is computed from those
	// timestamps, not from wall time.
	t0 := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	calls := 0
	now := func() time.Time {
		// First call: register. Second call: decide, 250ms later.
		t := t0
		if calls > 0 {
			t = t0.Add(250 * time.Millisecond)
		}
		calls++
		return t
	}
	rec, store := newRecorderWithStore(t)
	q := NewActionApprovalQueue(nil, now)
	q.SetAuditRecorder(rec)

	a := q.Register("send-email", "", "", nil)
	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	events, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	got, ok := eventByType(events, model.EventTypeApprovalApproved)
	if !ok {
		t.Fatal("expected approval.approved event")
	}
	if got.Payload["aileron.approval.wait_ms"] != int64(250) {
		t.Errorf("aileron.approval.wait_ms = %v, want 250", got.Payload["aileron.approval.wait_ms"])
	}
}

func TestActionApprovalQueue_NilRecorderEmitsNothing(t *testing.T) {
	// SetAuditRecorder(nil) should not break the queue's invariants.
	// The decide path must still unblock waiters and broadcast the
	// resolved event to subscribers.
	q := NewActionApprovalQueue(nil, nil)
	q.SetAuditRecorder(nil) // explicit no-op

	a := q.Register("send-email", "", "", nil)
	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
}

func TestActionApprovalQueue_NoRecorderConfiguredEmitsNothing(t *testing.T) {
	// Default state — recorder never set. Same expectation as above.
	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "", "", nil)
	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
}
