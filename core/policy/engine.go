package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// RuleEngine evaluates policy rules from the policy store against intents.
type RuleEngine struct {
	policies store.PolicyStore
}

// NewRuleEngine returns a policy engine backed by the given store.
func NewRuleEngine(policies store.PolicyStore) *RuleEngine {
	return &RuleEngine{policies: policies}
}

// Evaluate implements Engine.
func (e *RuleEngine) Evaluate(ctx context.Context, req EvaluationRequest) (model.Decision, error) {
	// Load all active policies for the workspace.
	activeStatus := api.PolicyStatusActive
	policies, err := e.policies.List(ctx, store.PolicyFilter{
		WorkspaceID: req.WorkspaceID,
		Status:      &activeStatus,
	})
	if err != nil {
		return model.Decision{}, fmt.Errorf("policy: list policies: %w", err)
	}

	// Flatten to intent fields for condition evaluation.
	fields := flattenIntent(req.Action, req.Context)

	// Collect all matching rules across policies, sorted by priority.
	type match struct {
		policy api.Policy
		rule   api.PolicyRule
	}
	var matches []match

	for _, p := range policies {
		for _, r := range p.Rules {
			if !actionTypeMatches(req.Action.Type, r) {
				continue
			}
			if !conditionsMatch(r.Conditions, fields) {
				continue
			}
			matches = append(matches, match{policy: p, rule: r})
		}
	}

	if len(matches) == 0 {
		// Default: allow with low risk when no rules match.
		return model.Decision{
			Disposition: model.DispositionAllow,
			RiskLevel:   model.RiskLevelLow,
		}, nil
	}

	// Sort by priority descending (higher priority wins).
	sort.Slice(matches, func(i, j int) bool {
		pi, pj := 0, 0
		if matches[i].rule.Priority != nil {
			pi = *matches[i].rule.Priority
		}
		if matches[j].rule.Priority != nil {
			pj = *matches[j].rule.Priority
		}
		return pi > pj
	})

	winner := matches[0]
	disposition := effectToDisposition(winner.rule.Effect)
	riskLevel := effectToRiskLevel(winner.rule.Effect)

	var policyMatches []model.PolicyMatch
	for _, m := range matches {
		explanation := ""
		if m.rule.Description != nil {
			explanation = *m.rule.Description
		}
		policyMatches = append(policyMatches, model.PolicyMatch{
			PolicyID:      m.policy.PolicyId,
			PolicyVersion: m.policy.Version,
			RuleID:        m.rule.RuleId,
			Explanation:   explanation,
		})
	}

	decision := model.Decision{
		Disposition:     disposition,
		RiskLevel:       riskLevel,
		MatchedPolicies: policyMatches,
	}

	if disposition == model.DispositionDeny {
		reason := "denied by policy"
		if winner.rule.Description != nil {
			reason = *winner.rule.Description
		}
		decision.DenialReason = reason
	}

	return decision, nil
}

// actionTypeMatches checks whether a rule applies to the given action type.
// Rules without conditions that have a field matching "action.type" apply to
// all action types. Rules with action.type conditions are filtered later.
// For now, we check conditions for an "action.type" field match.
func actionTypeMatches(actionType string, rule api.PolicyRule) bool {
	if rule.Conditions == nil || len(*rule.Conditions) == 0 {
		return true // no conditions = matches all
	}
	for _, c := range *rule.Conditions {
		if c.Field != nil && *c.Field == "action.type" {
			return evaluateCondition(c, actionType)
		}
	}
	// No action.type condition — rule matches all action types.
	return true
}

// conditionsMatch checks all conditions in a rule against flattened intent fields.
func conditionsMatch(conditions *[]api.PolicyCondition, fields map[string]any) bool {
	if conditions == nil {
		return true
	}
	for _, c := range *conditions {
		if c.Field == nil || c.Operator == nil || c.Value == nil {
			continue
		}
		fieldVal, ok := fields[*c.Field]
		if !ok {
			// Field not present in intent — condition fails.
			return false
		}
		if !evaluateCondition(c, fieldVal) {
			return false
		}
	}
	return true
}

// evaluateCondition evaluates a single condition against a field value.
func evaluateCondition(c api.PolicyCondition, fieldVal any) bool {
	if c.Operator == nil || c.Value == nil {
		return true
	}

	condVal := extractConditionValue(c.Value)
	fieldStr := fmt.Sprintf("%v", fieldVal)

	switch *c.Operator {
	case api.Eq:
		return fieldStr == fmt.Sprintf("%v", condVal)
	case api.Neq:
		return fieldStr != fmt.Sprintf("%v", condVal)
	case api.In:
		return valueInList(fieldStr, condVal)
	case api.NotIn:
		return !valueInList(fieldStr, condVal)
	case api.Contains:
		return strings.Contains(fieldStr, fmt.Sprintf("%v", condVal))
	case api.Matches:
		// Simple glob matching with * wildcard.
		pattern := fmt.Sprintf("%v", condVal)
		return globMatch(pattern, fieldStr)
	case api.Gt, api.Gte, api.Lt, api.Lte:
		return compareNumeric(*c.Operator, fieldVal, condVal)
	default:
		return false
	}
}

