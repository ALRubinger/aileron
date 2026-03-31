package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// ApprovalStore is a thread-safe in-memory implementation of store.ApprovalStore.
type ApprovalStore struct {
	mu        sync.RWMutex
	approvals map[string]api.Approval
}

// NewApprovalStore returns an empty in-memory approval store.
func NewApprovalStore() *ApprovalStore {
	return &ApprovalStore{approvals: make(map[string]api.Approval)}
}

func (s *ApprovalStore) Create(_ context.Context, approval api.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvals[approval.ApprovalId] = approval
	return nil
}

func (s *ApprovalStore) Get(_ context.Context, approvalID string) (api.Approval, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.approvals[approvalID]
	if !ok {
		return api.Approval{}, &store.ErrNotFound{Entity: "approval", ID: approvalID}
	}
	return a, nil
}

func (s *ApprovalStore) List(_ context.Context, filter store.ApprovalFilter) ([]api.Approval, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.Approval
	for _, a := range s.approvals {
		if filter.WorkspaceID != "" {
			if a.WorkspaceId == nil || *a.WorkspaceId != filter.WorkspaceID {
				continue
			}
		}
		if filter.Status != nil && a.Status != *filter.Status {
			continue
		}
		if filter.IntentID != "" && a.IntentId != filter.IntentID {
			continue
		}
		result = append(result, a)
	}
	return result, nil
}

func (s *ApprovalStore) Update(_ context.Context, approval api.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.approvals[approval.ApprovalId]; !ok {
		return &store.ErrNotFound{Entity: "approval", ID: approval.ApprovalId}
	}
	s.approvals[approval.ApprovalId] = approval
	return nil
}
