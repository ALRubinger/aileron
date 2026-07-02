package audit

import (
	"context"
	"time"

	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/google/uuid"
)

// Recorder writes audit events for failures and successes flowing
// through the gateway, action installer, and connector installer.
//
// The package-level [Store] SPI handles persistence; Recorder layers
// on top of it to mint the audit_id, format the [Event] payload, and
// stamp the audit_id back onto [failure.Failure] so the envelope
// returned to the caller carries a working back-reference.
type Recorder interface {
	// RecordFailure persists the failure as an audit event and
	// returns the minted audit_id. The same id is also stamped onto
	// the failure via failure.SetAuditID so callers can write the
	// envelope without an extra lookup.
	RecordFailure(ctx context.Context, f *failure.Failure, actor model.ActorRef) string

	// RecordSuccess persists a non-failure event with the given type
	// and payload. Returns the minted event_id. The store append error
	// is swallowed: these are best-effort action-audit writes per
	// ADR-0010 and must not block the response they accompany.
	RecordSuccess(ctx context.Context, eventType model.EventType, actor model.ActorRef, payload map[string]any) string

	// RecordEvent persists a client-supplied event durably and returns
	// the minted event_id together with the store append error. Unlike
	// RecordSuccess it surfaces the persistence outcome, so the
	// `POST /v1/audit` ingest path can return a non-2xx when the append
	// fails rather than acknowledging a write that never committed.
	RecordEvent(ctx context.Context, eventType model.EventType, actor model.ActorRef, payload map[string]any) (string, error)
}

// NewRecorder returns a Recorder backed by the given Store. clk
// supplies timestamps; pass clock.System{} in production. idFn mints
// audit_ids; pass DefaultIDFn for the default UUID-based scheme.
func NewRecorder(s Store, clk Clock, idFn func() string) Recorder {
	if idFn == nil {
		idFn = DefaultIDFn
	}
	if clk == nil {
		clk = systemClock{}
	}
	return &recorder{store: s, clk: clk, idFn: idFn}
}

// Clock is the minimal interface Recorder needs from a clock. It
// matches the shape of internal/clock.Clock without taking the
// dependency, so this package stays small.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// DefaultIDFn returns a UUIDv4-based audit_id prefixed with `audit-`
// per ADR-0010's example shape.
func DefaultIDFn() string {
	return "audit-" + uuid.NewString()
}

type recorder struct {
	store Store
	clk   Clock
	idFn  func() string
}

// RecordFailure mints an audit_id, stamps it on f, and appends an
// event of type EventTypeExecutionFailed with the failure shape.
//
// The store error is intentionally swallowed: ADR-0010 wants the
// audit log to be a best-effort companion to the response — a
// failed audit write must not block the failure response. Production
// stores wrap this with an alerter / fallback.
func (r *recorder) RecordFailure(ctx context.Context, f *failure.Failure, actor model.ActorRef) string {
	id := r.idFn()
	if f != nil {
		f.SetAuditID(id)
	}
	// Audit-log payload uses OTel-namespaced field names per the
	// Phase 6.5 schema alignment for issue #390. The wire error
	// envelope keeps its flat REST-style keys (`class`, `boundary`,
	// …) per ADR-0010; the recorder translates between the two
	// surfaces at write time. `details` stays nested for now —
	// flattening into indexed attributes is deferred to Phase 7.
	payload := map[string]any{}
	if f != nil {
		payload["aileron.failure.class"] = string(f.Class())
		payload["aileron.failure.boundary"] = string(f.Boundary())
		payload["aileron.failure.retriable"] = f.Retriable()
		payload["aileron.failure.message"] = f.Message()
		if d := f.Details(); len(d) > 0 {
			payload["aileron.failure.details"] = d
		}
	}
	_ = r.store.Append(ctx, Event{
		EventID:   id,
		EventType: model.EventTypeExecutionFailed,
		Actor:     actor,
		Payload:   payload,
		Timestamp: r.clk.Now(),
	})
	return id
}

// RecordSuccess appends a non-failure event with the given type. The
// store append error is intentionally swallowed: per ADR-0010 these are
// best-effort action-audit writes that must not block the response they
// accompany. Callers that need the persistence outcome use RecordEvent.
func (r *recorder) RecordSuccess(ctx context.Context, eventType model.EventType, actor model.ActorRef, payload map[string]any) string {
	id, _ := r.RecordEvent(ctx, eventType, actor, payload)
	return id
}

// RecordEvent mints an id, appends the event, and returns the id together
// with the store append error so the ingest path can surface a durable
// failure instead of acknowledging an event that never committed.
func (r *recorder) RecordEvent(ctx context.Context, eventType model.EventType, actor model.ActorRef, payload map[string]any) (string, error) {
	id := r.idFn()
	err := r.store.Append(ctx, Event{
		EventID:   id,
		EventType: eventType,
		Actor:     actor,
		Payload:   payload,
		Timestamp: r.clk.Now(),
	})
	return id, err
}
