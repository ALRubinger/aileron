package policy

import (
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

func TestActiveStatus(t *testing.T) {
	if ActiveStatus() != api.PolicyStatusActive {
		t.Errorf("expected PolicyStatusActive, got %v", ActiveStatus())
	}
}

func TestMakePolicy(t *testing.T) {
	rules := []api.PolicyRule{{RuleId: "test"}}
	p := MakePolicy("id1", "ws1", rules, api.PolicyStatusActive)

	if p.PolicyId != "id1" {
		t.Errorf("PolicyId = %q, want 'id1'", p.PolicyId)
	}
	if p.WorkspaceId != "ws1" {
		t.Errorf("WorkspaceId = %q, want 'ws1'", p.WorkspaceId)
	}
	if len(p.Rules) != 1 {
		t.Errorf("Rules = %d, want 1", len(p.Rules))
	}
	if p.Status != api.PolicyStatusActive {
		t.Errorf("Status = %v, want Active", p.Status)
	}
	if p.CreatedAt == nil {
		t.Error("expected CreatedAt to be set")
	}
}
