package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// TraceStore is a thread-safe in-memory append-only trace store.
type TraceStore struct {
	mu     sync.RWMutex
	traces map[string]*api.Trace // keyed by intent_id
}

// NewTraceStore returns an empty in-memory trace store.
func NewTraceStore() *TraceStore {
	return &TraceStore{traces: make(map[string]*api.Trace)}
}

// Append adds an event to the trace for the given intent.
// If no trace exists, a new one is created.
func (s *TraceStore) Append(_ context.Context, intentID, workspaceID, traceID string, event api.TraceEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.traces[intentID]
	if !ok {
		wsID := workspaceID
		t = &api.Trace{
			TraceId:     traceID,
			IntentId:    intentID,
			WorkspaceId: &wsID,
			Events:      []api.TraceEvent{},
		}
		s.traces[intentID] = t
	}
	t.Events = append(t.Events, event)
	return nil
}

// GetByIntent returns the trace for a given intent ID.
func (s *TraceStore) GetByIntent(_ context.Context, intentID string) (api.Trace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.traces[intentID]
	if !ok {
		return api.Trace{}, &store.ErrNotFound{Entity: "trace", ID: intentID}
	}
	return *t, nil
}

// List returns all traces, optionally filtered.
func (s *TraceStore) List(_ context.Context, filter TraceFilter) ([]api.Trace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.Trace
	for _, t := range s.traces {
		if filter.WorkspaceID != "" {
			if t.WorkspaceId == nil || *t.WorkspaceId != filter.WorkspaceID {
				continue
			}
		}
		if filter.IntentID != "" && t.IntentId != filter.IntentID {
			continue
		}
		result = append(result, *t)
	}
	return result, nil
}

// TraceFilter scopes a trace list query.
type TraceFilter struct {
	WorkspaceID string
	IntentID    string
	PageSize    int
	PageToken   string
}
