package comms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
)

func TestSlackThreadHasReplies_EmptyInputs(t *testing.T) {
	ctx := context.Background()
	if comms.SlackThreadHasReplies(ctx, "", "C123", "123.456") {
		t.Error("expected false for empty bot token")
	}
	if comms.SlackThreadHasReplies(ctx, "xoxb-test", "", "123.456") {
		t.Error("expected false for empty channel")
	}
	if comms.SlackThreadHasReplies(ctx, "xoxb-test", "C123", "") {
		t.Error("expected false for empty message TS")
	}
}

func TestSlackThreadHasReplies_WithReplies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{"ts": "123.456", "text": "parent message", "user": "U_AUTHOR"},
				{"ts": "123.789", "text": "a reply", "user": "U_REPLIER"},
			},
		})
	}))
	defer server.Close()

	comms.SetThreadAPIURL(server.URL + "/")
	defer comms.SetThreadAPIURL("")

	result := comms.SlackThreadHasReplies(context.Background(), "xoxb-test", "C123", "123.456")
	if !result {
		t.Error("expected true when thread has replies")
	}
}

func TestSlackThreadHasReplies_NoReplies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{"ts": "123.456", "text": "parent message only", "user": "U_AUTHOR"},
			},
		})
	}))
	defer server.Close()

	comms.SetThreadAPIURL(server.URL + "/")
	defer comms.SetThreadAPIURL("")

	result := comms.SlackThreadHasReplies(context.Background(), "xoxb-test", "C123", "123.456")
	if result {
		t.Error("expected false when thread has no replies")
	}
}

func TestSlackThreadHasReplies_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "channel_not_found",
		})
	}))
	defer server.Close()

	comms.SetThreadAPIURL(server.URL + "/")
	defer comms.SetThreadAPIURL("")

	result := comms.SlackThreadHasReplies(context.Background(), "xoxb-test", "C123", "123.456")
	if result {
		t.Error("expected false on API error")
	}
}
