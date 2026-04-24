package policy

import (
	"context"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store/mem"
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

func TestEngine_WriteActionDefaultsToRequireApproval(t *testing.T) {
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
	if decision.Disposition != model.DispositionRequireApproval {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionRequireApproval)
	}
	if decision.RiskLevel != model.RiskLevelMedium {
		t.Errorf("RiskLevel = %q, want %q", decision.RiskLevel, model.RiskLevelMedium)
	}
}

func TestEngine_ReadActionDefaultsToAllow(t *testing.T) {
	engine := seedAndEngine(t)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "claude_code",
		Action: model.ActionIntent{
			Type:    "source.query",
			Summary: "Search documents",
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

func TestEngine_NoPoliciesWriteActionDefaultsToRequireApproval(t *testing.T) {
	// Empty policy store — no rules match, but write actions default to RequireApproval.
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
	if decision.Disposition != model.DispositionRequireApproval {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionRequireApproval)
	}
}

func TestEngine_NoPoliciesReadActionDefaultsToAllow(t *testing.T) {
	// Empty policy store — no rules match, read actions default to Allow.
	policies := mem.NewPolicyStore()
	engine := NewRuleEngine(policies)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		AgentID:     "agent_1",
		Action: model.ActionIntent{
			Type:    "source.query",
			Summary: "Search documents",
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Disposition != model.DispositionAllow {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionAllow)
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		// Exact match
		{"go test", "go test", true},
		{"go test", "go tests", false},
		// Single wildcard at end
		{"git push *", "git push origin main", true},
		{"git push *", "git push", false},  // no trailing space before *
		{"git push*", "git push", true},    // no space: * matches empty
		{"git push *", "git pull origin", false},
		// Single wildcard at start
		{"*.sh", "script.sh", true},
		{"*.sh", "foo/bar.sh", true},
		{"*.sh", "script.py", false},
		// Wildcard in middle
		{"git * --force", "git push --force", true},
		{"git * --force", "git push origin main --force", true},
		{"git * --force", "git push origin main", false},
		// Multiple wildcards
		{"*go*test*", "go test ./...", true},
		{"*go*test*", "cargo test", true},   // "go" found in "cargo"
		{"*go*test*", "car run", false},
		// Universal wildcard
		{"*", "anything at all", true},
		{"*", "", true},
		// Dot-star pattern (still works for action types)
		{"git.*", "git.push", true},
		{"git.*", "git.pull_request.create", true},
		{"git.*", "deploy.create", false},
		// No wildcard — exact match
		{"echo hello", "echo hello", true},
		{"echo hello", "echo hello world", false},
	}
	for _, tt := range tests {
		got := globMatch(tt.pattern, tt.value)
		if got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}

func TestFlattenIntent_Metadata(t *testing.T) {
	action := model.ActionIntent{
		Type:    "shell.exec",
		Summary: "go test ./...",
		Metadata: map[string]any{
			"shell.command":     "go test ./...",
			"shell.binary":      "go",
			"shell.args":        "test ./...",
			"shell.working_dir": "/home/user/project",
		},
	}
	fields := flattenIntent(action, model.IntentContext{})

	if fields["shell.command"] != "go test ./..." {
		t.Errorf("shell.command = %v, want 'go test ./...'", fields["shell.command"])
	}
	if fields["shell.binary"] != "go" {
		t.Errorf("shell.binary = %v, want 'go'", fields["shell.binary"])
	}
	if fields["shell.args"] != "test ./..." {
		t.Errorf("shell.args = %v, want 'test ./...'", fields["shell.args"])
	}
	if fields["shell.working_dir"] != "/home/user/project" {
		t.Errorf("shell.working_dir = %v", fields["shell.working_dir"])
	}
}

func TestFlattenIntent_NilMetadata(t *testing.T) {
	action := model.ActionIntent{
		Type: "shell.exec",
	}
	fields := flattenIntent(action, model.IntentContext{})
	// Should not panic with nil metadata.
	if fields["action.type"] != "shell.exec" {
		t.Errorf("action.type = %v", fields["action.type"])
	}
}

func TestIsWriteAction(t *testing.T) {
	tests := []struct {
		actionType string
		want       bool
	}{
		// Write actions
		{"email.send", true},
		{"email.draft", true},
		{"calendar.event.create", true},
		{"calendar.event.update", true},
		{"calendar.event.delete", true},
		{"git.issue.create", true},
		{"git.issue.comment", true},
		{"git.pull_request.create", true},
		{"git.pull_request.merge", true},
		{"comms.slack.send", true},
		{"payment.charge", true},
		{"deploy.release", true},
		{"cloud.resource.mutate", true},
		{"procurement.request.submit", true},

		// Read-only actions
		{"source.query", false},
		{"git.status", false},
		{"calendar.event.list", false},
		{"shell.exec", false},
		{"unknown.action", false},
	}
	for _, tt := range tests {
		got := isWriteAction(tt.actionType)
		if got != tt.want {
			t.Errorf("isWriteAction(%q) = %v, want %v", tt.actionType, got, tt.want)
		}
	}
}

func TestEngine_ExplicitAllowOverridesWriteDefault(t *testing.T) {
	// Create a policy that explicitly allows email.send.
	// This should be respected — the org made a deliberate choice.
	policies := mem.NewPolicyStore()
	ctx := context.Background()

	allowEffect := api.PolicyRuleEffectAllow
	actionField := "action.type"
	eqOp := api.Eq
	emailSend := "email.send"
	condVal := api.PolicyCondition_Value{}
	condVal.FromPolicyConditionValue0(emailSend)
	priority := 100
	desc := "allow email sending"

	policies.Create(ctx, api.Policy{
		PolicyId:    "pol_allow_email",
		WorkspaceId: "default",
		Version:     1,
		Status:      api.PolicyStatusActive,
		Rules: []api.PolicyRule{
			{
				RuleId:   "rule_allow_email",
				Effect:   allowEffect,
				Priority: &priority,
				Description: &desc,
				Conditions: &[]api.PolicyCondition{
					{
						Field:    &actionField,
						Operator: &eqOp,
						Value:    &condVal,
					},
				},
			},
		},
	})

	engine := NewRuleEngine(policies)
	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		Action: model.ActionIntent{
			Type:    "email.send",
			Summary: "Send quarterly report",
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// Explicit Allow policy should be respected for write actions.
	if decision.Disposition != model.DispositionAllow {
		t.Errorf("Disposition = %q, want %q (explicit Allow should override write default)",
			decision.Disposition, model.DispositionAllow)
	}
}

func TestEngine_ExplicitDenyRespectedForWriteAction(t *testing.T) {
	policies := mem.NewPolicyStore()
	ctx := context.Background()

	denyEffect := api.PolicyRuleEffectDeny
	actionField := "action.type"
	eqOp := api.Eq
	emailSend := "email.send"
	condVal := api.PolicyCondition_Value{}
	condVal.FromPolicyConditionValue0(emailSend)
	priority := 100
	desc := "deny email sending"

	policies.Create(ctx, api.Policy{
		PolicyId:    "pol_deny_email",
		WorkspaceId: "default",
		Version:     1,
		Status:      api.PolicyStatusActive,
		Rules: []api.PolicyRule{
			{
				RuleId:      "rule_deny_email",
				Effect:      denyEffect,
				Priority:    &priority,
				Description: &desc,
				Conditions: &[]api.PolicyCondition{
					{
						Field:    &actionField,
						Operator: &eqOp,
						Value:    &condVal,
					},
				},
			},
		},
	})

	engine := NewRuleEngine(policies)
	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		Action: model.ActionIntent{
			Type:    "email.send",
			Summary: "Send spam",
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Disposition != model.DispositionDeny {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionDeny)
	}
}

func TestFlattenIntent_EmailFields(t *testing.T) {
	action := model.ActionIntent{
		Type:    "email.send",
		Summary: "Send quarterly report",
		Domain: model.DomainAction{
			Email: &model.EmailAction{
				To: []model.Recipient{
					{Name: "Alice", Email: "alice@example.com"},
					{Name: "Bob", Email: "bob@example.com"},
				},
				CC:       []model.Recipient{{Email: "manager@example.com"}},
				BCC:      []model.Recipient{{Email: "audit@example.com"}},
				Subject:  "Q4 Report",
				SendMode: "send_now",
			},
		},
	}
	fields := flattenIntent(action, model.IntentContext{})

	if fields["domain.email.subject"] != "Q4 Report" {
		t.Errorf("domain.email.subject = %v, want Q4 Report", fields["domain.email.subject"])
	}
	if fields["domain.email.send_mode"] != "send_now" {
		t.Errorf("domain.email.send_mode = %v, want send_now", fields["domain.email.send_mode"])
	}
	if fields["domain.email.recipient_count"] != 4 {
		t.Errorf("domain.email.recipient_count = %v, want 4", fields["domain.email.recipient_count"])
	}
	if fields["domain.email.to.0"] != "alice@example.com" {
		t.Errorf("domain.email.to.0 = %v", fields["domain.email.to.0"])
	}
	if fields["domain.email.to.1"] != "bob@example.com" {
		t.Errorf("domain.email.to.1 = %v", fields["domain.email.to.1"])
	}
	if fields["domain.email.cc.0"] != "manager@example.com" {
		t.Errorf("domain.email.cc.0 = %v", fields["domain.email.cc.0"])
	}
	if fields["domain.email.bcc.0"] != "audit@example.com" {
		t.Errorf("domain.email.bcc.0 = %v", fields["domain.email.bcc.0"])
	}
}

func TestFlattenIntent_CalendarFields(t *testing.T) {
	action := model.ActionIntent{
		Type:    "calendar.event.create",
		Summary: "Team standup",
		Domain: model.DomainAction{
			Calendar: &model.CalendarAction{
				Title:          "Daily Standup",
				Provider:       "google_calendar",
				Timezone:       "America/New_York",
				Location:       "Conference Room B",
				ConferenceType: "google_meet",
				Visibility:     "default",
				Attendees: []model.CalendarAttendee{
					{Email: "alice@example.com"},
					{Email: "bob@example.com"},
				},
			},
		},
	}
	fields := flattenIntent(action, model.IntentContext{})

	if fields["domain.calendar.title"] != "Daily Standup" {
		t.Errorf("domain.calendar.title = %v", fields["domain.calendar.title"])
	}
	if fields["domain.calendar.provider"] != "google_calendar" {
		t.Errorf("domain.calendar.provider = %v", fields["domain.calendar.provider"])
	}
	if fields["domain.calendar.timezone"] != "America/New_York" {
		t.Errorf("domain.calendar.timezone = %v", fields["domain.calendar.timezone"])
	}
	if fields["domain.calendar.location"] != "Conference Room B" {
		t.Errorf("domain.calendar.location = %v", fields["domain.calendar.location"])
	}
	if fields["domain.calendar.conference_type"] != "google_meet" {
		t.Errorf("domain.calendar.conference_type = %v", fields["domain.calendar.conference_type"])
	}
	if fields["domain.calendar.visibility"] != "default" {
		t.Errorf("domain.calendar.visibility = %v", fields["domain.calendar.visibility"])
	}
	if fields["domain.calendar.attendee_count"] != 2 {
		t.Errorf("domain.calendar.attendee_count = %v, want 2", fields["domain.calendar.attendee_count"])
	}
}

func TestEngine_CommsActionDefaultsToRequireApproval(t *testing.T) {
	// comms.* actions (Slack messages, etc.) should default to RequireApproval.
	policies := mem.NewPolicyStore()
	engine := NewRuleEngine(policies)
	ctx := context.Background()

	decision, err := engine.Evaluate(ctx, EvaluationRequest{
		WorkspaceID: "default",
		Action: model.ActionIntent{
			Type:    "comms.slack.send",
			Summary: "Send Slack message",
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Disposition != model.DispositionRequireApproval {
		t.Errorf("Disposition = %q, want %q", decision.Disposition, model.DispositionRequireApproval)
	}
}
