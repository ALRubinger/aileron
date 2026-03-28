// Package policy defines the SPI for the policy engine.
//
// The policy engine is the decision-making core of the control plane. It
// receives a normalized intent and returns a disposition: allow, deny,
// require approval, or request more evidence. All other components in the
// control plane consume this decision.
//
// The built-in implementation evaluates rule sets stored in the database.
// Alternative implementations (OPA, Cedar, custom DSLs) satisfy this
// interface without modifying the core.
package policy

import "context"

// Engine evaluates whether an intent should be allowed, denied, or escalated.
type Engine interface {
	Evaluate(ctx context.Context, req EvaluationRequest) (Decision, error)
}

// EvaluationRequest is the input to policy evaluation.
type EvaluationRequest struct {
	WorkspaceID string
	AgentID     string
	Action      Action
	Context     IntentContext
}

// Action describes what the agent intends to do.
type Action struct {
	// Type is a dot-namespaced action identifier, e.g. "payment.charge",
	// "git.pull_request.create", "calendar.event.create".
	Type          string
	Summary       string
	Justification string
	Target        ActionTarget
	Domain        DomainPayload
}

// ActionTarget describes the resource the action operates on.
type ActionTarget struct {
	Kind        string
	ID          string
	DisplayName string
}

// DomainPayload carries action-type-specific fields.
// Only the field corresponding to Action.Type will be populated.
type DomainPayload struct {
	Payment  *PaymentPayload
	Git      *GitPayload
	Calendar *CalendarPayload
}

// PaymentPayload carries payment-specific intent fields.
type PaymentPayload struct {
	VendorName    string
	VendorID      string
	AmountMinor   int64
	Currency      string
	Category      string
	BudgetCode    string
	FundingSource string
}

// GitPayload carries git-specific intent fields.
type GitPayload struct {
	Provider   string
	Repository string
	Branch     string
	BaseBranch string
	PRTitle    string
}

// CalendarPayload carries calendar-specific intent fields.
type CalendarPayload struct {
	Provider string
	Title    string
	// Attendee email addresses.
	Attendees []string
}

// IntentContext carries runtime context used by policy rules.
type IntentContext struct {
	Environment     string
	SourcePlatform  string
	SourceSessionID string
	IPAddress       string
	UserPresent     bool
	RiskHints       []string
}

// Decision is the output of policy evaluation.
type Decision struct {
	Disposition     Disposition
	RiskLevel       RiskLevel
	MatchedPolicies []PolicyMatch
	DenialReason    string
}

// PolicyMatch records which policy rule produced the decision.
type PolicyMatch struct {
	PolicyID      string
	PolicyVersion int
	RuleID        string
	Explanation   string
}

// Disposition is the outcome of policy evaluation.
type Disposition string

const (
	DispositionAllow               Disposition = "allow"
	DispositionAllowModified       Disposition = "allow_modified"
	DispositionRequireApproval     Disposition = "require_approval"
	DispositionDeny                Disposition = "deny"
	DispositionRequireMoreEvidence Disposition = "require_more_evidence"
)

// RiskLevel is the assessed risk of the intent.
type RiskLevel string

const (
	RiskLevelLow      RiskLevel = "low"
	RiskLevelMedium   RiskLevel = "medium"
	RiskLevelHigh     RiskLevel = "high"
	RiskLevelCritical RiskLevel = "critical"
)
