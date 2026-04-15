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
	ID          string      // dft_ + UUID
	UserID      string      // owning user (usr_ + UUID)
	Status      DraftStatus // pending → approved | edited | discarded
	Service     string      // "slack", "discord"
	Channel     string      // channel ID where the original message arrived
	Author      string      // who sent the original message
	MessageBody string      // the original message text
	MessageTS   string      // original message timestamp (for threading)
	DraftBody   string      // the AI-generated draft reply
	SentBody    string      // what was actually sent (empty until approved/edited)
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
