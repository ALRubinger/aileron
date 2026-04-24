package approval

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

func newTestOrchestrator() *InMemoryOrchestrator {
	store := newTestApprovalStore()
	counter := atomic.Int64{}
	idGen := func() string {
		return fmt.Sprintf("test_%d", counter.Add(1))
	}
	return NewInMemoryOrchestrator(store, idGen)
}

func TestOrchestrator_RequestCreatesApproval(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	apr, err := o.Request(ctx, ApprovalRequest{
		IntentID:    "int_1",
		WorkspaceID: "ws_1",
		Rationale:   "Policy requires approval for PRs to main",
		Approvers: []ApproverRef{
			{PrincipalID: "user_1", DisplayName: "Alice", Role: "owner"},
		},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	if apr.ApprovalID == "" {
		t.Error("ApprovalID should not be empty")
	}
	if apr.Status != StatusPending {
		t.Errorf("Status = %q, want %q", apr.Status, StatusPending)
	}
	if apr.IntentID != "int_1" {
		t.Errorf("IntentID = %q, want %q", apr.IntentID, "int_1")
	}
	if len(apr.Approvers) != 1 {
		t.Errorf("Approvers = %d, want 1", len(apr.Approvers))
	}
	if apr.ExpiresAt == nil {
		t.Error("ExpiresAt should be set")
	}
}

func TestOrchestrator_RequestDefaultApprover(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	apr, err := o.Request(ctx, ApprovalRequest{
		IntentID:    "int_1",
		WorkspaceID: "ws_1",
		Rationale:   "Needs approval",
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(apr.Approvers) != 1 {
		t.Fatalf("Approvers = %d, want 1", len(apr.Approvers))
	}
	if apr.Approvers[0].PrincipalID != "default-owner" {
		t.Errorf("Approvers[0].PrincipalID = %q, want %q", apr.Approvers[0].PrincipalID, "default-owner")
	}
}

func TestOrchestrator_ApproveFlipsStatus(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	apr, _ := o.Request(ctx, ApprovalRequest{
		IntentID: "int_1", WorkspaceID: "ws_1", Rationale: "test",
	})

	approved, err := o.Approve(ctx, apr.ApprovalID, ApproveRequest{Comment: "lgtm"})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != StatusApproved {
		t.Errorf("Status = %q, want %q", approved.Status, StatusApproved)
	}
	if approved.ResolvedAt == nil {
		t.Error("ResolvedAt should be set")
	}
	for _, a := range approved.Approvers {
		if a.Status != ActorStatusApproved {
			t.Errorf("Approver status = %q, want %q", a.Status, ActorStatusApproved)
		}
	}
}

func TestOrchestrator_DenyFlipsStatus(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	apr, _ := o.Request(ctx, ApprovalRequest{
		IntentID: "int_1", WorkspaceID: "ws_1", Rationale: "test",
	})

	denied, err := o.Deny(ctx, apr.ApprovalID, DenyRequest{Reason: "not now"})
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if denied.Status != StatusDenied {
		t.Errorf("Status = %q, want %q", denied.Status, StatusDenied)
	}
	if denied.ResolvedAt == nil {
		t.Error("ResolvedAt should be set")
	}
}

func TestOrchestrator_CannotApproveResolved(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	apr, _ := o.Request(ctx, ApprovalRequest{
		IntentID: "int_1", WorkspaceID: "ws_1", Rationale: "test",
	})
	o.Approve(ctx, apr.ApprovalID, ApproveRequest{})

	// Try to approve again.
	_, err := o.Approve(ctx, apr.ApprovalID, ApproveRequest{})
	if err == nil {
		t.Fatal("expected error when approving already-approved request")
	}
}

func TestOrchestrator_CannotDenyResolved(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	apr, _ := o.Request(ctx, ApprovalRequest{
		IntentID: "int_1", WorkspaceID: "ws_1", Rationale: "test",
	})
	o.Deny(ctx, apr.ApprovalID, DenyRequest{Reason: "no"})

	_, err := o.Deny(ctx, apr.ApprovalID, DenyRequest{Reason: "no again"})
	if err == nil {
		t.Fatal("expected error when denying already-denied request")
	}
}

func TestOrchestrator_ModifyFlipsStatus(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	apr, _ := o.Request(ctx, ApprovalRequest{
		IntentID: "int_1", WorkspaceID: "ws_1", Rationale: "test",
	})

	modified, err := o.Modify(ctx, apr.ApprovalID, ModifyRequest{
		Modifications: map[string]any{"amount": 5000},
	})
	if err != nil {
		t.Fatalf("Modify: %v", err)
	}
	if modified.Status != StatusModified {
		t.Errorf("Status = %q, want %q", modified.Status, StatusModified)
	}
}

func TestOrchestrator_ListPending(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	o.Request(ctx, ApprovalRequest{IntentID: "int_1", WorkspaceID: "ws_1", Rationale: "r1"})
	apr2, _ := o.Request(ctx, ApprovalRequest{IntentID: "int_2", WorkspaceID: "ws_1", Rationale: "r2"})
	o.Request(ctx, ApprovalRequest{IntentID: "int_3", WorkspaceID: "ws_1", Rationale: "r3"})
	o.Approve(ctx, apr2.ApprovalID, ApproveRequest{})

	pending, err := o.List(ctx, ListFilter{Status: StatusPending})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("List(pending): got %d, want 2", len(pending))
	}
}

func TestOrchestrator_Get(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	apr, _ := o.Request(ctx, ApprovalRequest{
		IntentID: "int_1", WorkspaceID: "ws_1", Rationale: "test",
	})

	got, err := o.Get(ctx, apr.ApprovalID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ApprovalID != apr.ApprovalID {
		t.Errorf("ApprovalID = %q, want %q", got.ApprovalID, apr.ApprovalID)
	}
}
