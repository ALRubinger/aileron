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
	EventType EventType
	Actor     ActorRef
	// Payload carries event-specific data. Must be serialisable to JSON.
	Payload   map[string]any
	Timestamp time.Time
}

// ActorRef identifies who or what produced the event.
type ActorRef struct {
	ID          string
	Type        ActorType
	DisplayName string
}

// ActorType classifies the event producer.
type ActorType string

const (
	ActorTypeAgent            ActorType = "agent"
	ActorTypeHuman            ActorType = "human"
	ActorTypeService          ActorType = "service"
	ActorTypeConnectorRuntime ActorType = "connector_runtime"
)

// EventType identifies the kind of event.
type EventType string

const (
	EventTypeIntentSubmitted    EventType = "intent.submitted"
	EventTypePolicyEvaluated    EventType = "policy.evaluated"
	EventTypeApprovalRequested  EventType = "approval.requested"
	EventTypeApprovalApproved   EventType = "approval.approved"
	EventTypeApprovalDenied     EventType = "approval.denied"
	EventTypeApprovalModified   EventType = "approval.modified"
	EventTypeGrantIssued        EventType = "grant.issued"
	EventTypeGrantRevoked       EventType = "grant.revoked"
	EventTypeExecutionStarted   EventType = "execution.started"
	EventTypeExecutionSucceeded EventType = "execution.succeeded"
	EventTypeExecutionFailed    EventType = "execution.failed"
)

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
	EventType   EventType
	From        *time.Time
	To          *time.Time
	PageSize    int
	PageToken   string
}
