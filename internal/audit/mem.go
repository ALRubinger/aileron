package audit

import (
	"context"
	"sort"
	"sync"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

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
