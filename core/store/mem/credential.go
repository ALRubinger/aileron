package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// CredentialStore is a thread-safe in-memory implementation of store.CredentialStore.
type CredentialStore struct {
	mu          sync.RWMutex
	credentials map[string]api.CredentialReference
}

// NewCredentialStore returns an empty in-memory credential store.
func NewCredentialStore() *CredentialStore {
	return &CredentialStore{credentials: make(map[string]api.CredentialReference)}
}

func (s *CredentialStore) Create(_ context.Context, cred api.CredentialReference) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.credentials[cred.CredentialId] = cred
	return nil
}

func (s *CredentialStore) Get(_ context.Context, credentialID string) (api.CredentialReference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.credentials[credentialID]
	if !ok {
		return api.CredentialReference{}, &store.ErrNotFound{Entity: "credential", ID: credentialID}
	}
	return c, nil
}

func (s *CredentialStore) List(_ context.Context, filter store.CredentialFilter) ([]api.CredentialReference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.CredentialReference
	for _, c := range s.credentials {
		if filter.WorkspaceID != "" && c.WorkspaceId != filter.WorkspaceID {
			continue
		}
		result = append(result, c)
	}
	return result, nil
}
