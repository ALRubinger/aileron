package policy

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func seedAndEngine(t *testing.T) *RuleEngine {
	t.Helper()
	policies := mem.NewPolicyStore()
	ctx := context.Background()
	if err := SeedPolicies(ctx, policies); err != nil {
		t.Fatalf("SeedPolicies: %v", err)
	}
	return NewRuleEngine(policies)
}

func TestEngine_PRToMainRequiresApproval(t *testing.T) {
	engine := seedAndEngine(t)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "claude_code",
		Action: model.ActionIntent{
			Type:    "git.pull_request.create",
			Summary: "Fix tax rounding",
			Domain: model.DomainAction{
				Git: &model.GitAction{
					Repository: "acme/checkout",
					Branch:     "fix/tax-rounding",
					BaseBranch: "main",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Disposition != model.DispositionRequireApproval {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionRequireApproval)
	}
	if decision.RiskLevel != model.RiskLevelMedium {
		t.Errorf("RiskLevel = %q, want %q", decision.RiskLevel, model.RiskLevelMedium)
	}
	if len(decision.MatchedPolicies) == 0 {
		t.Error("expected matched policies")
	}
}

func TestEngine_PRToProductionRequiresApproval(t *testing.T) {
	engine := seedAndEngine(t)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "claude_code",
		Action: model.ActionIntent{
			Type:    "git.pull_request.create",
			Summary: "Deploy hotfix",
			Domain: model.DomainAction{
				Git: &model.GitAction{
					Repository: "acme/checkout",
					Branch:     "hotfix/urgent",
					BaseBranch: "production",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Disposition != model.DispositionRequireApproval {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionRequireApproval)
	}
}

func TestEngine_PRToFeatureBranchAllowed(t *testing.T) {
	engine := seedAndEngine(t)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "claude_code",
		Action: model.ActionIntent{
			Type:    "git.pull_request.create",
			Summary: "Add tests",
			Domain: model.DomainAction{
				Git: &model.GitAction{
					Repository: "acme/checkout",
					Branch:     "feat/add-tests",
					BaseBranch: "develop",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The feature branch PR matches the "require approval for main" rule (action.type matches)
	// but the base_branch condition fails, so it falls through to the lower-priority
	// "allow feature branch PRs" rule.
	if decision.Disposition != model.DispositionAllow {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionAllow)
	}
}

func TestEngine_ForcePushDenied(t *testing.T) {
	engine := seedAndEngine(t)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "claude_code",
		Action: model.ActionIntent{
			Type:    "git.force_push",
			Summary: "Force push to main",
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Disposition != model.DispositionDeny {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionDeny)
	}
	if decision.RiskLevel != model.RiskLevelCritical {
		t.Errorf("RiskLevel = %q, want %q", decision.RiskLevel, model.RiskLevelCritical)
	}
	if decision.DenialReason == "" {
		t.Error("expected denial reason")
	}
}

func TestEngine_UnknownActionDefaultsToAllow(t *testing.T) {
	engine := seedAndEngine(t)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "claude_code",
		Action: model.ActionIntent{
			Type:    "email.send",
			Summary: "Send welcome email",
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Disposition != model.DispositionAllow {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionAllow)
	}
	if decision.RiskLevel != model.RiskLevelLow {
		t.Errorf("RiskLevel = %q, want %q", decision.RiskLevel, model.RiskLevelLow)
	}
}

func TestEngine_NoPoliciesDefaultsToAllow(t *testing.T) {
	// Empty policy store — no rules match.
	policies := mem.NewPolicyStore()
	engine := NewRuleEngine(policies)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "agent_1",
		Action: model.ActionIntent{
			Type:    "git.pull_request.create",
			Summary: "Create PR",
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Disposition != model.DispositionAllow {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionAllow)
	}
}
