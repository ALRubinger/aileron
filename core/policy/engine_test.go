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

func TestFlattenToolCall(t *testing.T) {
	tc := &ToolCallContext{
		ServerName:    "github",
		ToolName:      "create_pull_request",
		QualifiedName: "github__create_pull_request",
		Arguments: map[string]any{
			"owner": "acme",
			"repo":  "checkout",
			"base":  "main",
			"head":  "fix/tax",
		},
	}

	fields := flattenToolCall(tc)

	expected := map[string]any{
		"tool.server":         "github",
		"tool.name":           "create_pull_request",
		"tool.qualified_name": "github__create_pull_request",
		"tool.argument.owner": "acme",
		"tool.argument.repo":  "checkout",
		"tool.argument.base":  "main",
		"tool.argument.head":  "fix/tax",
	}

	for k, want := range expected {
		got, ok := fields[k]
		if !ok {
			t.Errorf("missing field %q", k)
			continue
		}
		if got != want {
			t.Errorf("fields[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestEngine_ToolCallFieldsMergedIntoEvaluation(t *testing.T) {
	engine := seedAndEngine(t)
	ctx := context.Background()

	// Evaluate with a ToolCall context alongside the normal Action.
	// The seed policies match on action.type "git.pull_request.create"
	// and domain.git.base_branch "main". The ToolCall adds tool.* fields
	// but shouldn't interfere with existing policy matching.
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
		ToolCall: &ToolCallContext{
			ServerName:    "github",
			ToolName:      "create_pull_request",
			QualifiedName: "github__create_pull_request",
			Arguments: map[string]any{
				"base": "main",
			},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// Should still require approval (same policy match as without ToolCall).
	if decision.Disposition != model.DispositionRequireApproval {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionRequireApproval)
	}
}

func TestFlattenToolCall_EmptyArguments(t *testing.T) {
	tc := &ToolCallContext{
		ServerName:    "slack",
		ToolName:      "send_message",
		QualifiedName: "slack__send_message",
		Arguments:     nil,
	}

	fields := flattenToolCall(tc)
	if fields["tool.server"] != "slack" {
		t.Errorf("tool.server = %v, want %q", fields["tool.server"], "slack")
	}
	if fields["tool.name"] != "send_message" {
		t.Errorf("tool.name = %v, want %q", fields["tool.name"], "send_message")
	}
	// No tool.argument.* fields should exist.
	for k := range fields {
		if len(k) > 14 && k[:14] == "tool.argument." {
			t.Errorf("unexpected argument field %q for nil arguments", k)
		}
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
