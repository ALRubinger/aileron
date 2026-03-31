package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// ExecutionStore is a thread-safe in-memory implementation of store.ExecutionStore.
type ExecutionStore struct {
	mu         sync.RWMutex
	executions map[string]api.Execution
}

// NewExecutionStore returns an empty in-memory execution store.
func NewExecutionStore() *ExecutionStore {
	return &ExecutionStore{executions: make(map[string]api.Execution)}
}

func (s *ExecutionStore) Create(_ context.Context, exec api.Execution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executions[exec.ExecutionId] = exec
	return nil
}

func (s *ExecutionStore) Get(_ context.Context, executionID string) (api.Execution, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.executions[executionID]
	if !ok {
		return api.Execution{}, &store.ErrNotFound{Entity: "execution", ID: executionID}
	}
	return e, nil
}

func (s *ExecutionStore) Update(_ context.Context, exec api.Execution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.executions[exec.ExecutionId]; !ok {
		return &store.ErrNotFound{Entity: "execution", ID: exec.ExecutionId}
	}
	s.executions[exec.ExecutionId] = exec
	return nil
}
