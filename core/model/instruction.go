package model

import "time"

// UserInstruction is an explicit rule the user gives Aileron for how
// to communicate on their behalf. Instructions are included in the LLM
// system prompt during draft generation — they are the highest-priority
// context, overriding learned patterns and behavioral model inferences.
type UserInstruction struct {
	ID        string    // ins_ + UUID
	UserID    string    // owning user (usr_ + UUID)
	Body      string    // the instruction text
	Scope     string    // optional free-text context: channel, person, topic, or empty for global
	Active    bool      // toggleable without deleting
	CreatedAt time.Time
	UpdatedAt time.Time
}
