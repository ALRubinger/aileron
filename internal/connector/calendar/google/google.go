// Package google implements the calendar connector for Google Calendar.
//
// This connector executes calendar.event.* actions via the Google Calendar
// API. Credentials (OAuth refresh token) are injected at execution time
// from the vault.
package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ALRubinger/aileron/internal/connector"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	cal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

const (
	connectorType     = "calendar"
	connectorProvider = "google_calendar"
)

// Connector executes calendar actions via Google Calendar.
type Connector struct {
	clientID     string
	clientSecret string
	endpoint     string // override Calendar API base URL (for testing)
}

// New returns a new Google Calendar connector.
// clientID and clientSecret are required for OAuth token refresh.
func New(clientID, clientSecret string) *Connector {
	return &Connector{
		clientID:     clientID,
		clientSecret: clientSecret,
	}
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

func (c *Connector) createEvent(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	svc, err := c.newService(ctx, req.Credential.Value)
	if err != nil {
		return failResult("google_calendar: " + err.Error()), nil
	}

	event := buildEvent(req.Parameters)
	calendarID := paramStr(req.Parameters, "calendar_id")
	if calendarID == "" {
		calendarID = "primary"
	}

	insertCall := svc.Events.Insert(calendarID, event)
	if paramStr(req.Parameters, "conference_type") == "google_meet" {
		insertCall.ConferenceDataVersion(1)
	}

	created, err := insertCall.Context(ctx).Do()
	if err != nil {
		return failResult("google_calendar: events.insert: " + err.Error()), nil
	}

	return connector.ExecutionResult{
		Status:     connector.ExecutionStatusSucceeded,
		ReceiptRef: created.HtmlLink,
		Output: map[string]any{
			"event_id":   created.Id,
			"event_link": created.HtmlLink,
		},
	}, nil
}

func (c *Connector) updateEvent(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	svc, err := c.newService(ctx, req.Credential.Value)
	if err != nil {
		return failResult("google_calendar: " + err.Error()), nil
	}

	eventID := paramStr(req.Parameters, "event_id")
	if eventID == "" {
		return failResult("google_calendar: event_id parameter required"), nil
	}

	calendarID := paramStr(req.Parameters, "calendar_id")
	if calendarID == "" {
		calendarID = "primary"
	}

	event := buildEvent(req.Parameters)
	updated, err := svc.Events.Update(calendarID, eventID, event).Context(ctx).Do()
	if err != nil {
		return failResult("google_calendar: events.update: " + err.Error()), nil
	}

	return connector.ExecutionResult{
		Status:     connector.ExecutionStatusSucceeded,
		ReceiptRef: updated.HtmlLink,
		Output: map[string]any{
			"event_id":   updated.Id,
			"event_link": updated.HtmlLink,
		},
	}, nil
}

func (c *Connector) deleteEvent(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	svc, err := c.newService(ctx, req.Credential.Value)
	if err != nil {
		return failResult("google_calendar: " + err.Error()), nil
	}

	eventID := paramStr(req.Parameters, "event_id")
	if eventID == "" {
		return failResult("google_calendar: event_id parameter required"), nil
	}

	calendarID := paramStr(req.Parameters, "calendar_id")
	if calendarID == "" {
		calendarID = "primary"
	}

	if err := svc.Events.Delete(calendarID, eventID).Context(ctx).Do(); err != nil {
		return failResult("google_calendar: events.delete: " + err.Error()), nil
	}

	return connector.ExecutionResult{
		Status: connector.ExecutionStatusSucceeded,
		Output: map[string]any{"deleted": true},
	}, nil
}

func (c *Connector) newService(ctx context.Context, tokenJSON []byte) (*cal.Service, error) {
	ts, err := c.tokenSource(ctx, tokenJSON)
	if err != nil {
		return nil, err
	}
	opts := []option.ClientOption{option.WithTokenSource(ts)}
	if c.endpoint != "" {
		opts = append(opts, option.WithEndpoint(c.endpoint))
	}
	return cal.NewService(ctx, opts...)
}

func (c *Connector) tokenSource(ctx context.Context, tokenJSON []byte) (oauth2.TokenSource, error) {
	var tok oauth2.Token
	if err := json.Unmarshal(tokenJSON, &tok); err != nil {
		return nil, fmt.Errorf("invalid token JSON: %w", err)
	}
	if tok.RefreshToken == "" && tok.AccessToken == "" {
		return nil, fmt.Errorf("token has neither access_token nor refresh_token")
	}
	oauthConfig := &oauth2.Config{
		ClientID:     c.clientID,
		ClientSecret: c.clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{cal.CalendarScope, cal.CalendarEventsScope},
	}
	return oauthConfig.TokenSource(ctx, &tok), nil
}

func buildEvent(params map[string]any) *cal.Event {
	event := &cal.Event{}

	if title := paramStr(params, "title"); title != "" {
		event.Summary = title
	}
	if desc := paramStr(params, "description"); desc != "" {
		event.Description = desc
	}
	if loc := paramStr(params, "location"); loc != "" {
		event.Location = loc
	}
	if vis := paramStr(params, "visibility"); vis != "" {
		event.Visibility = vis
	}

	tz := paramStr(params, "timezone")

	if startTime := paramStr(params, "start_time"); startTime != "" {
		event.Start = &cal.EventDateTime{DateTime: startTime, TimeZone: tz}
	}
	if endTime := paramStr(params, "end_time"); endTime != "" {
		event.End = &cal.EventDateTime{DateTime: endTime, TimeZone: tz}
	}

	if attendees, ok := params["attendees"]; ok {
		if attendeeList, ok := attendees.([]any); ok {
			for _, a := range attendeeList {
				if am, ok := a.(map[string]any); ok {
					attendee := &cal.EventAttendee{}
					if email, ok := am["email"].(string); ok {
						attendee.Email = email
					}
					if name, ok := am["name"].(string); ok {
						attendee.DisplayName = name
					}
					if opt, ok := am["optional"].(bool); ok {
						attendee.Optional = opt
					}
					event.Attendees = append(event.Attendees, attendee)
				}
			}
		}
	}

	if paramStr(params, "conference_type") == "google_meet" {
		event.ConferenceData = &cal.ConferenceData{
			CreateRequest: &cal.CreateConferenceRequest{
				RequestId: paramStr(params, "conference_request_id"),
				ConferenceSolutionKey: &cal.ConferenceSolutionKey{
					Type: "hangoutsMeet",
				},
			},
		}
		if event.ConferenceData.CreateRequest.RequestId == "" {
			// Generate a unique request ID from the grant ID or intent ID.
			event.ConferenceData.CreateRequest.RequestId = paramStr(params, "action_type")
		}
	}

	return event
}

func paramStr(params map[string]any, key string) string {
	v, ok := params[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func failResult(msg string) connector.ExecutionResult {
	return connector.ExecutionResult{
		Status: connector.ExecutionStatusFailed,
		Error:  msg,
	}
}
