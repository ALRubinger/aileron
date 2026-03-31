package mem

import (
	"context"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

func TestIntentStore_CreateAndGet(t *testing.T) {
	s := NewIntentStore()
	ctx := context.Background()

	intent := api.IntentEnvelope{
		IntentId:    "int_1",
		WorkspaceId: "ws_1",
		Agent:       api.ActorRef{Id: "agent_1", Type: api.Agent},
		Action:      api.ActionIntent{Type: "git.pull_request.create", Summary: "Create PR"},
		Status:      api.PendingPolicy,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Decision:    api.Decision{Disposition: api.DecisionDispositionAllow, RiskLevel: api.Low},
	}

	if err := s.Create(ctx, intent); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "int_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.IntentId != "int_1" {
		t.Errorf("IntentId = %q, want %q", got.IntentId, "int_1")
	}
	if got.Action.Type != "git.pull_request.create" {
		t.Errorf("Action.Type = %q, want %q", got.Action.Type, "git.pull_request.create")
	}
}

func TestIntentStore_GetNotFound(t *testing.T) {
	s := NewIntentStore()
	_, err := s.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent intent")
	}
	var nf *store.ErrNotFound
	if !isErrNotFound(err, &nf) {
		t.Fatalf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestIntentStore_List(t *testing.T) {
	s := NewIntentStore()
	ctx := context.Background()
	now := time.Now().UTC()

	s.Create(ctx, api.IntentEnvelope{
		IntentId: "int_1", WorkspaceId: "ws_1", Status: api.PendingApproval,
		Agent: api.ActorRef{Id: "a1", Type: api.Agent}, CreatedAt: now, UpdatedAt: now,
		Action: api.ActionIntent{Type: "git.pull_request.create", Summary: "s1"},
		Decision: api.Decision{Disposition: api.DecisionDispositionRequireApproval, RiskLevel: api.Medium},
	})
	s.Create(ctx, api.IntentEnvelope{
		IntentId: "int_2", WorkspaceId: "ws_1", Status: api.Approved,
		Agent: api.ActorRef{Id: "a2", Type: api.Agent}, CreatedAt: now, UpdatedAt: now,
		Action: api.ActionIntent{Type: "payment.charge", Summary: "s2"},
		Decision: api.Decision{Disposition: api.DecisionDispositionAllow, RiskLevel: api.Low},
	})
	s.Create(ctx, api.IntentEnvelope{
		IntentId: "int_3", WorkspaceId: "ws_2", Status: api.PendingApproval,
		Agent: api.ActorRef{Id: "a1", Type: api.Agent}, CreatedAt: now, UpdatedAt: now,
		Action: api.ActionIntent{Type: "git.pull_request.create", Summary: "s3"},
		Decision: api.Decision{Disposition: api.DecisionDispositionRequireApproval, RiskLevel: api.Medium},
	})

	// Filter by workspace.
	results, _ := s.List(ctx, store.IntentFilter{WorkspaceID: "ws_1"})
	if len(results) != 2 {
		t.Errorf("List(ws_1): got %d, want 2", len(results))
	}

	// Filter by status.
	pending := api.PendingApproval
	results, _ = s.List(ctx, store.IntentFilter{Status: &pending})
	if len(results) != 2 {
		t.Errorf("List(pending_approval): got %d, want 2", len(results))
	}

	// Filter by agent.
	results, _ = s.List(ctx, store.IntentFilter{AgentID: "a2"})
	if len(results) != 1 {
		t.Errorf("List(agent=a2): got %d, want 1", len(results))
	}

	// No filter.
	results, _ = s.List(ctx, store.IntentFilter{})
	if len(results) != 3 {
		t.Errorf("List(all): got %d, want 3", len(results))
	}
}

func TestIntentStore_Update(t *testing.T) {
	s := NewIntentStore()
	ctx := context.Background()
	now := time.Now().UTC()

	intent := api.IntentEnvelope{
		IntentId: "int_1", WorkspaceId: "ws_1", Status: api.PendingPolicy,
		Agent: api.ActorRef{Id: "a1", Type: api.Agent}, CreatedAt: now, UpdatedAt: now,
		Action: api.ActionIntent{Type: "git.pull_request.create", Summary: "s"},
		Decision: api.Decision{Disposition: api.DecisionDispositionAllow, RiskLevel: api.Low},
	}
	s.Create(ctx, intent)

	intent.Status = api.Approved
	if err := s.Update(ctx, intent); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := s.Get(ctx, "int_1")
	if got.Status != api.Approved {
		t.Errorf("Status = %q, want %q", got.Status, api.Approved)
	}
}

func TestIntentStore_UpdateNotFound(t *testing.T) {
	s := NewIntentStore()
	err := s.Update(context.Background(), api.IntentEnvelope{IntentId: "nope"})
	if err == nil {
		t.Fatal("expected error for nonexistent intent")
	}
}

func isErrNotFound(err error, target **store.ErrNotFound) bool {
	nf, ok := err.(*store.ErrNotFound)
	if ok && target != nil {
		*target = nf
	}
	return ok
}
