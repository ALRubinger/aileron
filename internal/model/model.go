// Package model defines shared domain types used across the control plane.
//
// These types are the canonical Go representations of the structures defined
// in the OpenAPI specification. All SPI packages import from here rather than
// defining their own copies.
package model

import "time"

// ActorRef identifies who or what performed an action.
type ActorRef struct {
	ID          string    `json:"id"`
	Type        ActorType `json:"type"`
	DisplayName string    `json:"display_name"`
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
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// ActionIntent describes what an agent intends to do.
type ActionIntent struct {
	// Type is a dot-namespaced action identifier, e.g. "payment.charge",
	// "git.pull_request.create", "calendar.event.create".
	Type          string         `json:"type"`
	Summary       string         `json:"summary"`
	Justification string         `json:"justification"`
	Target        ActionTarget   `json:"target"`
	Domain        DomainAction   `json:"domain"`
	Metadata      map[string]any `json:"metadata"`
}

// DomainAction carries action-type-specific fields.
// Exactly one field should be populated, corresponding to the ActionIntent.Type prefix.
type DomainAction struct {
	Git         *GitAction         `json:"git,omitempty"`
	Deploy      *DeployAction      `json:"deploy,omitempty"`
	Cloud       *CloudAction       `json:"cloud,omitempty"`
	Email       *EmailAction       `json:"email,omitempty"`
	Calendar    *CalendarAction    `json:"calendar,omitempty"`
	Payment     *PaymentAction     `json:"payment,omitempty"`
	Procurement *ProcurementAction `json:"procurement,omitempty"`
}

// Money represents an amount in minor units (e.g. cents) with a currency code.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"` // ISO 4217, e.g. "USD"
}

// GitAction carries git/source-control-specific intent fields.
type GitAction struct {
	Provider       string   `json:"provider"` // "github", "gitlab", "bitbucket", "custom"
	Repository     string   `json:"repository"`
	Branch         string   `json:"branch"`
	BaseBranch     string   `json:"base_branch"`
	CommitSHAs     []string `json:"commit_shas"`
	FilesChanged   []string `json:"files_changed"`
	PRTitle        string   `json:"pr_title"`
	PRBody         string   `json:"pr_body"`
	Labels         []string `json:"labels"`
	Reviewers      []string `json:"reviewers"`
	ChecksRequired []string `json:"checks_required"`
	IssueTitle     string   `json:"issue_title"`
	IssueBody      string   `json:"issue_body"`
	IssueLabels    []string `json:"issue_labels"`
	IssueAssignees []string `json:"issue_assignees"`
}

// DeployAction carries deployment-specific intent fields.
type DeployAction struct {
	Provider            string `json:"provider"` // "kubernetes", "vercel", "netlify", etc.
	Service             string `json:"service"`
	Environment         string `json:"environment"`
	ArtifactRef         string `json:"artifact_ref"`
	ImageRef            string `json:"image_ref"`
	Cluster             string `json:"cluster"`
	Namespace           string `json:"namespace"`
	Strategy            string `json:"strategy"` // "rolling", "blue_green", "canary", "replace", "custom"
	RollbackSupported   bool   `json:"rollback_supported"`
	ChangeTicketID      string `json:"change_ticket_id"`
	MaintenanceWindowID string `json:"maintenance_window_id"`
}

// CloudAction carries cloud-resource-specific intent fields.
type CloudAction struct {
	Provider      string            `json:"provider"` // "aws", "gcp", "azure", "cloudflare", "custom"
	AccountID     string            `json:"account_id"`
	Region        string            `json:"region"`
	ResourceType  string            `json:"resource_type"`
	ResourceID    string            `json:"resource_id"`
	Operation     string            `json:"operation"`
	EstimatedCost *Money            `json:"estimated_cost,omitempty"`
	Tags          map[string]string `json:"tags"`
}

