package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// IntentStore is a thread-safe in-memory implementation of store.IntentStore.
type IntentStore struct {
	mu      sync.RWMutex
	intents map[string]api.IntentEnvelope
}

// NewIntentStore returns an empty in-memory intent store.
func NewIntentStore() *IntentStore {
	return &IntentStore{intents: make(map[string]api.IntentEnvelope)}
}

func (s *IntentStore) Create(_ context.Context, intent api.IntentEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents[intent.IntentId] = intent
	return nil
}

func (s *IntentStore) Get(_ context.Context, intentID string) (api.IntentEnvelope, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	intent, ok := s.intents[intentID]
	if !ok {
		return api.IntentEnvelope{}, &store.ErrNotFound{Entity: "intent", ID: intentID}
	}
	return intent, nil
}

func (s *IntentStore) List(_ context.Context, filter store.IntentFilter) ([]api.IntentEnvelope, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.IntentEnvelope
	for _, intent := range s.intents {
		if filter.WorkspaceID != "" && intent.WorkspaceId != filter.WorkspaceID {
			continue
		}
		if filter.Status != nil && intent.Status != *filter.Status {
			continue
		}
		if filter.AgentID != "" && intent.Agent.Id != filter.AgentID {
			continue
		}
		result = append(result, intent)
	}
	return result, nil
}

func (s *IntentStore) Update(_ context.Context, intent api.IntentEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.intents[intent.IntentId]; !ok {
		return &store.ErrNotFound{Entity: "intent", ID: intent.IntentId}
	}
	s.intents[intent.IntentId] = intent
	return nil
}
