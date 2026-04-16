package gmail_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gmailsource "github.com/ALRubinger/aileron/core/source/gmail"
	"google.golang.org/api/option"
)

func testToken() []byte {
	b, _ := json.Marshal(map[string]string{
		"access_token": "test-token",
		"token_type":   "bearer",
	})
	return b
}

func TestConnector_Provider(t *testing.T) {
	c := gmailsource.New()
	if c.Provider() != "gmail" {
		t.Errorf("expected gmail, got %s", c.Provider())
	}
}

func TestConnector_Tools(t *testing.T) {
	c := gmailsource.New()
	tools := c.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if !names["gmail_search"] {
		t.Error("expected gmail_search tool")
	}
	if !names["gmail_get_thread"] {
		t.Error("expected gmail_get_thread tool")
	}
}

func TestConnector_UnknownTool(t *testing.T) {
	c := gmailsource.New()
	token, _ := json.Marshal(map[string]string{"access_token": "test"})
	_, err := c.Execute(context.Background(), "gmail_nonexistent", nil, token)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestConnector_InvalidToken(t *testing.T) {
	c := gmailsource.New()
	_, err := c.Execute(context.Background(), "gmail_search", map[string]any{"query": "test"}, []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestConnector_EmptyAccessToken(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"token_type": "bearer"})
	c := gmailsource.New()
	_, err := c.Execute(context.Background(), "gmail_search", map[string]any{"query": "test"}, token)
	if err == nil {
		t.Fatal("expected error for missing access_token")
	}
}

func TestConnector_Search_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/gmail/v1/users/me/messages" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{
				"messages":            []map[string]any{{"id": "msg_1", "threadId": "thread_1"}},
				"resultSizeEstimate": 1,
			})
		case r.URL.Path == "/gmail/v1/users/me/messages/msg_1":
			json.NewEncoder(w).Encode(map[string]any{
				"id":      "msg_1",
				"snippet": "Here is the migration proposal",
				"payload": map[string]any{
					"headers": []map[string]any{
						{"name": "Subject", "value": "Migration Proposal"},
						{"name": "From", "value": "sarah@example.com"},
						{"name": "Date", "value": "Mon, 14 Apr 2026 10:00:00 -0700"},
					},
				},
			})
		}
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	result, err := c.Execute(context.Background(), "gmail_search", map[string]any{
		"query": "migration proposal",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages := result["messages"].([]map[string]any)
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0]["subject"] != "Migration Proposal" {
		t.Errorf("expected Migration Proposal, got %v", messages[0]["subject"])
	}
}

func TestConnector_GetThread_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "thread_1",
			"messages": []map[string]any{
				{
					"id":      "msg_1",
					"snippet": "Original message",
					"payload": map[string]any{
						"headers": []map[string]any{
							{"name": "Subject", "value": "Re: Proposal"},
							{"name": "From", "value": "sarah@example.com"},
						},
					},
				},
				{
					"id":      "msg_2",
					"snippet": "Reply message",
					"payload": map[string]any{
						"headers": []map[string]any{
							{"name": "Subject", "value": "Re: Proposal"},
							{"name": "From", "value": "you@example.com"},
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	result, err := c.Execute(context.Background(), "gmail_get_thread", map[string]any{
		"thread_id": "thread_1",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages := result["messages"].([]map[string]any)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
}

func TestConnector_Search_WithMaxResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages":            []map[string]any{},
			"resultSizeEstimate": 0,
		})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	_, err := c.Execute(context.Background(), "gmail_search", map[string]any{
		"query":       "test",
		"max_results": float64(5),
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnector_OAuthTokenFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages":            []map[string]any{},
			"resultSizeEstimate": 0,
		})
	}))
	defer server.Close()

	token, _ := json.Marshal(map[string]any{
		"access_token": "oauth2-format-token",
		"token_type":   "bearer",
		"expiry":       "2026-12-31T00:00:00Z",
	})
	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	_, err := c.Execute(context.Background(), "gmail_search", map[string]any{"query": "test"}, token)
	if err != nil {
		t.Fatalf("unexpected error with oauth2 token format: %v", err)
	}
}

func TestConnector_Search_MissingQuery(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"access_token": "test"})
	c := gmailsource.New()
	_, err := c.Execute(context.Background(), "gmail_search", map[string]any{}, token)
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestConnector_GetThread_MissingThreadID(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"access_token": "test"})
	c := gmailsource.New()
	_, err := c.Execute(context.Background(), "gmail_get_thread", map[string]any{}, token)
	if err == nil {
		t.Fatal("expected error for missing thread_id")
	}
}
