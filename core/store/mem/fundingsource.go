package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// FundingSourceStore is a thread-safe in-memory implementation of store.FundingSourceStore.
type FundingSourceStore struct {
	mu      sync.RWMutex
	sources map[string]api.FundingSource
}

// NewFundingSourceStore returns an empty in-memory funding source store.
func NewFundingSourceStore() *FundingSourceStore {
	return &FundingSourceStore{sources: make(map[string]api.FundingSource)}
}

func (s *FundingSourceStore) Create(_ context.Context, fs api.FundingSource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources[fs.FundingSourceId] = fs
	return nil
}

func (s *FundingSourceStore) Get(_ context.Context, fundingSourceID string) (api.FundingSource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fs, ok := s.sources[fundingSourceID]
	if !ok {
		return api.FundingSource{}, &store.ErrNotFound{Entity: "funding_source", ID: fundingSourceID}
	}
	return fs, nil
}

func (s *FundingSourceStore) List(_ context.Context, filter store.FundingSourceFilter) ([]api.FundingSource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.FundingSource
	for _, fs := range s.sources {
		if filter.WorkspaceID != "" && fs.WorkspaceId != filter.WorkspaceID {
			continue
		}
		result = append(result, fs)
	}
	return result, nil
}
