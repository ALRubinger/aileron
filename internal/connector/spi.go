// Package connector defines the SPI for execution connectors.
//
// A connector bridges the control plane to an external system — a payment
// processor, a git provider, a calendar service, etc. When the policy engine
// allows an action (or a human approves it), the control plane selects the
// appropriate connector and calls Execute.
//
// Each connector implementation is responsible for one external system.
// The SPI is intentionally narrow: connectors receive a bounded execution
// request and return a result. Credential injection and parameter bounding
// happen in the control plane before the connector is called.
package connector

import (
	"context"
	"sync"
)

// Connector executes an approved action against an external system.
type Connector interface {
	// Type returns the connector type identifier (e.g. "payments", "git",
	// "calendar"). Used by the registry to route execution requests.
	Type() string

	// Provider returns the specific provider name (e.g. "stripe", "github",
	// "google_calendar"). Multiple connectors may share a Type.
	Provider() string

	// Execute runs the action described by the request.
	Execute(ctx context.Context, req ExecutionRequest) (ExecutionResult, error)
}

// ExecutionRequest is passed to a connector when executing an approved action.
type ExecutionRequest struct {
	GrantID     string
	IntentID    string
	ConnectorID string
	// ActionType is the dot-namespaced action, e.g. "payment.charge".
	ActionType string
	// Parameters are the bounded, approved parameters for execution.
	// These are constrained by the execution grant and may differ from
	// the original intent if a human modified them during approval.
	Parameters map[string]any
	// Credential is the injected secret for this connector, resolved from
	// the vault. Connectors must not store or log this value.
	Credential *InjectedCredential
}

// InjectedCredential carries a resolved secret for use during execution.
// Connectors receive this transiently and must not persist it.
type InjectedCredential struct {
	Type  string
	Value []byte
}

// ExecutionResult is returned by a connector after execution.
type ExecutionResult struct {
	Status     ExecutionStatus
	Output     map[string]any
	ReceiptRef string
	Error      string
}

// ExecutionStatus is the outcome of a connector execution.
type ExecutionStatus string

const (
	ExecutionStatusSucceeded ExecutionStatus = "succeeded"
	ExecutionStatusFailed    ExecutionStatus = "failed"
	ExecutionStatusCancelled ExecutionStatus = "cancelled"
)

// Registry holds the registered connectors and routes execution requests.
// It is safe for concurrent use.
type Registry struct {
	mu         sync.RWMutex
	connectors map[string]Connector
}

// NewRegistry returns an empty connector registry.
func NewRegistry() *Registry {
	return &Registry{connectors: make(map[string]Connector)}
}

// Register adds a connector to the registry.
// The key is "<type>/<provider>", e.g. "payments/stripe".
func (r *Registry) Register(ctx context.Context, c Connector) {
	_ = ctx // available for future database-backed implementations
	key := c.Type() + "/" + c.Provider()
	r.mu.Lock()
	r.connectors[key] = c
	r.mu.Unlock()
}

// Get returns the connector for the given type and provider.
func (r *Registry) Get(ctx context.Context, connectorType, provider string) (Connector, bool) {
	_ = ctx // available for future database-backed implementations
	r.mu.RLock()
	c, ok := r.connectors[connectorType+"/"+provider]
	r.mu.RUnlock()
	return c, ok
}
