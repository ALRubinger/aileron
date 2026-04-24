package mem

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

// DraftFeedbackStore is a thread-safe in-memory implementation of store.DraftFeedbackStore.
type DraftFeedbackStore struct {
	mu       sync.RWMutex
	feedback []model.DraftFeedback
}

// NewDraftFeedbackStore returns an empty in-memory feedback store.
func NewDraftFeedbackStore() *DraftFeedbackStore {
	return &DraftFeedbackStore{}
}

func (s *DraftFeedbackStore) Create(_ context.Context, feedback model.DraftFeedback) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.feedback = append(s.feedback, feedback)
	return nil
}

func (s *DraftFeedbackStore) List(_ context.Context, filter store.DraftFeedbackFilter) ([]model.DraftFeedback, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []model.DraftFeedback
	for _, fb := range s.feedback {
		if filter.UserID != "" && fb.UserID != filter.UserID {
			continue
		}
		if filter.Signal != nil && fb.Signal != *filter.Signal {
			continue
		}
		result = append(result, fb)
	}
	return result, nil
}