// extractConditionValue pulls the actual value from the union type.
func extractConditionValue(v *api.PolicyCondition_Value) any {
	if v == nil {
		return nil
	}
	// Try string first (most common).
	if s, err := v.AsPolicyConditionValue0(); err == nil {
		return s
	}
	// Try array (for in/not_in).
	if arr, err := v.AsPolicyConditionValue4(); err == nil {
		return arr
	}
	// Try int.
	if i, err := v.AsPolicyConditionValue2(); err == nil {
		return i
	}
	// Try float.
	if f, err := v.AsPolicyConditionValue1(); err == nil {
		return f
	}
	// Try bool.
	if b, err := v.AsPolicyConditionValue3(); err == nil {
		return b
	}
	// Fallback: unmarshal as raw JSON.
	data, _ := v.MarshalJSON()
	var raw any
	json.Unmarshal(data, &raw)
	return raw
}

func valueInList(val string, list any) bool {
	switch l := list.(type) {
	case []interface{}:
		for _, item := range l {
			if fmt.Sprintf("%v", item) == val {
				return true
			}
		}
	case []string:
		for _, item := range l {
			if item == val {
				return true
			}
		}
	}
	return false
}

func globMatch(pattern, value string) bool {
	// Simple wildcard matching: * matches any substring.
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, ".*")
		return strings.HasPrefix(value, prefix+".")
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(value, pattern[1:])
	}
	return pattern == value
}

func compareNumeric(op api.PolicyConditionOperator, fieldVal, condVal any) bool {
	fv := toFloat64(fieldVal)
	cv := toFloat64(condVal)
	switch op {
	case api.Gt:
		return fv > cv
	case api.Gte:
		return fv >= cv
	case api.Lt:
		return fv < cv
	case api.Lte:
		return fv <= cv
	}
	return false
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// flattenIntent creates a flat map of dot-notated fields from an intent
// and its context for use in condition evaluation.
func flattenIntent(action model.ActionIntent, intentCtx model.IntentContext) map[string]any {
	fields := map[string]any{
		"action.type":    action.Type,
		"action.summary": action.Summary,
	}

	if action.Target.Kind != "" {
		fields["target.kind"] = action.Target.Kind
	}
	if action.Target.ID != "" {
		fields["target.id"] = action.Target.ID
	}

	// Context fields.
	if intentCtx.Environment != "" {
		fields["context.environment"] = intentCtx.Environment
	}
	if intentCtx.SourcePlatform != "" {
		fields["context.source_platform"] = intentCtx.SourcePlatform
	}

	// Domain-specific fields.
	if action.Domain.Git != nil {
		g := action.Domain.Git
		fields["domain.git.provider"] = g.Provider
		fields["domain.git.repository"] = g.Repository
		fields["domain.git.branch"] = g.Branch
		fields["domain.git.base_branch"] = g.BaseBranch
		fields["domain.git.pr_title"] = g.PRTitle
	}
	if action.Domain.Payment != nil {
		p := action.Domain.Payment
		fields["domain.payment.vendor_name"] = p.VendorName
		fields["domain.payment.amount"] = p.Amount.Amount
		fields["domain.payment.currency"] = p.Amount.Currency
		fields["domain.payment.merchant_category"] = p.MerchantCategory
	}
	if action.Domain.Calendar != nil {
		c := action.Domain.Calendar
		fields["domain.calendar.title"] = c.Title
		if c.Attendees != nil {
			fields["domain.calendar.attendee_count"] = len(c.Attendees)
		}
	}

	return fields
}

func effectToDisposition(effect api.PolicyRuleEffect) model.Disposition {
	switch effect {
	case api.PolicyRuleEffectAllow:
		return model.DispositionAllow
	case api.PolicyRuleEffectDeny:
		return model.DispositionDeny
	case api.PolicyRuleEffectRequireApproval:
		return model.DispositionRequireApproval
	case api.PolicyRuleEffectAllowWithModification:
		return model.DispositionAllowModified
	default:
		return model.DispositionDeny
	}
}

func effectToRiskLevel(effect api.PolicyRuleEffect) model.RiskLevel {
	switch effect {
	case api.PolicyRuleEffectAllow:
		return model.RiskLevelLow
	case api.PolicyRuleEffectDeny:
		return model.RiskLevelCritical
	case api.PolicyRuleEffectRequireApproval:
		return model.RiskLevelMedium
	case api.PolicyRuleEffectAllowWithModification:
		return model.RiskLevelMedium
	default:
		return model.RiskLevelHigh
	}
}
