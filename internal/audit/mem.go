package audit

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

// EventFilter scopes a flat-event list query against the audit store
// (as opposed to [Filter], which scopes a trace-grouped query). The
// filter is the on-the-wire shape for `GET /v1/audit`: every field
// is optional, and they compose with AND semantics.
type EventFilter struct {
	// Since is an inclusive lower bound on Event.Timestamp. Zero
	// disables the bound.
	Since time.Time
	// EventID, when non-empty, restricts the result to the single
	// event with this id (or empty when no such event exists).
	EventID string
	// ConnectorFQN, when non-empty, matches events whose payload
	// references this FQN under any of the recorder's connector
	// keys: `aileron.connector.fqn` (binding lifecycle and
	// connector install events), `aileron.action.fqn` (action
	// install events — when filtering by the action's own FQN),
	// or `aileron.failure.details.connector` (failure events).
	ConnectorFQN string
	// Class, when non-empty, matches events whose payload carries
	// this failure class. Has no effect on success events (they
	// don't record a class), so a Class filter naturally narrows
	// to failures.
	Class string
	// OutputName, when non-empty, matches `output.materialized`
	// events whose payload carries this artifact name under
	// `aileron.output.name`. Has no effect on other event types
	// (they don't record an output name), so it naturally narrows
	// to materialized outputs.
	OutputName string
	// ContentHash, when non-empty, matches the `output.materialized`
	// event whose payload carries this content hash under
	// `aileron.output.content_hash` (a full `sha256:<hex>` digest).
	// Matching is exact string equality.
	ContentHash string
	// Limit caps the number of events returned. <=0 means no cap.
	Limit int
}

// ListEvents returns a flat slice of recorded events matching the
// filter, ordered newest-first. The store is in-memory and the scan
// is linear; suitable for v1 dev/test volumes.
func (m *MemStore) ListEvents(_ context.Context, f EventFilter) ([]Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Event, 0, len(m.events))
	for _, e := range m.events {
		if !matchEventFilter(e.event, f) {
			continue
		}
		out = append(out, e.event)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// matchEventFilter returns true when ev passes every non-zero field
// of f. Connector matching deliberately walks the three keys the
// recorder uses today; if a future event type adds another connector
// key, extend this helper rather than the caller.
func matchEventFilter(ev Event, f EventFilter) bool {
	if !f.Since.IsZero() && ev.Timestamp.Before(f.Since) {
		return false
	}
	if f.EventID != "" && ev.EventID != f.EventID {
		return false
	}
	if f.Class != "" {
		got, _ := ev.Payload["aileron.failure.class"].(string)
		if got != f.Class {
			return false
		}
	}
	if f.ConnectorFQN != "" && !payloadMatchesConnector(ev.Payload, f.ConnectorFQN) {
		return false
	}
	if f.OutputName != "" {
		got, _ := ev.Payload["aileron.output.name"].(string)
		if got != f.OutputName {
			return false
		}
	}
	if f.ContentHash != "" {
		got, _ := ev.Payload["aileron.output.content_hash"].(string)
		if got != f.ContentHash {
			return false
		}
	}
	return true
}

// payloadMatchesConnector returns true when the payload carries the
// given connector FQN under any of the namespaced keys the recorder
// uses today:
//   - `aileron.connector.fqn` (binding lifecycle, connector install)
//   - `aileron.action.fqn`    (action install — matches when the user
//     filters by the action's own FQN, since actions are addressed
//     under the same FQN namespace as connectors)
//   - `aileron.failure.details.connector` (failure events that
//     surface a connector identity in nested details)
func payloadMatchesConnector(p map[string]any, fqn string) bool {
	if got, _ := p["aileron.connector.fqn"].(string); got == fqn {
		return true
	}
	if got, _ := p["aileron.action.fqn"].(string); got == fqn {
		return true
	}
	if details, ok := p["aileron.failure.details"].(map[string]any); ok {
		if got, _ := details["connector"].(string); got == fqn {
			return true
		}
	}
	return false
}

// MemStore is an in-memory implementation of the [Store] SPI suitable
// for v1 dev/test environments. Events are kept in a single ordered
// slice; lookups are filtered linearly. Trace IDs come from the
// caller — typically the audit_id of an envelope or an upstream
// trace correlation ID — and are stored alongside the event.
//
// Persistence to Postgres is post-MVP; the SPI is identical so the
// switch is a wiring change.
type MemStore struct {
	mu     sync.RWMutex
	events []eventWithTrace
}

// eventWithTrace carries the optional trace correlation ID alongside
// the SPI's Event. The SPI's Event has no TraceID field, so we keep
// it next to the event in this storage layer.
type eventWithTrace struct {
	traceID     string
	intentID    string
	workspaceID string
	event       Event
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{} }

// Append records a new event.
func (m *MemStore) Append(_ context.Context, event Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, eventWithTrace{event: event})
	return nil
}

