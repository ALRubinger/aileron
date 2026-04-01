package main

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/core/audit"
)

// memAuditStore is a minimal in-memory implementation of audit.Store
// for use in the gateway's embedded mode.
type memAuditStore struct {
	mu     sync.RWMutex
	events []audit.Event
}

// newMemAuditStore returns an empty in-memory audit store.
func newMemAuditStore() *memAuditStore {
	return &memAuditStore{}
}

// Append records a new event.
func (s *memAuditStore) Append(_ context.Context, event audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

// ListTraces returns an empty list; trace aggregation is not needed in the
// gateway's embedded audit store.
func (s *memAuditStore) ListTraces(_ context.Context, _ audit.Filter) ([]audit.Trace, error) {
	return nil, nil
}

// GetTrace returns an empty trace; trace retrieval is not needed in the
// gateway's embedded audit store.
func (s *memAuditStore) GetTrace(_ context.Context, _ string) (audit.Trace, error) {
	return audit.Trace{}, nil
}
