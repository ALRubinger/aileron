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
	ID        string         // fb_ + UUID
	UserID    string         // owning user
	DraftID   string         // the draft this feedback is for
	Signal    FeedbackSignal // approved, edited, discarded
	Service   string         // "slack", etc.
	Channel   string         // where the original message was
	DraftBody string         // what the AI generated
	SentBody  string         // what was actually sent (empty if discarded)
	ToolsUsed []string       // source connector tools called during generation
	CreatedAt time.Time
}
