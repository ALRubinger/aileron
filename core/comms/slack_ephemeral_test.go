package comms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
)

func TestPostEphemeralDraft_EmptyBotToken(t *testing.T) {
	err := comms.PostEphemeralDraft(context.Background(), comms.SlackDraftMessage{
		BotToken: "",
		Channel:  "C123",
		UserID:   "U123",
		DraftID:  "dft_1",
		Draft:    "test draft",
	})
	if err == nil {
		t.Fatal("expected error for empty bot token")
	}
}

func TestPostEphemeralDraft_EmptyChannel(t *testing.T) {
	err := comms.PostEphemeralDraft(context.Background(), comms.SlackDraftMessage{
		BotToken: "xoxb-test",
		Channel:  "",
		UserID:   "U123",
	})
	if err == nil {
		t.Fatal("expected error for empty channel")
	}
}

func TestPostEphemeralDraft_EmptyUserID(t *testing.T) {
	err := comms.PostEphemeralDraft(context.Background(), comms.SlackDraftMessage{
		BotToken: "xoxb-test",
		Channel:  "C123",
		UserID:   "",
	})
	if err == nil {
		t.Fatal("expected error for empty user ID")
	}
}

func TestPostEphemeralText_EmptyBotToken(t *testing.T) {
	err := comms.PostEphemeralText(context.Background(), "", "C123", "U123", "hello", "")
	if err == nil {
		t.Fatal("expected error for empty bot token")
	}
}

func TestPostEphemeralText_EmptyChannel(t *testing.T) {
	err := comms.PostEphemeralText(context.Background(), "xoxb-test", "", "U123", "hello", "")
	if err == nil {
		t.Fatal("expected error for empty channel")
	}
}

func TestPostEphemeralText_EmptyUserID(t *testing.T) {
	err := comms.PostEphemeralText(context.Background(), "xoxb-test", "C123", "", "hello", "")
	if err == nil {
		t.Fatal("expected error for empty user ID")
	}
}

func TestPostEphemeralText_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":         true,
			"message_ts": "1234567890.123456",
		})
	}))
	defer server.Close()

	comms.SetEphemeralAPIURL(server.URL + "/")
	defer comms.SetEphemeralAPIURL("")

	err := comms.PostEphemeralText(context.Background(), "xoxb-test", "C123", "U456", "Drafting a reply...", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPostEphemeralText_WithThreadTS(t *testing.T) {
	var capturedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		capturedBody = r.FormValue("thread_ts")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":         true,
			"message_ts": "1234567890.123456",
		})
	}))
	defer server.Close()

	comms.SetEphemeralAPIURL(server.URL + "/")
	defer comms.SetEphemeralAPIURL("")

	err := comms.PostEphemeralText(context.Background(), "xoxb-test", "C123", "U456", "Drafting...", "999.888")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedBody != "999.888" {
		t.Errorf("expected thread_ts=999.888, got %q", capturedBody)
	}
}

func TestPostEphemeralText_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "channel_not_found",
		})
	}))
	defer server.Close()

	comms.SetEphemeralAPIURL(server.URL + "/")
	defer comms.SetEphemeralAPIURL("")

	err := comms.PostEphemeralText(context.Background(), "xoxb-test", "C123", "U456", "hello", "")
	if err == nil {
		t.Fatal("expected error on API failure")
	}
}

func TestBuildDraftBlocks_Basic(t *testing.T) {
	blocks := comms.BuildDraftBlocks(comms.SlackDraftMessage{
		Author:  "Sarah",
		DraftID: "dft_123",
		Draft:   "No, the claims stay the same.",
	})

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (section + actions), got %d", len(blocks))
	}
}

func TestBuildDraftBlocks_LongDraftTruncated(t *testing.T) {
	longDraft := strings.Repeat("x", 600)
	blocks := comms.BuildDraftBlocks(comms.SlackDraftMessage{
		Author:  "Sarah",
		DraftID: "dft_123",
		Draft:   longDraft,
	})

	// Should still produce valid blocks without error.
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
}

func TestBuildDraftBlocks_EmptyDraft(t *testing.T) {
	blocks := comms.BuildDraftBlocks(comms.SlackDraftMessage{
		Author:  "Sarah",
		DraftID: "dft_123",
		Draft:   "",
	})

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks even with empty draft, got %d", len(blocks))
	}
}
