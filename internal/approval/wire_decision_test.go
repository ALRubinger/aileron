package approval_test

import (
	"errors"
	"testing"

	"github.com/ALRubinger/aileron/internal/approval"
)

// WireDecision contract:
//   - waitErr non-nil → DecisionDeny (timeout, ctx cancelled, etc.)
//   - !approved → DecisionDeny
//   - approved + no save_policy → DecisionAllowOnce
//   - approved + save_policy="project" → DecisionAllowProject
//   - approved + save_policy="user"    → DecisionAllowUser
//   - approved + unknown save_policy   → DecisionAllowOnce (degrade
//     rather than fail; user already clicked Approve)

func TestWireDecision_TimeoutCollapsesToDeny(t *testing.T) {
	got := approval.WireDecision(approval.ActionDecision{Approved: true}, errors.New("timeout"))
	if got != approval.DecisionDeny {
		t.Errorf("got %q, want %q", got, approval.DecisionDeny)
	}
}

func TestWireDecision_NotApprovedCollapsesToDeny(t *testing.T) {
	got := approval.WireDecision(approval.ActionDecision{Approved: false}, nil)
	if got != approval.DecisionDeny {
		t.Errorf("got %q, want %q", got, approval.DecisionDeny)
	}
}

func TestWireDecision_ApprovedNoSavePolicy_AllowOnce(t *testing.T) {
	got := approval.WireDecision(approval.ActionDecision{Approved: true}, nil)
	if got != approval.DecisionAllowOnce {
		t.Errorf("got %q, want %q", got, approval.DecisionAllowOnce)
	}
}

func TestWireDecision_ApprovedSavePolicyTable(t *testing.T) {
	cases := map[string]string{
		"project":  approval.DecisionAllowProject,
		"user":     approval.DecisionAllowUser,
		"":         approval.DecisionAllowOnce,
		"unknown":  approval.DecisionAllowOnce,
		"PROJECT":  approval.DecisionAllowOnce, // case-sensitive guard
	}
	for save, want := range cases {
		t.Run(save, func(t *testing.T) {
			got := approval.WireDecision(approval.ActionDecision{
				Approved:      true,
				EditedPayload: map[string]any{approval.SavePolicyKey: save},
			}, nil)
			if got != want {
				t.Errorf("save_policy=%q: got %q, want %q", save, got, want)
			}
		})
	}
}

func TestWireDecision_NonStringSavePolicyDegradesToAllowOnce(t *testing.T) {
	got := approval.WireDecision(approval.ActionDecision{
		Approved:      true,
		EditedPayload: map[string]any{approval.SavePolicyKey: 42}, // wrong type
	}, nil)
	if got != approval.DecisionAllowOnce {
		t.Errorf("got %q, want %q", got, approval.DecisionAllowOnce)
	}
}
