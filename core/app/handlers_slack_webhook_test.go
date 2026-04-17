package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
)

const testSigningSecret = "test-signing-secret-12345"

func newSlackWebhookServer() *apiServer {
	return &apiServer{
		log:                slog.Default(),
		connectedAccounts:  mem.NewConnectedAccountStore(),
		slackSigningSecret: testSigningSecret,
		slackDedup:         newSlackEventDedup(),
	}
}

func signSlackRequest(body []byte, secret string) (string, string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return ts, sig
}

func slackWebhookRequest(body []byte, timestamp, signature string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/webhooks/slack/events", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Slack-Request-Timestamp", timestamp)
	r.Header.Set("X-Slack-Signature", signature)
	return r
}

func TestSlackWebhook_URLVerification(t *testing.T) {
	srv := newSlackWebhookServer()
	body, _ := json.Marshal(map[string]string{
		"type":      "url_verification",
		"challenge": "test-challenge-xyz",
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["challenge"] != "test-challenge-xyz" {
		t.Errorf("expected challenge echo, got %v", resp)
	}
}

func TestSlackWebhook_InvalidSignature(t *testing.T) {
	srv := newSlackWebhookServer()
	body, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": "x"})
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, "v0=invalid-signature"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid signature, got %d", w.Code)
	}
}

func TestSlackWebhook_MissingSignature(t *testing.T) {
	srv := newSlackWebhookServer()
	body := []byte(`{"type":"url_verification","challenge":"x"}`)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/webhooks/slack/events", strings.NewReader(string(body)))
	// No signature headers.
	srv.handleSlackEvent(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing signature, got %d", w.Code)
	}
}

func TestSlackWebhook_StaleTimestamp(t *testing.T) {
	srv := newSlackWebhookServer()
	body := []byte(`{"type":"url_verification","challenge":"x"}`)

	staleTs := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", staleTs, string(body))
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, staleTs, sig))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for stale timestamp, got %d", w.Code)
	}
}

func TestSlackWebhook_MentionedUser_GetsDraft(t *testing.T) {
	srv := newSlackWebhookServer()
	ctx := context.Background()

	// Alice is the Aileron user; Bob sends the message mentioning Alice.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_alice",
		Provider:       model.ConnectedAccountProviderSlack,
		ExternalUserID: "U_ALICE",
		ExternalTeamID: "T001TEAM",
		Status:         model.ConnectedAccountStatusActive,
	})

	var mu sync.Mutex
	var receivedUserID string
	var receivedMsg comms.IncomingMessage
	srv.onSlackMessage = func(_ context.Context, userID string, msg comms.IncomingMessage) {
		mu.Lock()
		receivedUserID = userID
		receivedMsg = msg
		mu.Unlock()
	}

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_mention",
		"event": map[string]any{
			"type":    "message",
			"user":    "U_BOB",
			"text":    "Hey <@U_ALICE> can you review this PR?",
			"channel": "C0BACKEND",
			"ts":      "1234567890.123456",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if receivedUserID != "usr_alice" {
		t.Errorf("expected usr_alice, got %q", receivedUserID)
	}
	if receivedMsg.Body != "Hey <@U_ALICE> can you review this PR?" {
		t.Errorf("expected message body, got %q", receivedMsg.Body)
	}
	if receivedMsg.Service != "slack" {
		t.Errorf("expected service=slack, got %q", receivedMsg.Service)
	}
}

func TestSlackWebhook_OwnMessage_Skipped(t *testing.T) {
	srv := newSlackWebhookServer()
	ctx := context.Background()

	// Alice sends a message — should NOT trigger a draft for Alice.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_alice",
		Provider:       model.ConnectedAccountProviderSlack,
		ExternalUserID: "U_ALICE",
		ExternalTeamID: "T001TEAM",
		Status:         model.ConnectedAccountStatusActive,
	})

	called := false
	srv.onSlackMessage = func(_ context.Context, _ string, _ comms.IncomingMessage) {
		called = true
	}

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_own",
		"event": map[string]any{
			"type":    "message",
			"user":    "U_ALICE",
			"text":    "I pushed the fix",
			"channel": "C0BACKEND",
			"ts":      "123.456",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("expected own message to be skipped, but draft was triggered")
	}
}

