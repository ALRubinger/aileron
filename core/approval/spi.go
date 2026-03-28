// Package approval defines the SPI for the approval orchestrator.
//
// The approval orchestrator manages the human-in-the-loop lifecycle:
// routing approval requests to the right people, tracking their responses,
// handling expiry, and issuing execution grants when approval is granted.
//
// The built-in implementation persists approvals in the database and
// dispatches notifications via the notify.Notifier SPI. Alternative
// implementations can integrate with external workflow systems.
package approval

import (
	"context"
	"time"
)

// Orchestrator manages the lifecycle of approval requests.
type Orchestrator interface {
	// Request creates a new approval request for the given intent.
	Request(ctx context.Context, req ApprovalRequest) (Approval, error)

	// Get returns an approval by ID.
	Get(ctx context.Context, approvalID string) (Approval, error)

	// Approve records an approval decision and issues an execution grant.
	Approve(ctx context.Context, approvalID string, req ApproveRequest) (Approval, error)

	// Deny records a denial and closes the intent.
	Deny(ctx context.Context, approvalID string, req DenyRequest) (Approval, error)

	// Modify approves with parameter constraints, bounding what the agent
	// may execute.
	Modify(ctx context.Context, approvalID string, req ModifyRequest) (Approval, error)

	// List returns pending approvals, optionally filtered by assignee.
	List(ctx context.Context, filter ListFilter) ([]Approval, error)
}

// ApprovalRequest is the input for creating a new approval.
type ApprovalRequest struct {
	IntentID    string
	WorkspaceID string
	// Rationale is the human-readable reason approval is required.
	Rationale string
	// Approvers is the list of principals who may act on this request.
	Approvers []ApproverRef
	ExpiresAt *time.Time
	// EditableBounds describes which parameters an approver may modify.
	EditableBounds map[string]any
}

// ApproverRef identifies a principal who may approve or deny.
type ApproverRef struct {
	PrincipalID string
	DisplayName string
	Role        string
}

// Approval is the full approval record.
type Approval struct {
	ApprovalID     string
	IntentID       string
	WorkspaceID    string
	Status         Status
	Rationale      string
	Approvers      []ApproverActor
	EditableBounds map[string]any
	ExpiresAt      *time.Time
	RequestedAt    time.Time
	ResolvedAt     *time.Time
}

// ApproverActor is an approver with their individual status.
type ApproverActor struct {
	PrincipalID string
	DisplayName string
	Role        string
	Status      ActorStatus
}

// Status is the overall state of an approval request.
type Status string

const (
	StatusPending   Status = "pending"
	StatusApproved  Status = "approved"
	StatusDenied    Status = "denied"
	StatusModified  Status = "modified"
	StatusDelegated Status = "delegated"
	StatusExpired   Status = "expired"
	StatusCancelled Status = "cancelled"
)

// ActorStatus is an individual approver's response.
type ActorStatus string

const (
	ActorStatusPending   ActorStatus = "pending"
	ActorStatusApproved  ActorStatus = "approved"
	ActorStatusDenied    ActorStatus = "denied"
	ActorStatusDelegated ActorStatus = "delegated"
)

// ApproveRequest is the input for an approval decision.
type ApproveRequest struct {
	Comment           string
	ApproveOnce       bool
	StepUpAuthAssertion string
}

// DenyRequest is the input for a denial decision.
type DenyRequest struct {
	Reason  string
	Comment string
}

// ModifyRequest approves with bounded parameter constraints.
type ModifyRequest struct {
	Comment       string
	Modifications map[string]any
}

// ListFilter scopes an approval list query.
type ListFilter struct {
	WorkspaceID string
	Assignee    string
	Status      Status
	PageSize    int
	PageToken   string
}
