package comms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
)

// slackOK is a minimal Slack API "ok" response.
func slackOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func TestSetAssistantStatus_EmptyToken(t *testing.T) {
	err := comms.SetAssistantStatus(context.Background(), "", "C123", "123.456", "Thinking...")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestSetAssistantStatus_Success(t *testing.T) {
	var receivedStatus string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		receivedStatus = r.FormValue("status")
		slackOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	err := comms.SetAssistantStatus(context.Background(), "xoxb-test", "C123", "123.456", "Researching...")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedStatus != "Researching..." {
		t.Errorf("expected status %q, got %q", "Researching...", receivedStatus)
	}
}

func TestSetAssistantSuggestedPrompts_EmptyToken(t *testing.T) {
	err := comms.SetAssistantSuggestedPrompts(context.Background(), "", "C123", "123.456", nil)
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestSetAssistantSuggestedPrompts_Success(t *testing.T) {
	var receivedPrompts string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		receivedPrompts = r.FormValue("prompts")
		slackOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	prompts := []comms.SlackAgentPrompt{
		{Title: "Draft a reply", Message: "Draft a reply to my latest @mention"},
		{Title: "Summarize", Message: "Summarize what I missed"},
	}
	err := comms.SetAssistantSuggestedPrompts(context.Background(), "xoxb-test", "C123", "123.456", prompts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify prompts were serialized as JSON.
	var parsed []map[string]string
	if err := json.Unmarshal([]byte(receivedPrompts), &parsed); err != nil {
		t.Fatalf("failed to parse prompts JSON: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(parsed))
	}
	if parsed[0]["title"] != "Draft a reply" {
		t.Errorf("expected first prompt title %q, got %q", "Draft a reply", parsed[0]["title"])
	}
}

func TestSetAssistantTitle_EmptyToken(t *testing.T) {
	err := comms.SetAssistantTitle(context.Background(), "", "C123", "123.456", "Aileron")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestSetAssistantTitle_Success(t *testing.T) {
	var receivedTitle string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		receivedTitle = r.FormValue("title")
		slackOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	err := comms.SetAssistantTitle(context.Background(), "xoxb-test", "C123", "123.456", "Aileron")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedTitle != "Aileron" {
		t.Errorf("expected title %q, got %q", "Aileron", receivedTitle)
	}
}

func TestStartStream_EmptyToken(t *testing.T) {
	_, err := comms.StartStream(context.Background(), "", "C123", "123.456")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestStartStream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": "C123",
			"ts":      "999.001",
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	ts, err := comms.StartStream(context.Background(), "xoxb-test", "C123", "123.456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ts != "999.001" {
		t.Errorf("expected ts %q, got %q", "999.001", ts)
	}
}

func TestAppendStream_EmptyToken(t *testing.T) {
	err := comms.AppendStream(context.Background(), "", "C123", "999.001", "hello")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestAppendStream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": "C123",
			"ts":      "999.001",
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	err := comms.AppendStream(context.Background(), "xoxb-test", "C123", "999.001", "more text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStopStream_EmptyToken(t *testing.T) {
	err := comms.StopStream(context.Background(), "", "C123", "999.001")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestStopStream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": "C123",
			"ts":      "999.001",
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	err := comms.StopStream(context.Background(), "xoxb-test", "C123", "999.001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenConversation_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"channel": map[string]any{
				"id": "D_DM_CHANNEL",
			},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	channelID, err := comms.OpenConversation(context.Background(), "xoxb-test", "U_ALICE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if channelID != "D_DM_CHANNEL" {
		t.Errorf("expected D_DM_CHANNEL, got %q", channelID)
	}
}

func TestResolveSlackDisplayName_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"user": map[string]any{
				"id":        "U_ALICE",
				"real_name": "Alice Smith",
				"profile": map[string]any{
					"display_name": "alice",
				},
			},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	name := comms.ResolveSlackDisplayName(context.Background(), "xoxb-test", "U_ALICE")
	if name != "alice" {
		t.Errorf("expected 'alice', got %q", name)
	}
}

func TestResolveSlackDisplayName_FallsBackToRealName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"user": map[string]any{
				"id":        "U_BOB",
				"real_name": "Bob Jones",
				"profile": map[string]any{
					"display_name": "",
				},
			},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	name := comms.ResolveSlackDisplayName(context.Background(), "xoxb-test", "U_BOB")
	if name != "Bob Jones" {
		t.Errorf("expected 'Bob Jones', got %q", name)
	}
}

func TestResolveSlackDisplayName_FallsBackToUserID(t *testing.T) {
	// Empty token — can't call API, returns raw user ID.
	name := comms.ResolveSlackDisplayName(context.Background(), "", "U_ALICE")
	if name != "U_ALICE" {
		t.Errorf("expected 'U_ALICE', got %q", name)
	}
}

func TestResolveSlackDisplayName_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	name := comms.ResolveSlackDisplayName(context.Background(), "xoxb-test", "U_UNKNOWN")
	if name != "U_UNKNOWN" {
		t.Errorf("expected fallback to 'U_UNKNOWN', got %q", name)
	}
}

func TestOpenModal_EmptyToken(t *testing.T) {
	_, err := comms.OpenModal(context.Background(), "", "trigger123", comms.EmptyModalView())
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestUpdateModal_EmptyToken(t *testing.T) {
	_, err := comms.UpdateModal(context.Background(), "", "view123", comms.EmptyModalView())
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestOpenConversation_EmptyToken(t *testing.T) {
	_, err := comms.OpenConversation(context.Background(), "", "U123")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}