// EmailAction carries email-specific intent fields.
type EmailAction struct {
	From        *Recipient      `json:"from,omitempty"`
	To          []Recipient     `json:"to"`
	CC          []Recipient     `json:"cc,omitempty"`
	BCC         []Recipient     `json:"bcc,omitempty"`
	Subject     string          `json:"subject"`
	BodyText    string          `json:"body_text"`
	BodyHTML    string          `json:"body_html"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
	ThreadRef   string          `json:"thread_ref"`
	SendMode    string          `json:"send_mode"` // "send_now", "draft_only"
}

// Recipient identifies an email participant.
type Recipient struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// AttachmentRef references a file attachment.
type AttachmentRef struct {
	Name       string `json:"name"`
	MIMEType   string `json:"mime_type"`
	URL        string `json:"url"`
	StorageRef string `json:"storage_ref"`
}

// CalendarAction carries calendar-specific intent fields.
type CalendarAction struct {
	Provider       string             `json:"provider"` // "google_calendar", "outlook", "custom"
	Title          string             `json:"title"`
	Description    string             `json:"description"`
	Attendees      []CalendarAttendee `json:"attendees,omitempty"`
	StartTime      *time.Time         `json:"start_time,omitempty"`
	EndTime        *time.Time         `json:"end_time,omitempty"`
	Timezone       string             `json:"timezone"`
	Location       string             `json:"location"`
	ConferenceType string             `json:"conference_type"` // "none", "google_meet", "zoom", "teams", "custom"
	CalendarID     string             `json:"calendar_id"`
	Visibility     string             `json:"visibility"` // "default", "public", "private"
}

// CalendarAttendee is a participant in a calendar event.
type CalendarAttendee struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Optional bool   `json:"optional"`
}

// PaymentAction carries payment-specific intent fields.
type PaymentAction struct {
	VendorName                  string              `json:"vendor_name"`
	VendorID                    string              `json:"vendor_id"`
	Amount                      Money               `json:"amount"`
	MerchantCategory            string              `json:"merchant_category"`
	BudgetCode                  string              `json:"budget_code"`
	FundingSourceID             string              `json:"funding_source_id"`
	PaymentInstrumentPreference string              `json:"payment_instrument_preference"` // "virtual_card", "ach", "wallet", "network_proxy", "unspecified"
	Beneficiary                 *PaymentBeneficiary `json:"beneficiary,omitempty"`
	LineItems                   []LineItem          `json:"line_items,omitempty"`
	Renewal                     bool                `json:"renewal"`
	ContractTerm                string              `json:"contract_term"`
	RecurringInterval           string              `json:"recurring_interval"` // "none", "monthly", "quarterly", "annual"
	MerchantReference           string              `json:"merchant_reference"`
}

// PaymentBeneficiary identifies the person or team benefiting from a payment.
type PaymentBeneficiary struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Department string `json:"department"`
}

// LineItem is a single item in a payment or procurement request.
type LineItem struct {
	Description string `json:"description"`
	Quantity    int    `json:"quantity"`
	UnitAmount  Money  `json:"unit_amount"`
	SKU         string `json:"sku"`
}

// ProcurementAction carries procurement-specific intent fields.
type ProcurementAction struct {
	RequestType            string     `json:"request_type"` // "software", "travel", "equipment", "contractor", "services", "other"
	VendorName             string     `json:"vendor_name"`
	AmountEstimate         *Money     `json:"amount_estimate,omitempty"`
	Justification          string     `json:"justification"`
	Requestor              string     `json:"requestor"`
	CostCenter             string     `json:"cost_center"`
	LegalReviewRequired    bool       `json:"legal_review_required"`
	SecurityReviewRequired bool       `json:"security_review_required"`
	LineItems              []LineItem `json:"line_items,omitempty"`
}

// IntentContext carries runtime context used by policy rules.
type IntentContext struct {
	Environment      string   `json:"environment"`
	SourcePlatform   string   `json:"source_platform"`
	SourceSessionID  string   `json:"source_session_id"`
	SourceTraceID    string   `json:"source_trace_id"`
	IPAddress        string   `json:"ip_address"`
	UserPresent      bool     `json:"user_present"`
	RiskHints        []string `json:"risk_hints,omitempty"`
	TemporaryGrantID string   `json:"temporary_grant_id"`
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
	Disposition     Disposition   `json:"disposition"`
	RiskLevel       RiskLevel     `json:"risk_level"`
	MatchedPolicies []PolicyMatch `json:"matched_policies,omitempty"`
	DenialReason    string        `json:"denial_reason"`
}

// PolicyMatch records which policy rule produced a decision.
type PolicyMatch struct {
	PolicyID      string `json:"policy_id"`
	PolicyVersion int    `json:"policy_version"`
	RuleID        string `json:"rule_id"`
	Explanation   string `json:"explanation"`
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
	EventTypeToolCallIntercepted     EventType = "tool.call.intercepted"
	EventTypeToolCallForwarded       EventType = "tool.call.forwarded"
	EventTypeToolCallDenied          EventType = "tool.call.denied"
	EventTypeToolCallPendingApproval EventType = "tool.call.pending_approval"

	// Binding lifecycle events (ADR-0006). Payloads carry the binding
	// name, connector FQN, and capability kind — never the credential
	// bytes.
	EventTypeBindingCreated   EventType = "binding.created"
	EventTypeBindingResolved  EventType = "binding.resolved"
	EventTypeBindingRebound   EventType = "binding.rebound"
	EventTypeBindingRevoked   EventType = "binding.revoked"
	EventTypeBindingRefreshed EventType = "binding.refreshed"

	// Action install event (ADR-0003 + #366). Payload carries the
	// action name, FQN, version, source, and on-disk path.
	EventTypeActionInstalled EventType = "action.installed"

	// Connector install event (ADR-0007). Payload carries the FQN,
	// version, hash, signature status, and the consent decision per
	// the install-consent acceptance criteria.
	EventTypeConnectorInstalled EventType = "connector.installed"

	// Connector operation event for generated sandbox shims. Payload
	// identifies the spec-declared operation and mediation decision,
	// never credential values.
	EventTypeConnectorOperationRejected EventType = "connector.operation.rejected"

	// Connector proxy event for HTTPS data-plane attempts. Payload
	// identifies the spec-declared operation and upstream destination,
	// never credential values.
	EventTypeConnectorProxyRejected EventType = "connector.proxy.rejected"
	EventTypeConnectorProxyProxied  EventType = "connector.proxy.proxied"

	// Sandbox proxy event for HTTPS data-plane attempts rejected before
	// a connector operation could be uniquely resolved.
	EventTypeSandboxProxyRejected EventType = "sandbox.proxy.rejected"

	// Sandbox shell mediation event. Payload identifies the shell command
	// decision without recording environment variables or command output.
	EventTypeSandboxShellDecided EventType = "sandbox.shell.decided"
)