func TestSlackWebhook_SelfMention_GetsDraft(t *testing.T) {
	// Regression test: @mentioning yourself should trigger a draft.
	// Previously, the author was always skipped even when self-mentioned.
	srv := newSlackWebhookServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_alice",
		Provider:       model.ConnectedAccountProviderSlack,
		ExternalUserID: "U_ALICE",
		ExternalTeamID: "T001TEAM",
		Status:         model.ConnectedAccountStatusActive,
	})

	var mu sync.Mutex
	called := false
	srv.onSlackMessage = func(_ context.Context, _ string, _ comms.IncomingMessage) {
		mu.Lock()
		called = true
		mu.Unlock()
	}

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_selfmention",
		"event": map[string]any{
			"type":    "message",
			"user":    "U_ALICE",
			"text":    "<@U_ALICE> remind me to review PR #247",
			"channel": "C0BACKEND",
			"ts":      "123.789",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Error("expected self-mention to trigger a draft")
	}
}

func TestSlackWebhook_NoMention_NoDraft(t *testing.T) {
	srv := newSlackWebhookServer()
	ctx := context.Background()

	// Alice is connected but not mentioned in the message.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_alice",
		Provider:       model.ConnectedAccountProviderSlack,
		ExternalUserID: "U_ALICE",
		ExternalTeamID: "T001TEAM",
		Status:         model.ConnectedAccountStatusActive,
	})

	called := false
	srv.onSlackMessage = func(_ context.Context, _ string, _ comms.IncomingMessage) {
		called = true
	}

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_nomention",
		"event": map[string]any{
			"type":    "message",
			"user":    "U_BOB",
			"text":    "General discussion, no mentions",
			"channel": "C0BACKEND",
			"ts":      "123.456",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("expected no draft when user is not mentioned")
	}
}

func TestIsSlackMention(t *testing.T) {
	tests := []struct {
		text   string
		userID string
		want   bool
	}{
		{"Hey <@U123> check this", "U123", true},
		{"<@U123> <@U456> both mentioned", "U456", true},
		{"No mentions here", "U123", false},
		{"Partial U123 not a mention", "U123", false},
		{"<@U999> wrong user", "U123", false},
		{"", "U123", false},
	}
	for _, tt := range tests {
		if got := isSlackMention(tt.text, tt.userID); got != tt.want {
			t.Errorf("isSlackMention(%q, %q) = %v, want %v", tt.text, tt.userID, got, tt.want)
		}
	}
}

func TestSlackWebhook_BotMessageSkipped(t *testing.T) {
	srv := newSlackWebhookServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_alice",
		Provider:       model.ConnectedAccountProviderSlack,
		ExternalUserID: "U123ABC",
		ExternalTeamID: "T001TEAM",
		Status:         model.ConnectedAccountStatusActive,
	})

	called := false
	srv.onSlackMessage = func(_ context.Context, _ string, _ comms.IncomingMessage) {
		called = true
	}

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_bot",
		"event": map[string]any{
			"type":    "message",
			"user":    "U123ABC",
			"text":    "Bot says hello",
			"channel": "C0BACKEND",
			"ts":      "1234567890.111",
			"bot_id":  "B001BOT",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("expected bot message to be skipped")
	}
}

