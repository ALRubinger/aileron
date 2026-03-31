package mem

import (
	"context"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

func TestPolicyStore_CreateAndGet(t *testing.T) {
	s := NewPolicyStore()
	ctx := context.Background()
	now := time.Now().UTC()

	p := api.Policy{
		PolicyId:    "pol_1",
		WorkspaceId: "ws_1",
		Name:        "Test Policy",
		Version:     1,
		Status:      api.PolicyStatusActive,
		CreatedAt:   &now,
		UpdatedAt:   &now,
		Rules: []api.PolicyRule{
			{RuleId: "r1", Effect: api.PolicyRuleEffectAllow},
		},
	}

	if err := s.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "pol_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Test Policy" {
		t.Errorf("Name = %q, want %q", got.Name, "Test Policy")
	}
	if len(got.Rules) != 1 {
		t.Errorf("Rules = %d, want 1", len(got.Rules))
	}
}

func TestPolicyStore_ListByStatus(t *testing.T) {
	s := NewPolicyStore()
	ctx := context.Background()
	now := time.Now().UTC()

	s.Create(ctx, api.Policy{PolicyId: "pol_1", WorkspaceId: "ws_1", Name: "p1", Status: api.PolicyStatusActive, Version: 1, CreatedAt: &now, UpdatedAt: &now, Rules: []api.PolicyRule{}})
	s.Create(ctx, api.Policy{PolicyId: "pol_2", WorkspaceId: "ws_1", Name: "p2", Status: api.PolicyStatusDraft, Version: 1, CreatedAt: &now, UpdatedAt: &now, Rules: []api.PolicyRule{}})
	s.Create(ctx, api.Policy{PolicyId: "pol_3", WorkspaceId: "ws_1", Name: "p3", Status: api.PolicyStatusActive, Version: 1, CreatedAt: &now, UpdatedAt: &now, Rules: []api.PolicyRule{}})

	active := api.PolicyStatusActive
	results, _ := s.List(ctx, store.PolicyFilter{Status: &active})
	if len(results) != 2 {
		t.Errorf("List(active): got %d, want 2", len(results))
	}
}

func TestPolicyStore_Update(t *testing.T) {
	s := NewPolicyStore()
	ctx := context.Background()
	now := time.Now().UTC()

	s.Create(ctx, api.Policy{PolicyId: "pol_1", WorkspaceId: "ws_1", Name: "v1", Status: api.PolicyStatusActive, Version: 1, CreatedAt: &now, UpdatedAt: &now, Rules: []api.PolicyRule{}})

	p, _ := s.Get(ctx, "pol_1")
	p.Name = "v2"
	p.Version = 2
	s.Update(ctx, p)

	got, _ := s.Get(ctx, "pol_1")
	if got.Name != "v2" {
		t.Errorf("Name = %q, want %q", got.Name, "v2")
	}
	if got.Version != 2 {
		t.Errorf("Version = %d, want 2", got.Version)
	}
}
