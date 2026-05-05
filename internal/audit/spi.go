// Package audit defines the SPI for the immutable audit/trace store.
//
// Every significant event in the control plane lifecycle — intent submission,
// policy decision, approval action, execution outcome — is recorded as an
// append-only trace event. This provides a verifiable audit trail for
// compliance, debugging, and trust.
//
// The built-in implementation writes to a Postgres append-only table.
// Alternative implementations can target immutable object storage, a
// dedicated audit log service, or a blockchain-backed store.
package audit

import (
	"context"
	"time"

	"github.com/ALRubinger/aileron/internal/model"
)

// Store records and retrieves immutable trace events.
type Store interface {
	// Append records a new event. Implementations must treat this as
	// append-only: existing events must never be modified or deleted.
	Append(ctx context.Context, event Event) error

	// ListTraces returns traces matching the filter.
	ListTraces(ctx context.Context, filter Filter) ([]Trace, error)

	// GetTrace returns the full trace for a given intent.
	GetTrace(ctx context.Context, traceID string) (Trace, error)
}

// EventStore is the audit-store surface the daemon and the
// `/v1/audit` handlers depend on. It is the union of the trace-aware
// [Store] SPI and the flat-event API the read endpoint uses, so the
// in-memory and file-backed implementations are interchangeable at
// the wiring site in `internal/app/app.go`.
type EventStore interface {
	Store

	// AppendToTrace records an event scoped to a trace correlation
	// (intent/workspace).
	AppendToTrace(ctx context.Context, traceID, intentID, workspaceID string, event Event) error

	// GetByEventID returns the recorded event with this id and
	// reports whether one was found. Used by GET /v1/audit/{audit_id}.
	GetByEventID(eventID string) (Event, bool)

	// ListEvents returns flat events matching the filter, newest-first.
	// Used by GET /v1/audit.
	ListEvents(ctx context.Context, f EventFilter) ([]Event, error)
}

// Event is a single immutable audit record.
type Event struct {
	EventID   string
	EventType model.EventType
	Actor     model.ActorRef
	// Payload carries event-specific data. Must be serialisable to JSON.
	Payload   map[string]any
	Timestamp time.Time
}

// Trace is the full ordered event history for one intent.
type Trace struct {
	TraceID     string
	IntentID    string
	WorkspaceID string
	Events      []Event
}

// Filter scopes a trace list query.
type Filter struct {
	WorkspaceID string
	IntentID    string
	ActorID     string
	EventType   model.EventType
	From        *time.Time
	To          *time.Time
	PageSize    int
	PageToken   string
}
