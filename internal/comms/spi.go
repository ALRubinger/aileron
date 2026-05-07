// Package comms defines the SPI for bidirectional communication channels
// (a future channel listener implements [Listener]) and provides shared
// helpers — the notify queue, listener registry, and vault-reference
// resolution — that any channel implementation can build on.
package comms

import (
	"context"
	"time"
)

// IncomingMessage represents a message received from a comms channel.
type IncomingMessage struct {
	ID        string
	Service   string // "slack", "discord"
	Channel   string
	Author    string
	Body      string
	Timestamp time.Time
}

// OutgoingMessage represents a message to send to a comms channel.
type OutgoingMessage struct {
	Channel string
	Body    string
}

// Listener connects to a comms service and delivers incoming messages
// via a channel. Implementations handle the service-specific protocol
// (Slack Socket Mode, Discord Gateway, etc.).
type Listener interface {
	// Connect establishes the connection to the service. Call before Listen.
	Connect(ctx context.Context) error

	// Listen starts delivering messages. Blocks until the context is
	// cancelled or the connection drops. Incoming messages are sent to
	// the returned channel.
	Listen(ctx context.Context) (<-chan IncomingMessage, error)

	// Send posts a message to the given channel.
	Send(ctx context.Context, msg OutgoingMessage) error

	// Close shuts down the connection.
	Close() error

	// Service returns the service name (e.g. "slack", "discord").
	Service() string
}
