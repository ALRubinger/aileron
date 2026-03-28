// Package google implements the calendar connector for Google Calendar.
//
// This connector executes calendar.event.* actions via the Google Calendar
// API. Credentials (OAuth refresh token or service account) are injected at
// execution time from the vault.
package google

import (
	"context"
	"errors"

	"github.com/ALRubinger/aileron/core/connector"
)

const (
	connectorType     = "calendar"
	connectorProvider = "google_calendar"
)

// Connector executes calendar actions via Google Calendar.
type Connector struct{}

// New returns a new Google Calendar connector.
func New() *Connector {
	return &Connector{}
}

// Type implements connector.Connector.
func (c *Connector) Type() string { return connectorType }

// Provider implements connector.Connector.
func (c *Connector) Provider() string { return connectorProvider }

// Execute implements connector.Connector.
func (c *Connector) Execute(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	if req.Credential == nil {
		return connector.ExecutionResult{}, errors.New("google_calendar: no credential injected")
	}

	switch req.ActionType {
	case "calendar.event.create":
		return c.createEvent(ctx, req)
	case "calendar.event.update":
		return c.updateEvent(ctx, req)
	case "calendar.event.delete":
		return c.deleteEvent(ctx, req)
	default:
		return connector.ExecutionResult{
			Status: connector.ExecutionStatusFailed,
			Error:  "google_calendar: unsupported action type: " + req.ActionType,
		}, nil
	}
}

func (c *Connector) createEvent(_ context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	// TODO: implement via Google Calendar API events.insert.
	// Steps:
	//   1. Exchange OAuth refresh token for access token.
	//   2. Build calendar.Event from req.Parameters.
	//   3. Call events.insert on the target calendar.
	//   4. Return the event ID as ReceiptRef.
	_ = req
	return connector.ExecutionResult{}, errors.New("google_calendar: createEvent not yet implemented")
}

func (c *Connector) updateEvent(_ context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	// TODO: implement via Google Calendar API events.update.
	_ = req
	return connector.ExecutionResult{}, errors.New("google_calendar: updateEvent not yet implemented")
}

func (c *Connector) deleteEvent(_ context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	// TODO: implement via Google Calendar API events.delete.
	_ = req
	return connector.ExecutionResult{}, errors.New("google_calendar: deleteEvent not yet implemented")
}
