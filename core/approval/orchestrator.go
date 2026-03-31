package approval

import (
	"context"
	"fmt"
	"time"

	"github.com/ALRubinger/aileron/core/store"
)

// defaultGrantTTL is how long an execution grant remains active after approval.
const defaultGrantTTL = 5 * time.Minute

// defaultApprovalTTL is how long an approval request remains pending.
const defaultApprovalTTL = 1 * time.Hour

// InMemoryOrchestrator implements Orchestrator using in-memory stores.
type InMemoryOrchestrator struct {
	approvals store.ApprovalStore
	newID     func() string // ID generator
}

// NewInMemoryOrchestrator returns an orchestrator backed by the given store.
// The idGen function should return unique IDs (e.g., UUIDs).
func NewInMemoryOrchestrator(approvals store.ApprovalStore, idGen func() string) *InMemoryOrchestrator {
	return &InMemoryOrchestrator{
		approvals: approvals,
		newID:     idGen,
	}
}

func (o *InMemoryOrchestrator) Request(ctx context.Context, req ApprovalRequest) (Approval, error) {
	now := time.Now().UTC()

	expiresAt := now.Add(defaultApprovalTTL)
	if req.ExpiresAt != nil {
		expiresAt = *req.ExpiresAt
	}

	var actors []ApproverActor
	for _, ref := range req.Approvers {
		actors = append(actors, ApproverActor{
			PrincipalID: ref.PrincipalID,
			DisplayName: ref.DisplayName,
			Role:        ref.Role,
			Status:      ActorStatusPending,
		})
	}
	// Default approver if none specified.
	if len(actors) == 0 {
		actors = []ApproverActor{
			{
				PrincipalID: "default-owner",
				DisplayName: "Workspace Owner",
				Role:        "owner",
				Status:      ActorStatusPending,
			},
		}
	}

	approval := Approval{
		ApprovalID:     "apr_" + o.newID(),
		IntentID:       req.IntentID,
		WorkspaceID:    req.WorkspaceID,
		Status:         StatusPending,
		Rationale:      req.Rationale,
		Approvers:      actors,
		EditableBounds: req.EditableBounds,
		ExpiresAt:      &expiresAt,
		RequestedAt:    now,
	}

	// Convert to API type for storage.
	apiApproval := toAPIApproval(approval)
	if err := o.approvals.Create(ctx, apiApproval); err != nil {
		return Approval{}, fmt.Errorf("approval: create: %w", err)
	}

	return approval, nil
}

func (o *InMemoryOrchestrator) Get(ctx context.Context, approvalID string) (Approval, error) {
	apiApproval, err := o.approvals.Get(ctx, approvalID)
	if err != nil {
		return Approval{}, err
	}
	return fromAPIApproval(apiApproval), nil
}

func (o *InMemoryOrchestrator) Approve(ctx context.Context, approvalID string, req ApproveRequest) (Approval, error) {
	apiApproval, err := o.approvals.Get(ctx, approvalID)
	if err != nil {
		return Approval{}, err
	}

	approval := fromAPIApproval(apiApproval)
	if approval.Status != StatusPending {
		return Approval{}, fmt.Errorf("approval: cannot approve %s in state %s", approvalID, approval.Status)
	}

	now := time.Now().UTC()
	approval.Status = StatusApproved
	approval.ResolvedAt = &now

	// Mark all actors as approved.
	for i := range approval.Approvers {
		approval.Approvers[i].Status = ActorStatusApproved
	}

	if err := o.approvals.Update(ctx, toAPIApproval(approval)); err != nil {
		return Approval{}, fmt.Errorf("approval: update: %w", err)
	}

	return approval, nil
}

func (o *InMemoryOrchestrator) Deny(ctx context.Context, approvalID string, req DenyRequest) (Approval, error) {
	apiApproval, err := o.approvals.Get(ctx, approvalID)
	if err != nil {
		return Approval{}, err
	}

	approval := fromAPIApproval(apiApproval)
	if approval.Status != StatusPending {
		return Approval{}, fmt.Errorf("approval: cannot deny %s in state %s", approvalID, approval.Status)
	}

	now := time.Now().UTC()
	approval.Status = StatusDenied
	approval.ResolvedAt = &now

	for i := range approval.Approvers {
		approval.Approvers[i].Status = ActorStatusDenied
	}

	if err := o.approvals.Update(ctx, toAPIApproval(approval)); err != nil {
		return Approval{}, fmt.Errorf("approval: update: %w", err)
	}

	return approval, nil
}

func (o *InMemoryOrchestrator) Modify(ctx context.Context, approvalID string, req ModifyRequest) (Approval, error) {
	apiApproval, err := o.approvals.Get(ctx, approvalID)
	if err != nil {
		return Approval{}, err
	}

	approval := fromAPIApproval(apiApproval)
	if approval.Status != StatusPending {
		return Approval{}, fmt.Errorf("approval: cannot modify %s in state %s", approvalID, approval.Status)
	}

	now := time.Now().UTC()
	approval.Status = StatusModified
	approval.ResolvedAt = &now

	for i := range approval.Approvers {
		approval.Approvers[i].Status = ActorStatusApproved
	}

	if err := o.approvals.Update(ctx, toAPIApproval(approval)); err != nil {
		return Approval{}, fmt.Errorf("approval: update: %w", err)
	}

	return approval, nil
}

func (o *InMemoryOrchestrator) List(ctx context.Context, filter ListFilter) ([]Approval, error) {
	apiFilter := store.ApprovalFilter{
		WorkspaceID: filter.WorkspaceID,
		PageSize:    filter.PageSize,
		PageToken:   filter.PageToken,
	}
	if filter.Status != "" {
		s := statusToAPIStatus(filter.Status)
		apiFilter.Status = &s
	}

	apiApprovals, err := o.approvals.List(ctx, apiFilter)
	if err != nil {
		return nil, err
	}

	var result []Approval
	for _, a := range apiApprovals {
		result = append(result, fromAPIApproval(a))
	}
	return result, nil
}
