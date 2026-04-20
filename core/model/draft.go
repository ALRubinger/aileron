package model

import "time"

// DraftStatus tracks the lifecycle state of a draft reply.
type DraftStatus string

const (
	DraftStatusPending   DraftStatus = "pending"
	DraftStatusApproved  DraftStatus = "approved"
	DraftStatusEdited    DraftStatus = "edited"
	DraftStatusDiscarded DraftStatus = "discarded"
)

// Draft represents an AI-generated reply awaiting user review.
// Created by the draft generation pipeline when a message arrives.
// The user reviews and either approves (send as-is), edits (send
// revised text), or discards (don't send).
type Draft struct {
	ID          string      `json:"id"`           // dft_ + UUID
	UserID      string      `json:"user_id"`      // owning user (usr_ + UUID)
	Status      DraftStatus `json:"status"`       // pending → approved | edited | discarded
	Service     string      `json:"service"`      // "slack", "discord"
	Channel     string      `json:"channel"`      // channel ID where the original message arrived
	Author      string      `json:"author"`       // who sent the original message
	MessageBody string      `json:"message_body"` // the original message text
	MessageTS   string      `json:"message_ts"`   // original message timestamp (for threading)
	DraftBody   string      `json:"draft_body"`   // the AI-generated draft reply
	SentBody    string      `json:"sent_body"`    // what was actually sent (empty until approved/edited)
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}
