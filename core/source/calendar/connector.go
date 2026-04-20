// Package calendar implements a SourceConnector for Google Calendar, providing
// read-only tools for checking events and availability.
package calendar

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ALRubinger/aileron/core/source"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	cal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// Connector implements source.SourceConnector for Google Calendar.
type Connector struct {
	oauthConfig  *oauth2.Config
	clientOption option.ClientOption
}

// New creates a new Calendar source connector with OAuth credentials for token refresh.
func New(clientID, clientSecret string) *Connector {
	return &Connector{
		oauthConfig: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     google.Endpoint,
		},
	}
}

// WithClientOption returns a copy with a custom client option (for testing).
func (c *Connector) WithClientOption(opt option.ClientOption) *Connector {
	cp := *c
	cp.clientOption = opt
	return &cp
}

// WithTokenEndpoint returns a copy that uses the given URL for OAuth token
// refresh instead of Google's production endpoint. For testing only.
func (c *Connector) WithTokenEndpoint(tokenURL string) *Connector {
	cp := *c
	cfg := *c.oauthConfig
	cfg.Endpoint = oauth2.Endpoint{
		AuthURL:  cfg.Endpoint.AuthURL,
		TokenURL: tokenURL,
	}
	cp.oauthConfig = &cfg
	return &cp
}

func (c *Connector) Provider() string { return "google_calendar" }

func (c *Connector) Tools() []source.ToolDefinition {
	return []source.ToolDefinition{
		{
			Name:        "calendar_events",
			Description: "Get calendar events in a date range. Returns event titles, times, attendees, and locations.",
			Parameters: []source.ToolParam{
				{Name: "start", Type: "string", Description: "Start date/time in ISO 8601 format (e.g. 2026-04-15T00:00:00Z)", Required: true},
				{Name: "end", Type: "string", Description: "End date/time in ISO 8601 format", Required: true},
				{Name: "max_results", Type: "integer", Description: "Maximum events to return (default 20, max 50)", Required: false},
			},
		},
		{
			Name:        "calendar_free_busy",
			Description: "Check free/busy status for a time range. Returns busy time slots.",
			Parameters: []source.ToolParam{
				{Name: "start", Type: "string", Description: "Start date/time in ISO 8601 format", Required: true},
				{Name: "end", Type: "string", Description: "End date/time in ISO 8601 format", Required: true},
			},
		},
	}
}

func (c *Connector) Execute(ctx context.Context, tool string, params map[string]any, token []byte) (map[string]any, error) {
	svc, err := c.newService(ctx, token)
	if err != nil {
		return nil, err
	}

	switch tool {
	case "calendar_events":
		return c.events(ctx, svc, params)
	case "calendar_free_busy":
		return c.freeBusy(ctx, svc, params)
	default:
		return nil, fmt.Errorf("unknown tool: %s", tool)
	}
}

func (c *Connector) newService(ctx context.Context, token []byte) (*cal.Service, error) {
	ts, err := c.tokenSource(ctx, token)
	if err != nil {
		return nil, err
	}

	opts := []option.ClientOption{option.WithTokenSource(ts)}
	if c.clientOption != nil {
		opts = append(opts, c.clientOption)
	}
	svc, err := cal.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating calendar service: %w", err)
	}
	return svc, nil
}

func (c *Connector) events(ctx context.Context, svc *cal.Service, params map[string]any) (map[string]any, error) {
	start, ok := params["start"].(string)
	if !ok || start == "" {
		return nil, fmt.Errorf("start parameter is required")
	}
	end, ok := params["end"].(string)
	if !ok || end == "" {
		return nil, fmt.Errorf("end parameter is required")
	}

	maxResults := int64(20)
	if mr, ok := params["max_results"]; ok {
		maxResults = toInt64(mr, 20)
	}
	if maxResults > 50 {
		maxResults = 50
	}

	resp, err := svc.Events.List("primary").
		TimeMin(start).TimeMax(end).
		MaxResults(maxResults).
		SingleEvents(true).
		OrderBy("startTime").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("calendar API error: %w", err)
	}

	events := make([]map[string]any, 0, len(resp.Items))
	for _, evt := range resp.Items {
		item := map[string]any{
			"id":      evt.Id,
			"summary": evt.Summary,
			"status":  evt.Status,
		}
		if evt.Start != nil {
			if evt.Start.DateTime != "" {
				item["start"] = evt.Start.DateTime
			} else {
				item["start"] = evt.Start.Date
			}
		}
		if evt.End != nil {
			if evt.End.DateTime != "" {
				item["end"] = evt.End.DateTime
			} else {
				item["end"] = evt.End.Date
			}
		}
		if evt.Location != "" {
			item["location"] = evt.Location
		}
		if len(evt.Attendees) > 0 {
			attendees := make([]string, 0, len(evt.Attendees))
			for _, a := range evt.Attendees {
				attendees = append(attendees, a.Email)
			}
			item["attendees"] = attendees
		}
		events = append(events, item)
	}

	return map[string]any{"events": events}, nil
}

func (c *Connector) freeBusy(ctx context.Context, svc *cal.Service, params map[string]any) (map[string]any, error) {
	start, ok := params["start"].(string)
	if !ok || start == "" {
		return nil, fmt.Errorf("start parameter is required")
	}
	end, ok := params["end"].(string)
	if !ok || end == "" {
		return nil, fmt.Errorf("end parameter is required")
	}

	resp, err := svc.Freebusy.Query(&cal.FreeBusyRequest{
		TimeMin: start,
		TimeMax: end,
		Items: []*cal.FreeBusyRequestItem{
			{Id: "primary"},
		},
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("calendar API error: %w", err)
	}

	var busySlots []map[string]any
	if cal, ok := resp.Calendars["primary"]; ok {
		for _, busy := range cal.Busy {
			busySlots = append(busySlots, map[string]any{
				"start": busy.Start,
				"end":   busy.End,
			})
		}
	}

	return map[string]any{"busy": busySlots}, nil
}

// tokenSource parses the stored OAuth token and returns a token source that
// automatically refreshes expired access tokens using the OAuth client credentials.
func (c *Connector) tokenSource(ctx context.Context, tokenJSON []byte) (oauth2.TokenSource, error) {
	var tok oauth2.Token
	if err := json.Unmarshal(tokenJSON, &tok); err != nil {
		return nil, fmt.Errorf("invalid token JSON: %w", err)
	}
	if tok.RefreshToken == "" && tok.AccessToken == "" {
		return nil, fmt.Errorf("token has neither access_token nor refresh_token")
	}
	return c.oauthConfig.TokenSource(ctx, &tok), nil
}

func toInt64(v any, def int64) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return def
		}
		return i
	default:
		return def
	}
}
