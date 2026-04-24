package model

import "time"

// UserInstruction is an explicit rule the user gives Aileron for how
// to communicate on their behalf. Instructions are included in the LLM
// system prompt during draft generation — they are the highest-priority
// context, overriding learned patterns and behavioral model inferences.
type UserInstruction struct {
	ID        string    `json:"id"`         // ins_ + UUID
	UserID    string    `json:"user_id"`    // owning user (usr_ + UUID)
	Body      string    `json:"body"`       // the instruction text
	Scope     string    `json:"scope"`      // optional free-text context: channel, person, topic, or empty for global
	Active    bool      `json:"active"`     // toggleable without deleting
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
