package mem

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// LLMConfigStore is a thread-safe in-memory implementation of store.LLMConfigStore.
type LLMConfigStore struct {
	mu      sync.RWMutex
	configs map[string]model.LLMConfig
}

// NewLLMConfigStore returns an empty in-memory LLM config store.
func NewLLMConfigStore() *LLMConfigStore {
	return &LLMConfigStore{configs: make(map[string]model.LLMConfig)}
}

func (s *LLMConfigStore) Upsert(_ context.Context, cfg model.LLMConfig) (model.LLMConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if a config already exists for this owner.
	for id, existing := range s.configs {
		if existing.OwnerType == cfg.OwnerType && existing.OwnerID == cfg.OwnerID {
			cfg.ID = id
			cfg.CreatedAt = existing.CreatedAt
			s.configs[id] = cfg
			return cfg, nil
		}
	}

	s.configs[cfg.ID] = cfg
	return cfg, nil
}

func (s *LLMConfigStore) GetByOwner(_ context.Context, ownerType model.LLMConfigOwnerType, ownerID string) (model.LLMConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, cfg := range s.configs {
		if cfg.OwnerType == ownerType && cfg.OwnerID == ownerID {
			return cfg, nil
		}
	}
	return model.LLMConfig{}, &store.ErrNotFound{Entity: "llm_config", ID: string(ownerType) + "/" + ownerID}
}

func (s *LLMConfigStore) Delete(_ context.Context, configID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.configs[configID]; !ok {
		return &store.ErrNotFound{Entity: "llm_config", ID: configID}
	}
	delete(s.configs, configID)
	return nil
}
