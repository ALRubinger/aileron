package model

import "time"

// FeedbackSignal identifies the type of user action on a draft.
type FeedbackSignal string

const (
	FeedbackSignalApproved  FeedbackSignal = "approved"
	FeedbackSignalEdited    FeedbackSignal = "edited"
	FeedbackSignalDiscarded FeedbackSignal = "discarded"
)

// DraftFeedback records a user's action on a draft reply. These signals
// feed the behavioral model — approved drafts reinforce context assembly
// and tone, edited drafts provide correction signals (the diff between
// DraftBody and SentBody), and discarded drafts are negative signals.
type DraftFeedback struct {
	ID        string         `json:"id"`         // fb_ + UUID
	UserID    string         `json:"user_id"`    // owning user
	DraftID   string         `json:"draft_id"`   // the draft this feedback is for
	Signal    FeedbackSignal `json:"signal"`     // approved, edited, discarded
	Service   string         `json:"service"`    // "slack", etc.
	Channel   string         `json:"channel"`    // where the original message was
	DraftBody string         `json:"draft_body"` // what the AI generated
	SentBody  string         `json:"sent_body"`  // what was actually sent (empty if discarded)
	ToolsUsed []string       `json:"tools_used"` // source connector tools called during generation
	CreatedAt time.Time      `json:"created_at"`
}
