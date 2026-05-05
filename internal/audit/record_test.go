package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/model"
)

// Recorder contract:
//   - RecordFailure mints an audit_id, stamps it on the failure, and
//     appends an EventTypeExecutionFailed event whose payload carries
//     the namespaced failure attributes (aileron.failure.class,
//     aileron.failure.boundary, aileron.failure.retriable,
//     aileron.failure.message, aileron.failure.details) per the
//     OTel-aligned audit schema (issue #390 Phase 6.5).
//   - RecordSuccess appends an event with the given type/payload.
//   - Clock and idFn are injectable for deterministic tests.
//   - Store.Append errors are swallowed (best-effort audit per ADR-0010).

type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

func newRecorderWithMem(t *testing.T) (*audit.MemStore, audit.Recorder, func() string) {
	t.Helper()
	store := audit.NewMemStore()
	var counter int
	idFn := func() string {
		counter++
		return "audit-" + string(rune('a'+counter-1))
	}
	r := audit.NewRecorder(store, fixedClock{t: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}, idFn)
	return store, r, idFn
}

func TestRecordFailure_StampsAuditIDAndAppends(t *testing.T) {
	store, r, _ := newRecorderWithMem(t)
	f := failure.Upstream(503, "service unavailable",
		failure.WithDetails(map[string]any{"retried": 2}))
	id := r.RecordFailure(context.Background(), f, model.ActorRef{ID: "agent-1", Type: model.ActorTypeAgent})

	if id == "" {
		t.Fatal("RecordFailure returned empty id")
	}
	if f.AuditID() != id {
		t.Errorf("audit_id stamp = %q, want %q", f.AuditID(), id)
	}
	ev, ok := store.GetByEventID(id)
	if !ok {
		t.Fatalf("event %q not found in store", id)
	}
	if ev.EventType != model.EventTypeExecutionFailed {
		t.Errorf("event type = %q, want execution.failed", ev.EventType)
	}
	if ev.Payload["aileron.failure.class"] != string(failure.ExternalAPIError) {
		t.Errorf("payload[aileron.failure.class] = %v", ev.Payload["aileron.failure.class"])
	}
	if ev.Payload["aileron.failure.retriable"] != true {
		t.Errorf("payload[aileron.failure.retriable] = %v", ev.Payload["aileron.failure.retriable"])
	}
	if ev.Payload["aileron.failure.message"] != "service unavailable" {
		t.Errorf("payload[aileron.failure.message] = %v", ev.Payload["aileron.failure.message"])
	}
	if d, ok := ev.Payload["aileron.failure.details"].(map[string]any); !ok || d["retried"] != 2 {
		t.Errorf("payload[aileron.failure.details] = %v", ev.Payload["aileron.failure.details"])
	}
	if ev.Actor.ID != "agent-1" {
		t.Errorf("actor.id = %q", ev.Actor.ID)
	}
}

func TestRecordFailure_NilFailureNoCrash(t *testing.T) {
	_, r, _ := newRecorderWithMem(t)
	id := r.RecordFailure(context.Background(), nil, model.ActorRef{})
	if id == "" {
		t.Error("RecordFailure(nil) should still mint an id")
	}
}

func TestRecordSuccess_AppendsEvent(t *testing.T) {
	store, r, _ := newRecorderWithMem(t)
	id := r.RecordSuccess(context.Background(),
		model.EventTypeToolCallIntercepted,
		model.ActorRef{ID: "aileron", Type: model.ActorTypeService},
		map[string]any{"tool": "ship_update"},
	)
	if id == "" {
		t.Fatal("RecordSuccess returned empty id")
	}
	ev, ok := store.GetByEventID(id)
	if !ok {
		t.Fatalf("event %q not found", id)
	}
	if ev.EventType != model.EventTypeToolCallIntercepted {
		t.Errorf("event type = %q", ev.EventType)
	}
	if ev.Payload["tool"] != "ship_update" {
		t.Errorf("payload.tool = %v", ev.Payload["tool"])
	}
}

func TestNewRecorder_DefaultsAreUsable(t *testing.T) {
	r := audit.NewRecorder(audit.NewMemStore(), nil, nil)
	id := r.RecordSuccess(context.Background(),
		model.EventTypeIntentSubmitted, model.ActorRef{}, nil)
	if id == "" || id == "audit-" {
		t.Errorf("default idFn produced empty/odd id %q", id)
	}
}

func TestDefaultIDFn_PrefixedAndUnique(t *testing.T) {
	a := audit.DefaultIDFn()
	b := audit.DefaultIDFn()
	if a == b {
		t.Error("DefaultIDFn returned duplicate ids")
	}
	for _, id := range []string{a, b} {
		if len(id) < len("audit-") || id[:len("audit-")] != "audit-" {
			t.Errorf("id %q lacks audit- prefix", id)
		}
	}
}

func TestMemStore_AppendAndList(t *testing.T) {
	store := audit.NewMemStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	if err := store.AppendToTrace(ctx, "trace-1", "intent-1", "ws-1", audit.Event{
		EventID:   "e-1",
		EventType: model.EventTypeIntentSubmitted,
		Actor:     model.ActorRef{ID: "u1"},
		Timestamp: now,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.AppendToTrace(ctx, "trace-1", "intent-1", "ws-1", audit.Event{
		EventID:   "e-2",
		EventType: model.EventTypeExecutionSucceeded,
		Actor:     model.ActorRef{ID: "agent-x"},
		Timestamp: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	traces, err := store.ListTraces(ctx, audit.Filter{IntentID: "intent-1"})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	if len(traces[0].Events) != 2 {
		t.Errorf("trace.Events = %d, want 2", len(traces[0].Events))
	}

	got, err := store.GetTrace(ctx, "trace-1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if got.IntentID != "intent-1" || got.WorkspaceID != "ws-1" {
		t.Errorf("GetTrace fields = %+v", got)
	}
	if _, err := store.GetTrace(ctx, "missing"); err == nil {
		t.Error("GetTrace(missing) should return error")
	}
}

func TestMemStore_FilterByEventTypeAndActorAndTime(t *testing.T) {
	store := audit.NewMemStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []audit.Event{
		{EventID: "1", EventType: model.EventTypeIntentSubmitted, Actor: model.ActorRef{ID: "a"}, Timestamp: now},
		{EventID: "2", EventType: model.EventTypeExecutionFailed, Actor: model.ActorRef{ID: "a"}, Timestamp: now.Add(time.Hour)},
		{EventID: "3", EventType: model.EventTypeExecutionFailed, Actor: model.ActorRef{ID: "b"}, Timestamp: now.Add(2 * time.Hour)},
	} {
		store.Append(ctx, e)
	}
	out, err := store.ListTraces(ctx, audit.Filter{EventType: model.EventTypeExecutionFailed, ActorID: "a"})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	got := 0
	for _, tr := range out {
		got += len(tr.Events)
	}
	if got != 1 {
		t.Errorf("filtered events = %d, want 1", got)
	}

	from := now.Add(30 * time.Minute)
	to := now.Add(90 * time.Minute)
	out, _ = store.ListTraces(ctx, audit.Filter{From: &from, To: &to})
	got = 0
	for _, tr := range out {
		got += len(tr.Events)
	}
	if got != 1 {
		t.Errorf("range-filtered events = %d, want 1", got)
	}
}
