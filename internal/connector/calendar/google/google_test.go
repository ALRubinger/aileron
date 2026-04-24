package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/connector"
	"golang.org/x/oauth2"
)

func validToken() []byte {
	tok := oauth2.Token{AccessToken: "test-access-token"}
	b, _ := json.Marshal(tok)
	return b
}

func newTestConnector(serverURL string) *Connector {
	return &Connector{
		clientID:     "test-client-id",
		clientSecret: "test-client-secret",
		endpoint:     serverURL,
	}
}

func TestConnector_TypeAndProvider(t *testing.T) {
	c := New("", "")
	if c.Type() != "calendar" {
		t.Errorf("Type() = %q, want %q", c.Type(), "calendar")
	}
	if c.Provider() != "google_calendar" {
		t.Errorf("Provider() = %q, want %q", c.Provider(), "google_calendar")
	}
}

func TestConnector_NoCredential(t *testing.T) {
	c := New("", "")
	_, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.create",
	})
	if err == nil {
		t.Fatal("expected error for nil credential")
	}
}

func TestConnector_UnsupportedAction(t *testing.T) {
	c := New("", "")
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.unknown",
		Credential: &connector.InjectedCredential{Value: validToken()},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestConnector_InvalidToken(t *testing.T) {
	c := newTestConnector("http://unused")
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.create",
		Credential: &connector.InjectedCredential{Value: []byte("not-json")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestConnector_EmptyToken(t *testing.T) {
	c := newTestConnector("http://unused")
	emptyTok, _ := json.Marshal(oauth2.Token{})
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.create",
		Credential: &connector.InjectedCredential{Value: emptyTok},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed for empty token", result.Status)
	}
}

func TestCreateEvent_Success(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":       "event123",
			"htmlLink": "https://calendar.google.com/event?eid=event123",
		})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.create",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"title":       "Team Standup",
			"description": "Daily sync",
			"start_time":  "2026-04-25T10:00:00",
			"end_time":    "2026-04-25T10:30:00",
			"timezone":    "America/New_York",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
	if result.ReceiptRef != "https://calendar.google.com/event?eid=event123" {
		t.Errorf("ReceiptRef = %q", result.ReceiptRef)
	}
	if result.Output["event_id"] != "event123" {
		t.Errorf("event_id = %v", result.Output["event_id"])
	}
	if !strings.Contains(receivedPath, "/calendars/primary/events") {
		t.Errorf("path = %q, want /calendars/primary/events", receivedPath)
	}
}

func TestCreateEvent_CustomCalendarID(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "ev1", "htmlLink": "link"})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.create",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"title":       "Meeting",
			"calendar_id": "team@group.calendar.google.com",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
	if !strings.Contains(receivedPath, "team@group.calendar.google.com") {
		t.Errorf("path = %q, expected custom calendar ID", receivedPath)
	}
}

func TestCreateEvent_WithGoogleMeet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify conferenceDataVersion query param is set.
		if r.URL.Query().Get("conferenceDataVersion") != "1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "ev_meet", "htmlLink": "link"})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.create",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"title":           "Video Call",
			"conference_type": "google_meet",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
}

func TestCreateEvent_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"Calendar access denied"}}`))
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.create",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{"title": "Test"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "events.insert") {
		t.Errorf("Error = %q, want mention of events.insert", result.Error)
	}
}

func TestUpdateEvent_Success(t *testing.T) {
	var receivedPath, receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":       "event123",
			"htmlLink": "https://calendar.google.com/event?eid=event123",
		})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.update",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"event_id": "event123",
			"title":    "Updated Standup",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
	if receivedMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", receivedMethod)
	}
	if !strings.Contains(receivedPath, "/calendars/primary/events/event123") {
		t.Errorf("path = %q", receivedPath)
	}
}

func TestUpdateEvent_MissingEventID(t *testing.T) {
	c := newTestConnector("http://unused")
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.update",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{"title": "Updated Title"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestUpdateEvent_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"Not Found"}}`))
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.update",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"event_id": "nonexistent",
			"title":    "Updated",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestDeleteEvent_Success(t *testing.T) {
	var receivedPath, receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.delete",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"event_id":    "event_to_delete",
			"calendar_id": "work@group.calendar.google.com",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
	if result.Output["deleted"] != true {
		t.Errorf("deleted = %v", result.Output["deleted"])
	}
	if receivedMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", receivedMethod)
	}
	if !strings.Contains(receivedPath, "event_to_delete") {
		t.Errorf("path = %q, want event_to_delete in path", receivedPath)
	}
}

