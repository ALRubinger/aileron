package mem

import (
	"context"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

func TestApprovalStore_CreateAndGet(t *testing.T) {
	s := NewApprovalStore()
	ctx := context.Background()

	ws := "ws_1"
	approval := api.Approval{
		ApprovalId:  "apr_1",
		IntentId:    "int_1",
		WorkspaceId: &ws,
		Status:      api.ApprovalStatusPending,
		RequestedAt: time.Now().UTC(),
		Approvers: []api.ApprovalActor{
			{PrincipalId: "user_1"},
		},
	}

	if err := s.Create(ctx, approval); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "apr_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ApprovalId != "apr_1" {
		t.Errorf("ApprovalId = %q, want %q", got.ApprovalId, "apr_1")
	}
	if got.Status != api.ApprovalStatusPending {
		t.Errorf("Status = %q, want %q", got.Status, api.ApprovalStatusPending)
	}
}

func TestApprovalStore_GetNotFound(t *testing.T) {
	s := NewApprovalStore()
	_, err := s.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestApprovalStore_ListByStatus(t *testing.T) {
	s := NewApprovalStore()
	ctx := context.Background()
	ws := "ws_1"
	now := time.Now().UTC()

	s.Create(ctx, api.Approval{ApprovalId: "apr_1", IntentId: "int_1", WorkspaceId: &ws, Status: api.ApprovalStatusPending, RequestedAt: now})
	s.Create(ctx, api.Approval{ApprovalId: "apr_2", IntentId: "int_2", WorkspaceId: &ws, Status: api.ApprovalStatusApproved, RequestedAt: now})
	s.Create(ctx, api.Approval{ApprovalId: "apr_3", IntentId: "int_3", WorkspaceId: &ws, Status: api.ApprovalStatusPending, RequestedAt: now})

	pending := api.ApprovalStatusPending
	results, _ := s.List(ctx, store.ApprovalFilter{Status: &pending})
	if len(results) != 2 {
		t.Errorf("List(pending): got %d, want 2", len(results))
	}

	approved := api.ApprovalStatusApproved
	results, _ = s.List(ctx, store.ApprovalFilter{Status: &approved})
	if len(results) != 1 {
		t.Errorf("List(approved): got %d, want 1", len(results))
	}
}

func TestApprovalStore_Update(t *testing.T) {
	s := NewApprovalStore()
	ctx := context.Background()
	ws := "ws_1"

	s.Create(ctx, api.Approval{
		ApprovalId: "apr_1", IntentId: "int_1", WorkspaceId: &ws,
		Status: api.ApprovalStatusPending, RequestedAt: time.Now().UTC(),
	})

	a, _ := s.Get(ctx, "apr_1")
	a.Status = api.ApprovalStatusApproved
	now := time.Now().UTC()
	a.ResolvedAt = &now
	if err := s.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := s.Get(ctx, "apr_1")
	if got.Status != api.ApprovalStatusApproved {
		t.Errorf("Status = %q, want %q", got.Status, api.ApprovalStatusApproved)
	}
	if got.ResolvedAt == nil {
		t.Error("ResolvedAt should be set")
	}
}
