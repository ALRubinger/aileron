package gmail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(tools))
	}
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, expected := range []string{"gmail_search", "gmail_get_thread", "drive_search", "drive_get_doc"} {
		if !names[expected] {
			t.Errorf("expected %s tool", expected)
		}
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

func TestConnector_Search_WithDateFilters(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/messages/") {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "snippet": "Test",
				"payload": map[string]any{
					"headers": []map[string]any{
						{"name": "Subject", "value": "Test"},
						{"name": "From", "value": "test@example.com"},
						{"name": "Date", "value": "Wed, 15 Apr 2026"},
					},
				},
			})
			return
		}
		gotQuery = r.URL.Query().Get("q")
		json.NewEncoder(w).Encode(map[string]any{
			"messages":           []map[string]any{{"id": "msg_1", "threadId": "t_1"}},
			"resultSizeEstimate": 1,
		})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	_, err := c.Execute(context.Background(), "gmail_search", map[string]any{
		"query":  "migration",
		"after":  "2026-04-14",
		"before": "2026-04-16",
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(gotQuery, "after:2026-04-14") {
		t.Errorf("expected after: in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "before:2026-04-16") {
		t.Errorf("expected before: in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "migration") {
		t.Errorf("expected original query preserved, got %q", gotQuery)
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

func TestConnector_DriveSearch_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files": []map[string]any{
				{
					"id":           "doc_123",
					"name":         "Migration Plan",
					"mimeType":     "application/vnd.google-apps.document",
					"modifiedTime": "2026-04-10T10:00:00Z",
					"webViewLink":  "https://docs.google.com/document/d/doc_123",
				},
			},
		})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	result, err := c.Execute(context.Background(), "drive_search", map[string]any{
		"query": "migration plan",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	files := result["files"].([]map[string]any)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0]["name"] != "Migration Plan" {
		t.Errorf("expected Migration Plan, got %v", files[0]["name"])
	}
}

func TestConnector_DriveSearch_MissingQuery(t *testing.T) {
	c := gmailsource.New()
	_, err := c.Execute(context.Background(), "drive_search", map[string]any{}, testToken())
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestConnector_DriveGetDoc_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Drive export endpoint returns plain text directly.
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("This is the migration plan document content.\n\nPhase 1: Schema changes\nPhase 2: Data migration"))
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	result, err := c.Execute(context.Background(), "drive_get_doc", map[string]any{
		"file_id": "doc_123",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content, ok := result["content"].(string)
	if !ok || content == "" {
		t.Fatal("expected non-empty content")
	}
	if !containsStr(content, "migration plan") {
		t.Errorf("expected content to contain 'migration plan', got: %s", content)
	}
}

func TestConnector_DriveGetDoc_InvalidToken(t *testing.T) {
	c := gmailsource.New()
	_, err := c.Execute(context.Background(), "drive_get_doc", map[string]any{
		"file_id": "doc_123",
	}, []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestConnector_DriveSearch_InvalidToken(t *testing.T) {
	c := gmailsource.New()
	_, err := c.Execute(context.Background(), "drive_search", map[string]any{
		"query": "test",
	}, []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestConnector_DriveGetDoc_MissingFileID(t *testing.T) {
	c := gmailsource.New()
	_, err := c.Execute(context.Background(), "drive_get_doc", map[string]any{}, testToken())
	if err == nil {
		t.Fatal("expected error for missing file_id")
	}
}

func TestConnector_ToolsIncludeDrive(t *testing.T) {
	c := gmailsource.New()
	tools := c.Tools()
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools (2 gmail + 2 drive), got %d", len(tools))
	}
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if !names["drive_search"] {
		t.Error("expected drive_search tool")
	}
	if !names["drive_get_doc"] {
		t.Error("expected drive_get_doc tool")
	}
}

func TestConnector_DriveSearch_WithMaxResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{}})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	_, err := c.Execute(context.Background(), "drive_search", map[string]any{
		"query":       "test",
		"max_results": float64(5),
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnector_DriveGetDoc_LargeContent(t *testing.T) {
	// Generate content > 20KB to test truncation.
	largeContent := make([]byte, 25000)
	for i := range largeContent {
		largeContent[i] = 'x'
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(largeContent)
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	result, err := c.Execute(context.Background(), "drive_get_doc", map[string]any{
		"file_id": "doc_big",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := result["content"].(string)
	if !containsStr(content, "truncated") {
		t.Error("expected truncation marker in large document")
	}
}

func TestConnector_Search_IntMaxResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages":           []map[string]any{},
			"resultSizeEstimate": 0,
		})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	// Test with int type (not float64).
	_, err := c.Execute(context.Background(), "gmail_search", map[string]any{
		"query":       "test",
		"max_results": 3,
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnector_DriveSearch_IntMaxResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{}})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	_, err := c.Execute(context.Background(), "drive_search", map[string]any{
		"query":       "test",
		"max_results": 3,
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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

func TestConnector_Search_ConcurrentFetch(t *testing.T) {
	const numMessages = 5
	var peakConcurrent atomic.Int32
	var currentConcurrent atomic.Int32

	// Gate ensures all in-flight requests arrive before any responds,
	// so we can measure true peak concurrency.
	var gate sync.WaitGroup
	gate.Add(numMessages)

	// Build the list response with multiple messages.
	listMessages := make([]map[string]any, numMessages)
	for i := range numMessages {
		listMessages[i] = map[string]any{
			"id":       fmt.Sprintf("msg_%d", i),
			"threadId": fmt.Sprintf("thread_%d", i),
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// List endpoint.
		if r.URL.Path == "/gmail/v1/users/me/messages" && !strings.Contains(r.URL.Path, "msg_") {
			json.NewEncoder(w).Encode(map[string]any{
				"messages":           listMessages,
				"resultSizeEstimate": numMessages,
			})
			return
		}

		// Get endpoint — track concurrency and wait at the gate.
		cur := currentConcurrent.Add(1)
		for {
			peak := peakConcurrent.Load()
			if cur <= peak || peakConcurrent.CompareAndSwap(peak, cur) {
				break
			}
		}
		gate.Done()
		gate.Wait() // wait for all requests to arrive
		defer currentConcurrent.Add(-1)

		// Small sleep so peak measurement stays stable.
		time.Sleep(time.Millisecond)

		msgID := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		json.NewEncoder(w).Encode(map[string]any{
			"id":      msgID,
			"snippet": "Snippet for " + msgID,
			"payload": map[string]any{
				"headers": []map[string]any{
					{"name": "Subject", "value": "Subject " + msgID},
					{"name": "From", "value": "sender@example.com"},
					{"name": "Date", "value": "Mon, 14 Apr 2026 10:00:00 -0700"},
				},
			},
		})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	result, err := c.Execute(context.Background(), "gmail_search", map[string]any{
		"query": "test",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	messages := result["messages"].([]map[string]any)
	if len(messages) != numMessages {
		t.Fatalf("expected %d messages, got %d", numMessages, len(messages))
	}

	// All messages should have correct metadata.
	for _, msg := range messages {
		if msg["subject"] == nil || msg["from"] == nil || msg["date"] == nil {
			t.Errorf("message missing metadata: %v", msg)
		}
	}

	// Verify that at least 2 requests ran concurrently (proves parallelism).
	if peak := peakConcurrent.Load(); peak < 2 {
		t.Errorf("expected concurrent fetches (peak >= 2), got peak=%d", peak)
	}
}

func TestConnector_Search_ConcurrentFetch_ErrorPropagation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/gmail/v1/users/me/messages" && !strings.Contains(r.URL.Path, "msg_") {
			json.NewEncoder(w).Encode(map[string]any{
				"messages": []map[string]any{
					{"id": "msg_ok", "threadId": "t1"},
					{"id": "msg_fail", "threadId": "t2"},
				},
				"resultSizeEstimate": 2,
			})
			return
		}

		// Fail on msg_fail.
		if strings.HasSuffix(r.URL.Path, "/msg_fail") {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_ok", "snippet": "ok",
			"payload": map[string]any{
				"headers": []map[string]any{
					{"name": "Subject", "value": "OK"},
				},
			},
		})
	}))
	defer server.Close()

	c := gmailsource.New().WithClientOption(option.WithEndpoint(server.URL))
	_, err := c.Execute(context.Background(), "gmail_search", map[string]any{
		"query": "test",
	}, testToken())

	if err == nil {
		t.Fatal("expected error when a message fetch fails")
	}
	if !strings.Contains(err.Error(), "fetching message metadata") {
		t.Errorf("expected 'fetching message metadata' in error, got: %v", err)
	}
}
