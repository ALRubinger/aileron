// Package model defines shared domain types used across the control plane.
//
// These types are the canonical Go representations of the structures defined
// in the OpenAPI specification. All SPI packages import from here rather than
// defining their own copies.
package model

import "time"

// ActorRef identifies who or what performed an action.
type ActorRef struct {
	ID          string
	Type        ActorType
	DisplayName string
}

// ActorType classifies the actor.
type ActorType string

const (
	ActorTypeAgent            ActorType = "agent"
	ActorTypeHuman            ActorType = "human"
	ActorTypeService          ActorType = "service"
	ActorTypeConnectorRuntime ActorType = "connector_runtime"
)

// ActionTarget describes the resource an action operates on.
type ActionTarget struct {
	Kind        string
	ID          string
	DisplayName string
}

// ActionIntent describes what an agent intends to do.
type ActionIntent struct {
	// Type is a dot-namespaced action identifier, e.g. "payment.charge",
	// "git.pull_request.create", "calendar.event.create".
	Type          string
	Summary       string
	Justification string
	Target        ActionTarget
	Domain        DomainAction
	Metadata      map[string]any
}

// DomainAction carries action-type-specific fields.
// Exactly one field should be populated, corresponding to the ActionIntent.Type prefix.
type DomainAction struct {
	Git         *GitAction
	Deploy      *DeployAction
	Cloud       *CloudAction
	Email       *EmailAction
	Calendar    *CalendarAction
	Payment     *PaymentAction
	Procurement *ProcurementAction
}

// Money represents an amount in minor units (e.g. cents) with a currency code.
type Money struct {
	Amount   int64
	Currency string // ISO 4217, e.g. "USD"
}

// GitAction carries git/source-control-specific intent fields.
type GitAction struct {
	Provider       string // "github", "gitlab", "bitbucket", "custom"
	Repository     string
	Branch         string
	BaseBranch     string
	CommitSHAs     []string
	FilesChanged   []string
	PRTitle        string
	PRBody         string
	Labels         []string
	Reviewers      []string
	ChecksRequired []string
}

// DeployAction carries deployment-specific intent fields.
type DeployAction struct {
	Provider            string // "kubernetes", "vercel", "netlify", etc.
	Service             string
	Environment         string
	ArtifactRef         string
	ImageRef            string
	Cluster             string
	Namespace           string
	Strategy            string // "rolling", "blue_green", "canary", "replace", "custom"
	RollbackSupported   bool
	ChangeTicketID      string
	MaintenanceWindowID string
}

// CloudAction carries cloud-resource-specific intent fields.
type CloudAction struct {
	Provider      string // "aws", "gcp", "azure", "cloudflare", "custom"
	AccountID     string
	Region        string
	ResourceType  string
	ResourceID    string
	Operation     string
	EstimatedCost *Money
	Tags          map[string]string
}

// EmailAction carries email-specific intent fields.
type EmailAction struct {
	From        *Recipient
	To          []Recipient
	CC          []Recipient
	BCC         []Recipient
	Subject     string
	BodyText    string
	BodyHTML    string
	Attachments []AttachmentRef
	ThreadRef   string
	SendMode    string // "send_now", "draft_only"
}

// Recipient identifies an email participant.
type Recipient struct {
	Name  string
	Email string
}

// AttachmentRef references a file attachment.
type AttachmentRef struct {
	Name       string
	MIMEType   string
	URL        string
	StorageRef string
}

// CalendarAction carries calendar-specific intent fields.
type CalendarAction struct {
	Provider       string // "google_calendar", "outlook", "custom"
	Title          string
	Description    string
	Attendees      []CalendarAttendee
	StartTime      *time.Time
	EndTime        *time.Time
	Timezone       string
	Location       string
	ConferenceType string // "none", "google_meet", "zoom", "teams", "custom"
	CalendarID     string
	Visibility     string // "default", "public", "private"
}

// CalendarAttendee is a participant in a calendar event.
type CalendarAttendee struct {
	Name     string
	Email    string
	Optional bool
}

// PaymentAction carries payment-specific intent fields.
type PaymentAction struct {
	VendorName                   string
	VendorID                     string
	Amount                       Money
	MerchantCategory             string
	BudgetCode                   string
	FundingSourceID              string
	PaymentInstrumentPreference  string // "virtual_card", "ach", "wallet", "network_proxy", "unspecified"
	Beneficiary                  *PaymentBeneficiary
	LineItems                    []LineItem
	Renewal                      bool
	ContractTerm                 string
	RecurringInterval            string // "none", "monthly", "quarterly", "annual"
	MerchantReference            string
}

// PaymentBeneficiary identifies the person or team benefiting from a payment.
type PaymentBeneficiary struct {
	Name       string
	Email      string
	Department string
}

// LineItem is a single item in a payment or procurement request.
type LineItem struct {
	Description string
	Quantity    int
	UnitAmount  Money
	SKU         string
}

// ProcurementAction carries procurement-specific intent fields.
type ProcurementAction struct {
	RequestType            string // "software", "travel", "equipment", "contractor", "services", "other"
	VendorName             string
	AmountEstimate         *Money
	Justification          string
	Requestor              string
	CostCenter             string
	LegalReviewRequired    bool
	SecurityReviewRequired bool
	LineItems              []LineItem
}

// IntentContext carries runtime context used by policy rules.
type IntentContext struct {
	Environment      string
	SourcePlatform   string
	SourceSessionID  string
	SourceTraceID    string
	IPAddress        string
	UserPresent      bool
	RiskHints        []string
	TemporaryGrantID string
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

// RiskLevel is the assessed risk of an intent.
type RiskLevel string

const (
	RiskLevelLow      RiskLevel = "low"
	RiskLevelMedium   RiskLevel = "medium"
	RiskLevelHigh     RiskLevel = "high"
	RiskLevelCritical RiskLevel = "critical"
)

// Decision is the output of policy evaluation.
type Decision struct {
	Disposition     Disposition
	RiskLevel       RiskLevel
	MatchedPolicies []PolicyMatch
	DenialReason    string
}

// PolicyMatch records which policy rule produced a decision.
type PolicyMatch struct {
	PolicyID      string
	PolicyVersion int
	RuleID        string
	Explanation   string
}

// EventType identifies the kind of audit event.
type EventType string

const (
	EventTypeIntentSubmitted    EventType = "intent.submitted"
	EventTypePolicyEvaluated    EventType = "policy.evaluated"
	EventTypeApprovalRequested  EventType = "approval.requested"
	EventTypeApprovalApproved   EventType = "approval.approved"
	EventTypeApprovalDenied     EventType = "approval.denied"
	EventTypeApprovalModified   EventType = "approval.modified"
	EventTypeGrantIssued        EventType = "grant.issued"
	EventTypeGrantRevoked       EventType = "grant.revoked"
	EventTypeExecutionStarted   EventType = "execution.started"
	EventTypeExecutionSucceeded EventType = "execution.succeeded"
	EventTypeExecutionFailed    EventType = "execution.failed"

	// Gateway tool call events.
	EventTypeToolCallIntercepted    EventType = "tool.call.intercepted"
	EventTypeToolCallForwarded      EventType = "tool.call.forwarded"
	EventTypeToolCallDenied         EventType = "tool.call.denied"
	EventTypeToolCallPendingApproval EventType = "tool.call.pending_approval"
)
