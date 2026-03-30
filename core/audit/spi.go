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

	"github.com/ALRubinger/aileron/core/model"
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
