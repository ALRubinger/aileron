package policy

import (
	"context"
	"encoding/json"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// SeedPolicies creates the default demo policies in the store.
func SeedPolicies(ctx context.Context, policies store.PolicyStore) error {
	now := time.Now().UTC()
	defaultWorkspace := "default"

	// Helper to create a PolicyCondition_Value from a JSON value.
	makeValue := func(v any) *api.PolicyCondition_Value {
		data, _ := json.Marshal(v)
		val := &api.PolicyCondition_Value{}
		val.UnmarshalJSON(data)
		return val
	}

	// Policy 1: Require approval for PRs to protected branches.
	protectedBranches := api.Policy{
		PolicyId:    "pol_require_approval_protected_branches",
		WorkspaceId: defaultWorkspace,
		Name:        "Require approval for PRs to protected branches",
		Version:     1,
		Status:      api.PolicyStatusActive,
		CreatedAt:   &now,
		UpdatedAt:   &now,
		Rules: []api.PolicyRule{
			{
				RuleId: "rule_pr_to_main",
				Description: strPtr("Require approval for pull requests targeting main or production branches"),
				Effect: api.PolicyRuleEffectRequireApproval,
				Priority: intPtr(100),
				Conditions: &[]api.PolicyCondition{
					{
						Field:    strPtr("action.type"),
						Operator: operatorPtr(api.Eq),
						Value:    makeValue("git.pull_request.create"),
					},
					{
						Field:    strPtr("domain.git.base_branch"),
						Operator: operatorPtr(api.In),
						Value:    makeValue([]string{"main", "master", "production"}),
					},
				},
			},
		},
	}

	// Policy 2: Allow PRs to feature branches.
	featureBranches := api.Policy{
		PolicyId:    "pol_allow_feature_branch_prs",
		WorkspaceId: defaultWorkspace,
		Name:        "Allow PRs to feature branches",
		Version:     1,
		Status:      api.PolicyStatusActive,
		CreatedAt:   &now,
		UpdatedAt:   &now,
		Rules: []api.PolicyRule{
			{
				RuleId:      "rule_pr_feature",
				Description: strPtr("Allow pull requests to non-protected branches"),
				Effect:      api.PolicyRuleEffectAllow,
				Priority:    intPtr(50),
				Conditions: &[]api.PolicyCondition{
					{
						Field:    strPtr("action.type"),
						Operator: operatorPtr(api.Eq),
						Value:    makeValue("git.pull_request.create"),
					},
				},
			},
		},
	}

	// Policy 3: Deny force pushes.
	denyForcePush := api.Policy{
		PolicyId:    "pol_deny_force_push",
		WorkspaceId: defaultWorkspace,
		Name:        "Deny force pushes",
		Version:     1,
		Status:      api.PolicyStatusActive,
		CreatedAt:   &now,
		UpdatedAt:   &now,
		Rules: []api.PolicyRule{
			{
				RuleId:      "rule_no_force_push",
				Description: strPtr("Force pushes are never allowed"),
				Effect:      api.PolicyRuleEffectDeny,
				Priority:    intPtr(200),
				Conditions: &[]api.PolicyCondition{
					{
						Field:    strPtr("action.type"),
						Operator: operatorPtr(api.Eq),
						Value:    makeValue("git.force_push"),
					},
				},
			},
		},
	}

	// Policy 4: Require approval for tool calls targeting protected branches
	// via the MCP gateway. This uses tool.* fields set by the gateway when
	// proxying MCP tool calls.
	toolProtectedBranches := api.Policy{
		PolicyId:    "pol_tool_require_approval_protected_branches",
		WorkspaceId: defaultWorkspace,
		Name:        "Require approval for gateway tool calls targeting protected branches",
		Version:     1,
		Status:      api.PolicyStatusActive,
		CreatedAt:   &now,
		UpdatedAt:   &now,
		Rules: []api.PolicyRule{
			{
				RuleId:      "rule_tool_pr_to_main",
				Description: strPtr("Require approval for tool calls targeting main/master/production"),
				Effect:      api.PolicyRuleEffectRequireApproval,
				Priority:    intPtr(100),
				Conditions: &[]api.PolicyCondition{
					{
						Field:    strPtr("tool.argument.base"),
						Operator: operatorPtr(api.In),
						Value:    makeValue([]string{"main", "master", "production"}),
					},
				},
			},
		},
	}

	// Policy 5: Deny destructive tool calls (delete_* tools).
	denyDestructiveTools := api.Policy{
		PolicyId:    "pol_deny_destructive_tools",
		WorkspaceId: defaultWorkspace,
		Name:        "Deny destructive tool calls",
		Version:     1,
		Status:      api.PolicyStatusActive,
		CreatedAt:   &now,
		UpdatedAt:   &now,
		Rules: []api.PolicyRule{
			{
				RuleId:      "rule_deny_delete_tools",
				Description: strPtr("Deny tool calls with names matching delete_*"),
				Effect:      api.PolicyRuleEffectDeny,
				Priority:    intPtr(200),
				Conditions: &[]api.PolicyCondition{
					{
						Field:    strPtr("tool.name"),
						Operator: operatorPtr(api.Matches),
						Value:    makeValue("delete_*"),
					},
				},
			},
		},
	}

	allPolicies := []api.Policy{
		protectedBranches,
		featureBranches,
		denyForcePush,
		toolProtectedBranches,
		denyDestructiveTools,
	}
	for _, p := range allPolicies {
		if err := policies.Create(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func strPtr(s string) *string                                  { return &s }
func intPtr(i int) *int                                        { return &i }
func operatorPtr(o api.PolicyConditionOperator) *api.PolicyConditionOperator { return &o }
