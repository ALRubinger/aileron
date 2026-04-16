package calendar_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	calendarsource "github.com/ALRubinger/aileron/core/source/calendar"
	"google.golang.org/api/option"
)

func testToken() []byte {
	b, _ := json.Marshal(map[string]string{"access_token": "test-token", "token_type": "bearer"})
	return b
}

func TestConnector_Provider(t *testing.T) {
	c := calendarsource.New()
	if c.Provider() != "google_calendar" {
		t.Errorf("expected google_calendar, got %s", c.Provider())
	}
}

func TestConnector_Tools(t *testing.T) {
	c := calendarsource.New()
	tools := c.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if !names["calendar_events"] {
		t.Error("expected calendar_events tool")
	}
	if !names["calendar_free_busy"] {
		t.Error("expected calendar_free_busy tool")
	}
}

func TestConnector_UnknownTool(t *testing.T) {
	c := calendarsource.New()
	token, _ := json.Marshal(map[string]string{"access_token": "test"})
	_, err := c.Execute(context.Background(), "calendar_nonexistent", nil, token)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestConnector_InvalidToken(t *testing.T) {
	c := calendarsource.New()
	_, err := c.Execute(context.Background(), "calendar_events", map[string]any{
		"start": "2026-04-15T00:00:00Z",
		"end":   "2026-04-16T00:00:00Z",
	}, []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestConnector_EmptyAccessToken(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"token_type": "bearer"})
	c := calendarsource.New()
	_, err := c.Execute(context.Background(), "calendar_events", map[string]any{
		"start": "2026-04-15T00:00:00Z",
		"end":   "2026-04-16T00:00:00Z",
	}, token)
	if err == nil {
		t.Fatal("expected error for missing access_token")
	}
}

func TestConnector_Events_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"id":      "evt_1",
					"summary": "Team standup",
					"status":  "confirmed",
					"start":   map[string]any{"dateTime": "2026-04-15T10:00:00-07:00"},
					"end":     map[string]any{"dateTime": "2026-04-15T10:30:00-07:00"},
					"location": "Room 42",
					"attendees": []map[string]any{
						{"email": "sarah@example.com"},
						{"email": "you@example.com"},
					},
				},
			},
		})
	}))
	defer server.Close()

	c := calendarsource.New().WithClientOption(option.WithEndpoint(server.URL))
	result, err := c.Execute(context.Background(), "calendar_events", map[string]any{
		"start": "2026-04-15T00:00:00Z",
		"end":   "2026-04-16T00:00:00Z",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := result["events"].([]map[string]any)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0]["summary"] != "Team standup" {
		t.Errorf("expected Team standup, got %v", events[0]["summary"])
	}
	if events[0]["location"] != "Room 42" {
		t.Errorf("expected Room 42, got %v", events[0]["location"])
	}
}

func TestConnector_FreeBusy_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"calendars": map[string]any{
				"primary": map[string]any{
					"busy": []map[string]any{
						{"start": "2026-04-15T10:00:00Z", "end": "2026-04-15T10:30:00Z"},
						{"start": "2026-04-15T14:00:00Z", "end": "2026-04-15T15:00:00Z"},
					},
				},
			},
		})
	}))
	defer server.Close()

	c := calendarsource.New().WithClientOption(option.WithEndpoint(server.URL))
	result, err := c.Execute(context.Background(), "calendar_free_busy", map[string]any{
		"start": "2026-04-15T00:00:00Z",
		"end":   "2026-04-16T00:00:00Z",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	busy := result["busy"].([]map[string]any)
	if len(busy) != 2 {
		t.Fatalf("expected 2 busy slots, got %d", len(busy))
	}
}

func TestConnector_Events_WithMaxResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	}))
	defer server.Close()

	c := calendarsource.New().WithClientOption(option.WithEndpoint(server.URL))
	_, err := c.Execute(context.Background(), "calendar_events", map[string]any{
		"start":       "2026-04-15T00:00:00Z",
		"end":         "2026-04-16T00:00:00Z",
		"max_results": float64(5),
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnector_OAuthTokenFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	}))
	defer server.Close()

	token, _ := json.Marshal(map[string]any{
		"access_token": "oauth2-format-token",
		"token_type":   "bearer",
		"expiry":       "2026-12-31T00:00:00Z",
	})
	c := calendarsource.New().WithClientOption(option.WithEndpoint(server.URL))
	_, err := c.Execute(context.Background(), "calendar_events", map[string]any{
		"start": "2026-04-15T00:00:00Z",
		"end":   "2026-04-16T00:00:00Z",
	}, token)
	if err != nil {
		t.Fatalf("unexpected error with oauth2 token format: %v", err)
	}
}

func TestConnector_Events_MissingStart(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"access_token": "test"})
	c := calendarsource.New()
	_, err := c.Execute(context.Background(), "calendar_events", map[string]any{
		"end": "2026-04-16T00:00:00Z",
	}, token)
	if err == nil {
		t.Fatal("expected error for missing start")
	}
}

func TestConnector_Events_MissingEnd(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"access_token": "test"})
	c := calendarsource.New()
	_, err := c.Execute(context.Background(), "calendar_events", map[string]any{
		"start": "2026-04-15T00:00:00Z",
	}, token)
	if err == nil {
		t.Fatal("expected error for missing end")
	}
}

func TestConnector_FreeBusy_MissingStart(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"access_token": "test"})
	c := calendarsource.New()
	_, err := c.Execute(context.Background(), "calendar_free_busy", map[string]any{
		"end": "2026-04-16T00:00:00Z",
	}, token)
	if err == nil {
		t.Fatal("expected error for missing start")
	}
}

func TestConnector_FreeBusy_MissingEnd(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"access_token": "test"})
	c := calendarsource.New()
	_, err := c.Execute(context.Background(), "calendar_free_busy", map[string]any{
		"start": "2026-04-15T00:00:00Z",
	}, token)
	if err == nil {
		t.Fatal("expected error for missing end")
	}
}
