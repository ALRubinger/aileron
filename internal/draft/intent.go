package draft

import "github.com/ALRubinger/aileron/internal/model"

// IntentType classifies what the user is asking for.
type IntentType string

const (
	// IntentTypeInformational is a question or status request — no write action.
	IntentTypeInformational IntentType = "informational"

	// Write action intent types — these map to ActionIntent.Type values
	// and flow through the intent → policy → approval → execution pipeline.
	IntentTypeEmailSend          IntentType = "email.send"
	IntentTypeEmailDraft         IntentType = "email.draft"
	IntentTypeCalendarEventCreate IntentType = "calendar.event.create"
	IntentTypeGitIssueCreate     IntentType = "git.issue.create"
	IntentTypeGitIssueComment    IntentType = "git.issue.comment"
	IntentTypeSlackSend          IntentType = "comms.slack.send"
)

// ResolvedIntent is the output of the intent resolution pipeline.
// It represents what the user wants to do, with structured parameters
// for write actions or a text response for informational queries.
type ResolvedIntent struct {
	// Type classifies the intent: informational or a specific write action.
	Type IntentType `json:"type"`

	// Action is the structured intent for write actions. Nil for informational.
	Action *model.ActionIntent `json:"action,omitempty"`

	// Summary is a human-readable description of what will happen.
	// For write actions: "Sending an email to alice@example.com about..."
	// For informational: the text response to display.
	Summary string `json:"summary"`

	// ResponseText is the ghostwritten text for informational intents.
	// Empty for write actions (the approval UI renders the structured action).
	ResponseText string `json:"response_text,omitempty"`
}

// IsWriteAction returns true if this intent represents an action that
// requires human approval and execution via a connector.
func (r ResolvedIntent) IsWriteAction() bool {
	return r.Type != IntentTypeInformational
}
