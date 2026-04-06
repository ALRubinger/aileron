package mem

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// UserKeyMaterialStore is a thread-safe in-memory implementation of store.UserKeyMaterialStore.
type UserKeyMaterialStore struct {
	mu        sync.RWMutex
	materials map[string]model.UserKeyMaterial // keyed by user ID
}

// NewUserKeyMaterialStore returns an empty in-memory user key material store.
func NewUserKeyMaterialStore() *UserKeyMaterialStore {
	return &UserKeyMaterialStore{materials: make(map[string]model.UserKeyMaterial)}
}

func (s *UserKeyMaterialStore) Create(_ context.Context, material model.UserKeyMaterial) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.materials[material.UserID] = material
	return nil
}

func (s *UserKeyMaterialStore) Get(_ context.Context, userID string) (model.UserKeyMaterial, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.materials[userID]
	if !ok {
		return model.UserKeyMaterial{}, &store.ErrNotFound{Entity: "user_key_material", ID: userID}
	}
	return m, nil
}

func (s *UserKeyMaterialStore) Update(_ context.Context, material model.UserKeyMaterial) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.materials[material.UserID]; !ok {
		return &store.ErrNotFound{Entity: "user_key_material", ID: material.UserID}
	}
	s.materials[material.UserID] = material
	return nil
}