func TestSlackWebhook_UnknownTeamReturns200(t *testing.T) {
	srv := newSlackWebhookServer()
	// No connected accounts seeded.

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T_UNKNOWN",
		"event_id": "ev_unknown",
		"event": map[string]any{
			"type":    "message",
			"user":    "U_NOBODY",
			"text":    "Hello?",
			"channel": "C123",
			"ts":      "123.456",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	// Slack requires 200 regardless.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestSlackWebhook_MethodNotAllowed(t *testing.T) {
	srv := newSlackWebhookServer()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/webhooks/slack/events", nil)
	srv.handleSlackEvent(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestSlackWebhook_InvalidJSON(t *testing.T) {
	srv := newSlackWebhookServer()
	body := []byte(`not json`)
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSlackWebhook_UnknownPayloadType(t *testing.T) {
	srv := newSlackWebhookServer()
	body, _ := json.Marshal(map[string]string{"type": "app_rate_limited"})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown type, got %d", w.Code)
	}
}

func TestSlackWebhook_NonMessageEvent(t *testing.T) {
	srv := newSlackWebhookServer()

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001",
		"event_id": "ev_reaction",
		"event": map[string]any{
			"type": "reaction_added",
			"user": "U123",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	called := false
	srv.onSlackMessage = func(_ context.Context, _ string, _ comms.IncomingMessage) {
		called = true
	}

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("expected non-message event to be ignored")
	}
}

func TestSlackWebhook_NoHandlerConfigured(t *testing.T) {
	srv := newSlackWebhookServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_alice",
		Provider:       model.ConnectedAccountProviderSlack,
		ExternalUserID: "U_ALICE",
		ExternalTeamID: "T001TEAM",
		Status:         model.ConnectedAccountStatusActive,
	})
	// onSlackMessage is nil — should not panic when a mentioned user is found.

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_nohandler",
		"event": map[string]any{
			"type":    "message",
			"user":    "U_BOB",
			"text":    "Hey <@U_ALICE> hello",
			"channel": "C123",
			"ts":      "123.456",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// No panic = pass.
	time.Sleep(50 * time.Millisecond)
}

func TestSlackWebhook_EmptySigningSecret(t *testing.T) {
	srv := newSlackWebhookServer()
	srv.slackSigningSecret = "" // empty — should reject all

	body := []byte(`{"type":"url_verification","challenge":"x"}`)
	ts, sig := signSlackRequest(body, "any-secret")

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with empty signing secret, got %d", w.Code)
	}
}

func TestSlackWebhook_MalformedInnerEvent(t *testing.T) {
	srv := newSlackWebhookServer()

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001",
		"event_id": "ev_malformed",
		"event":    "not-a-json-object", // string instead of object
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	// Should still return 200 (Slack requires it) and not panic.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestSlackDedup_Expiry(t *testing.T) {
	d := &slackEventDedup{
		seen: map[string]time.Time{
			"old_event": time.Now().Add(-10 * time.Minute), // expired
		},
		ttl: 5 * time.Minute,
	}

	// A new event should trigger cleanup of the old one.
	if d.isDuplicate("new_event") {
		t.Error("new event should not be duplicate")
	}

	// Old event should have been swept.
	d.mu.Lock()
	_, exists := d.seen["old_event"]
	d.mu.Unlock()
	if exists {
		t.Error("expected old_event to be swept")
	}
}

func TestSlackWebhook_EventDeduplication(t *testing.T) {
	srv := newSlackWebhookServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_alice",
		Provider:       model.ConnectedAccountProviderSlack,
		ExternalUserID: "U_ALICE",
		ExternalTeamID: "T001TEAM",
		Status:         model.ConnectedAccountStatusActive,
	})

	callCount := 0
	var mu sync.Mutex
	srv.onSlackMessage = func(_ context.Context, _ string, _ comms.IncomingMessage) {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_dedup_001", // same event ID both times
		"event": map[string]any{
			"type":    "message",
			"user":    "U_BOB",
			"text":    "<@U_ALICE> duplicate event",
			"channel": "C123",
			"ts":      "123.456",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	// Send the same event twice.
	w1 := httptest.NewRecorder()
	srv.handleSlackEvent(w1, slackWebhookRequest(body, ts, sig))

	// Resign with fresh timestamp (Slack might retry with a new timestamp).
	ts2, sig2 := signSlackRequest(body, testSigningSecret)
	w2 := httptest.NewRecorder()
	srv.handleSlackEvent(w2, slackWebhookRequest(body, ts2, sig2))

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("expected callback called once (dedup), got %d", callCount)
	}
}
