package mem

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

// UserStore is a thread-safe in-memory implementation of store.UserStore.
type UserStore struct {
	mu    sync.RWMutex
	users map[string]model.User
}

// NewUserStore returns an empty in-memory user store.
func NewUserStore() *UserStore {
	return &UserStore{users: make(map[string]model.User)}
}

func (s *UserStore) Create(_ context.Context, user model.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[user.ID] = user
	return nil
}

func (s *UserStore) Get(_ context.Context, userID string) (model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[userID]
	if !ok {
		return model.User{}, &store.ErrNotFound{Entity: "user", ID: userID}
	}
	return u, nil
}

func (s *UserStore) GetByEmail(_ context.Context, email string) (model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.Email == email {
			return u, nil
		}
	}
	return model.User{}, &store.ErrNotFound{Entity: "user", ID: email}
}

func (s *UserStore) List(_ context.Context, filter store.UserFilter) ([]model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []model.User
	for _, u := range s.users {
		if filter.EnterpriseID != "" && u.EnterpriseID != filter.EnterpriseID {
			continue
		}
		if filter.Status != nil && u.Status != *filter.Status {
			continue
		}
		result = append(result, u)
	}
	return result, nil
}

func (s *UserStore) Update(_ context.Context, user model.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[user.ID]; !ok {
		return &store.ErrNotFound{Entity: "user", ID: user.ID}
	}
	s.users[user.ID] = user
	return nil
}