// AppendToTrace records an event scoped to a specific trace
// (intent/workspace). The base [Store] interface deliberately keeps
// trace correlation out of the Event struct; this helper is the
// canonical way to associate an event with a trace.
func (m *MemStore) AppendToTrace(_ context.Context, traceID, intentID, workspaceID string, event Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, eventWithTrace{
		traceID:     traceID,
		intentID:    intentID,
		workspaceID: workspaceID,
		event:       event,
	})
	return nil
}

// GetByEventID returns the recorded event with the given EventID and
// reports whether one was found. Useful for tests asserting that an
// audit_id stamped on a [failure.Failure] resolves to a real event.
func (m *MemStore) GetByEventID(eventID string) (Event, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.events {
		if e.event.EventID == eventID {
			return e.event, true
		}
	}
	return Event{}, false
}

// ListTraces returns traces matching the filter. Events without a
// trace ID are grouped under TraceID=="" and surface only when the
// filter is permissive.
func (m *MemStore) ListTraces(_ context.Context, filter Filter) ([]Trace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byTrace := map[string]*Trace{}
	for _, e := range m.events {
		if !matchFilter(e, filter) {
			continue
		}
		t, ok := byTrace[e.traceID]
		if !ok {
			t = &Trace{
				TraceID:     e.traceID,
				IntentID:    e.intentID,
				WorkspaceID: e.workspaceID,
			}
			byTrace[e.traceID] = t
		}
		t.Events = append(t.Events, e.event)
	}
	out := make([]Trace, 0, len(byTrace))
	for _, t := range byTrace {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TraceID < out[j].TraceID })
	return out, nil
}

// GetTrace returns the trace with the given TraceID.
func (m *MemStore) GetTrace(_ context.Context, traceID string) (Trace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t := Trace{TraceID: traceID}
	found := false
	for _, e := range m.events {
		if e.traceID != traceID {
			continue
		}
		found = true
		t.IntentID = e.intentID
		t.WorkspaceID = e.workspaceID
		t.Events = append(t.Events, e.event)
	}
	if !found {
		return Trace{}, &store.ErrNotFound{Entity: "trace", ID: traceID}
	}
	return t, nil
}

func matchFilter(e eventWithTrace, f Filter) bool {
	if f.WorkspaceID != "" && e.workspaceID != f.WorkspaceID {
		return false
	}
	if f.IntentID != "" && e.intentID != f.IntentID {
		return false
	}
	if f.ActorID != "" && e.event.Actor.ID != f.ActorID {
		return false
	}
	if f.EventType != "" && e.event.EventType != f.EventType {
		return false
	}
	if f.From != nil && e.event.Timestamp.Before(*f.From) {
		return false
	}
	if f.To != nil && e.event.Timestamp.After(*f.To) {
		return false
	}
	return true
}

// Compile-time check that MemStore satisfies the SPI.
var _ Store = (*MemStore)(nil)

// helperEnsureModelImported is a compile-time hint that pulls in
// model so the package imports stay tidy when other helpers refer
// to model types via interface satisfaction.
var _ model.ActorRef
