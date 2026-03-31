package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// PolicyStore is a thread-safe in-memory implementation of store.PolicyStore.
type PolicyStore struct {
	mu       sync.RWMutex
	policies map[string]api.Policy
}

// NewPolicyStore returns an empty in-memory policy store.
func NewPolicyStore() *PolicyStore {
	return &PolicyStore{policies: make(map[string]api.Policy)}
}

func (s *PolicyStore) Create(_ context.Context, policy api.Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[policy.PolicyId] = policy
	return nil
}

func (s *PolicyStore) Get(_ context.Context, policyID string) (api.Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[policyID]
	if !ok {
		return api.Policy{}, &store.ErrNotFound{Entity: "policy", ID: policyID}
	}
	return p, nil
}

func (s *PolicyStore) List(_ context.Context, filter store.PolicyFilter) ([]api.Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.Policy
	for _, p := range s.policies {
		if filter.WorkspaceID != "" && p.WorkspaceId != filter.WorkspaceID {
			continue
		}
		if filter.Status != nil && p.Status != *filter.Status {
			continue
		}
		result = append(result, p)
	}
	return result, nil
}

func (s *PolicyStore) Update(_ context.Context, policy api.Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policies[policy.PolicyId]; !ok {
		return &store.ErrNotFound{Entity: "policy", ID: policy.PolicyId}
	}
	s.policies[policy.PolicyId] = policy
	return nil
}