func TestDeleteEvent_MissingEventID(t *testing.T) {
	c := newTestConnector("http://unused")
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.delete",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestDeleteEvent_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"Not Found"}}`))
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "calendar.event.delete",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{"event_id": "gone"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestBuildEvent_Full(t *testing.T) {
	event := buildEvent(map[string]any{
		"title":           "Team Standup",
		"description":     "Daily sync",
		"location":        "Room B",
		"start_time":      "2026-04-25T10:00:00",
		"end_time":        "2026-04-25T10:30:00",
		"timezone":        "America/New_York",
		"visibility":      "private",
		"conference_type": "google_meet",
		"attendees": []any{
			map[string]any{"email": "alice@example.com", "name": "Alice", "optional": true},
			map[string]any{"email": "bob@example.com"},
		},
	})

	if event.Summary != "Team Standup" {
		t.Errorf("Summary = %q", event.Summary)
	}
	if event.Description != "Daily sync" {
		t.Errorf("Description = %q", event.Description)
	}
	if event.Location != "Room B" {
		t.Errorf("Location = %q", event.Location)
	}
	if event.Visibility != "private" {
		t.Errorf("Visibility = %q", event.Visibility)
	}
	if event.Start == nil || event.Start.DateTime != "2026-04-25T10:00:00" {
		t.Errorf("Start = %+v", event.Start)
	}
	if event.Start.TimeZone != "America/New_York" {
		t.Errorf("Start.TimeZone = %q", event.Start.TimeZone)
	}
	if event.End == nil || event.End.DateTime != "2026-04-25T10:30:00" {
		t.Errorf("End = %+v", event.End)
	}
	if len(event.Attendees) != 2 {
		t.Fatalf("Attendees count = %d, want 2", len(event.Attendees))
	}
	if event.Attendees[0].Email != "alice@example.com" {
		t.Errorf("Attendees[0].Email = %q", event.Attendees[0].Email)
	}
	if event.Attendees[0].DisplayName != "Alice" {
		t.Errorf("Attendees[0].DisplayName = %q", event.Attendees[0].DisplayName)
	}
	if !event.Attendees[0].Optional {
		t.Error("Attendees[0].Optional should be true")
	}
	if event.ConferenceData == nil || event.ConferenceData.CreateRequest == nil {
		t.Fatal("expected ConferenceData for google_meet")
	}
	if event.ConferenceData.CreateRequest.ConferenceSolutionKey.Type != "hangoutsMeet" {
		t.Errorf("ConferenceSolutionKey.Type = %q", event.ConferenceData.CreateRequest.ConferenceSolutionKey.Type)
	}
}

func TestBuildEvent_MinimalParams(t *testing.T) {
	event := buildEvent(map[string]any{"title": "Quick Chat"})
	if event.Summary != "Quick Chat" {
		t.Errorf("Summary = %q", event.Summary)
	}
	if event.Start != nil {
		t.Error("Start should be nil for missing start_time")
	}
	if event.Attendees != nil {
		t.Error("Attendees should be nil for missing attendees")
	}
	if event.ConferenceData != nil {
		t.Error("ConferenceData should be nil when no conference_type")
	}
}

func TestBuildEvent_NoConference(t *testing.T) {
	event := buildEvent(map[string]any{
		"title":           "Lunch",
		"conference_type": "none",
	})
	if event.ConferenceData != nil {
		t.Error("ConferenceData should be nil for conference_type=none")
	}
}

func TestParamStr(t *testing.T) {
	params := map[string]any{"title": "Test", "number": 42}
	if paramStr(params, "title") != "Test" {
		t.Errorf("paramStr title = %q", paramStr(params, "title"))
	}
	if paramStr(params, "number") != "42" {
		t.Errorf("paramStr number = %q", paramStr(params, "number"))
	}
	if paramStr(params, "missing") != "" {
		t.Errorf("paramStr missing = %q", paramStr(params, "missing"))
	}
}

func TestFailResult(t *testing.T) {
	r := failResult("test error")
	if r.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q", r.Status)
	}
	if r.Error != "test error" {
		t.Errorf("Error = %q", r.Error)
	}
}

func TestTokenSource_InvalidJSON(t *testing.T) {
	c := New("id", "secret")
	_, err := c.tokenSource(context.Background(), []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestTokenSource_EmptyToken(t *testing.T) {
	c := New("id", "secret")
	emptyTok, _ := json.Marshal(oauth2.Token{})
	_, err := c.tokenSource(context.Background(), emptyTok)
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestTokenSource_ValidToken(t *testing.T) {
	c := New("id", "secret")
	ts, err := c.tokenSource(context.Background(), validToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ts == nil {
		t.Fatal("expected non-nil token source")
	}
}
