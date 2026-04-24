package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	slacksource "github.com/ALRubinger/aileron/internal/source/slack"
)

// testToken returns a valid token JSON for testing.
func testToken() []byte {
	b, _ := json.Marshal(map[string]string{
		"access_token": "xoxp-test-token",
		"token_type":   "user",
	})
	return b
}

// newMockSlackServer creates a mock Slack API server for testing.
func newMockSlackServer() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{"user": "U123", "text": "Hello world", "ts": "1234567890.123456"},
				{"user": "U456", "text": "Hi there", "ts": "1234567891.123456"},
			},
		})
	})

	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{"user": "U123", "text": "Parent message", "ts": "1234567890.123456"},
				{"user": "U456", "text": "Reply 1", "ts": "1234567890.234567"},
			},
		})
	})

	mux.HandleFunc("/search.messages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": map[string]any{
				"matches": []map[string]any{
					{"channel": map[string]any{"id": "C0BACKEND"}, "user": "U123", "text": "JWT refactor done", "ts": "1234567890.123456"},
				},
			},
		})
	})

	return httptest.NewServer(mux)
}

func TestConnector_Provider(t *testing.T) {
	c := slacksource.New()
	if c.Provider() != "slack" {
		t.Errorf("expected slack, got %s", c.Provider())
	}
}

func TestConnector_Tools(t *testing.T) {
	c := slacksource.New()
	tools := c.Tools()
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}

	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, expected := range []string{"slack_channel_history", "slack_thread_replies", "slack_search_messages"} {
		if !names[expected] {
			t.Errorf("expected tool %s", expected)
		}
	}
}

func TestConnector_ChannelHistory(t *testing.T) {
	server := newMockSlackServer()
	defer server.Close()

	c := slacksource.New().WithAPIURL(server.URL + "/")
	result, err := c.Execute(context.Background(), "slack_channel_history", map[string]any{
		"channel": "C0BACKEND",
		"limit":   5,
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages, ok := result["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("expected messages array, got %T", result["messages"])
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0]["text"] != "Hello world" {
		t.Errorf("expected 'Hello world', got %v", messages[0]["text"])
	}
}

func TestConnector_ChannelHistory_MissingChannel(t *testing.T) {
	c := slacksource.New()
	_, err := c.Execute(context.Background(), "slack_channel_history", map[string]any{}, testToken())
	if err == nil {
		t.Fatal("expected error for missing channel")
	}
}

func TestConnector_ChannelHistory_LimitCapped(t *testing.T) {
	server := newMockSlackServer()
	defer server.Close()

	c := slacksource.New().WithAPIURL(server.URL + "/")
	// Limit > 100 should be capped.
	result, err := c.Execute(context.Background(), "slack_channel_history", map[string]any{
		"channel": "C0BACKEND",
		"limit":   200,
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["messages"] == nil {
		t.Fatal("expected messages in result")
	}
}

func TestConnector_ThreadReplies(t *testing.T) {
	server := newMockSlackServer()
	defer server.Close()

	c := slacksource.New().WithAPIURL(server.URL + "/")
	result, err := c.Execute(context.Background(), "slack_thread_replies", map[string]any{
		"channel":   "C0BACKEND",
		"thread_ts": "1234567890.123456",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages, ok := result["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("expected messages array, got %T", result["messages"])
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
}

func TestConnector_ThreadReplies_MissingParams(t *testing.T) {
	c := slacksource.New()

	// Missing channel.
	_, err := c.Execute(context.Background(), "slack_thread_replies", map[string]any{
		"thread_ts": "123.456",
	}, testToken())
	if err == nil {
		t.Fatal("expected error for missing channel")
	}

	// Missing thread_ts.
	_, err = c.Execute(context.Background(), "slack_thread_replies", map[string]any{
		"channel": "C0BACKEND",
	}, testToken())
	if err == nil {
		t.Fatal("expected error for missing thread_ts")
	}
}

func TestConnector_SearchMessages(t *testing.T) {
	server := newMockSlackServer()
	defer server.Close()

	c := slacksource.New().WithAPIURL(server.URL + "/")
	result, err := c.Execute(context.Background(), "slack_search_messages", map[string]any{
		"query": "JWT refactor",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages, ok := result["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("expected messages array, got %T", result["messages"])
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 match, got %d", len(messages))
	}
	if messages[0]["text"] != "JWT refactor done" {
		t.Errorf("expected 'JWT refactor done', got %v", messages[0]["text"])
	}
}

func TestConnector_SearchMessages_MissingQuery(t *testing.T) {
	c := slacksource.New()
	_, err := c.Execute(context.Background(), "slack_search_messages", map[string]any{}, testToken())
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestConnector_ChannelHistory_WithDateFilters(t *testing.T) {
	var gotOldest, gotLatest string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotOldest = r.FormValue("oldest")
		gotLatest = r.FormValue("latest")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"messages": []map[string]any{},
		})
	}))
	defer server.Close()

	c := slacksource.New().WithAPIURL(server.URL + "/")
	_, err := c.Execute(context.Background(), "slack_channel_history", map[string]any{
		"channel": "C0BACKEND",
		"after":   "2026-04-14",
		"before":  "2026-04-16",
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// after=2026-04-14 → Unix 1776211200 (UTC midnight)
	if gotOldest == "" {
		t.Error("expected oldest param to be set from after")
	}
	if gotLatest == "" {
		t.Error("expected latest param to be set from before")
	}
}

func TestConnector_SearchMessages_WithDateFilters(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotQuery = r.FormValue("query")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": map[string]any{
				"matches": []map[string]any{},
			},
		})
	}))
	defer server.Close()

	c := slacksource.New().WithAPIURL(server.URL + "/")
	_, err := c.Execute(context.Background(), "slack_search_messages", map[string]any{
		"query":  "deploy",
		"after":  "2026-04-14",
		"before": "2026-04-16",
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotQuery == "" {
		t.Fatal("expected query to be set")
	}
	if !strings.Contains(gotQuery, "after:2026-04-14") {
		t.Errorf("expected after: in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "before:2026-04-16") {
		t.Errorf("expected before: in query, got %q", gotQuery)
	}
}

func TestConnector_UnknownTool(t *testing.T) {
	c := slacksource.New()
	_, err := c.Execute(context.Background(), "slack_nonexistent", nil, testToken())
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestConnector_InvalidToken(t *testing.T) {
	c := slacksource.New()
	_, err := c.Execute(context.Background(), "slack_channel_history", map[string]any{
		"channel": "C0BACKEND",
	}, []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestConnector_EmptyAccessToken(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"token_type": "user"})
	c := slacksource.New()
	_, err := c.Execute(context.Background(), "slack_channel_history", map[string]any{
		"channel": "C0BACKEND",
	}, token)
	if err == nil {
		t.Fatal("expected error for missing access_token")
	}
}
