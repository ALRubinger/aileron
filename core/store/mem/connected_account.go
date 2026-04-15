package mem

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// ConnectedAccountStore is a thread-safe in-memory implementation of store.ConnectedAccountStore.
type ConnectedAccountStore struct {
	mu       sync.RWMutex
	accounts map[string]model.ConnectedAccount
}

// NewConnectedAccountStore returns an empty in-memory connected account store.
func NewConnectedAccountStore() *ConnectedAccountStore {
	return &ConnectedAccountStore{accounts: make(map[string]model.ConnectedAccount)}
}

func (s *ConnectedAccountStore) Create(_ context.Context, account model.ConnectedAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[account.ID] = account
	return nil
}

func (s *ConnectedAccountStore) Get(_ context.Context, accountID string) (model.ConnectedAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return model.ConnectedAccount{}, &store.ErrNotFound{Entity: "connected_account", ID: accountID}
	}
	return acc, nil
}

func (s *ConnectedAccountStore) GetByUserAndProvider(_ context.Context, userID string, provider model.ConnectedAccountProvider) (model.ConnectedAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, acc := range s.accounts {
		if acc.UserID == userID && acc.Provider == provider {
			return acc, nil
		}
	}
	return model.ConnectedAccount{}, &store.ErrNotFound{Entity: "connected_account", ID: userID + "/" + string(provider)}
}

func (s *ConnectedAccountStore) List(_ context.Context, filter store.ConnectedAccountFilter) ([]model.ConnectedAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []model.ConnectedAccount
	for _, acc := range s.accounts {
		if filter.UserID != "" && acc.UserID != filter.UserID {
			continue
		}
		if filter.Provider != nil && acc.Provider != *filter.Provider {
			continue
		}
		if filter.Status != nil && acc.Status != *filter.Status {
			continue
		}
		if filter.ExternalUserID != "" && acc.ExternalUserID != filter.ExternalUserID {
			continue
		}
		if filter.ExternalTeamID != "" && acc.ExternalTeamID != filter.ExternalTeamID {
			continue
		}
		result = append(result, acc)
	}
	return result, nil
}

func (s *ConnectedAccountStore) Update(_ context.Context, account model.ConnectedAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[account.ID]; !ok {
		return &store.ErrNotFound{Entity: "connected_account", ID: account.ID}
	}
	s.accounts[account.ID] = account
	return nil
}

func (s *ConnectedAccountStore) Delete(_ context.Context, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[accountID]; !ok {
		return &store.ErrNotFound{Entity: "connected_account", ID: accountID}
	}
	delete(s.accounts, accountID)
	return nil
}
