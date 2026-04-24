package mem

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

// DraftStore is a thread-safe in-memory implementation of store.DraftStore.
type DraftStore struct {
	mu     sync.RWMutex
	drafts map[string]model.Draft
}

// NewDraftStore returns an empty in-memory draft store.
func NewDraftStore() *DraftStore {
	return &DraftStore{drafts: make(map[string]model.Draft)}
}

func (s *DraftStore) Create(_ context.Context, draft model.Draft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drafts[draft.ID] = draft
	return nil
}

func (s *DraftStore) Get(_ context.Context, draftID string) (model.Draft, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.drafts[draftID]
	if !ok {
		return model.Draft{}, &store.ErrNotFound{Entity: "draft", ID: draftID}
	}
	return d, nil
}

func (s *DraftStore) List(_ context.Context, filter store.DraftFilter) ([]model.Draft, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []model.Draft
	for _, d := range s.drafts {
		if filter.UserID != "" && d.UserID != filter.UserID {
			continue
		}
		if filter.Status != nil && d.Status != *filter.Status {
			continue
		}
		result = append(result, d)
	}
	return result, nil
}

func (s *DraftStore) Update(_ context.Context, draft model.Draft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.drafts[draft.ID]; !ok {
		return &store.ErrNotFound{Entity: "draft", ID: draft.ID}
	}
	s.drafts[draft.ID] = draft
	return nil
}
