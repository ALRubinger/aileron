package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// GrantStore is a thread-safe in-memory implementation of store.GrantStore.
type GrantStore struct {
	mu     sync.RWMutex
	grants map[string]api.ExecutionGrant
}

// NewGrantStore returns an empty in-memory grant store.
func NewGrantStore() *GrantStore {
	return &GrantStore{grants: make(map[string]api.ExecutionGrant)}
}

func (s *GrantStore) Create(_ context.Context, grant api.ExecutionGrant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[grant.GrantId] = grant
	return nil
}

func (s *GrantStore) Get(_ context.Context, grantID string) (api.ExecutionGrant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.grants[grantID]
	if !ok {
		return api.ExecutionGrant{}, &store.ErrNotFound{Entity: "grant", ID: grantID}
	}
	return g, nil
}

func (s *GrantStore) Update(_ context.Context, grant api.ExecutionGrant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.grants[grant.GrantId]; !ok {
		return &store.ErrNotFound{Entity: "grant", ID: grant.GrantId}
	}
	s.grants[grant.GrantId] = grant
	return nil
}
