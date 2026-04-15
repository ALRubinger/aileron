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

func TestSlackWebhook_MessageEvent_RoutesToUser(t *testing.T) {
	srv := newSlackWebhookServer()
	ctx := context.Background()

	// Seed a connected Slack account.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_alice",
		Provider:       model.ConnectedAccountProviderSlack,
		ExternalUserID: "U123ABC",
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
		"event_id": "ev_001",
		"event": map[string]any{
			"type":    "message",
			"user":    "U123ABC",
			"text":    "Does the JWT refactor change the claims structure?",
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

	// Wait briefly for async processing.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if receivedUserID != "usr_alice" {
		t.Errorf("expected usr_alice, got %q", receivedUserID)
	}
	if receivedMsg.Body != "Does the JWT refactor change the claims structure?" {
		t.Errorf("expected message body, got %q", receivedMsg.Body)
	}
	if receivedMsg.Service != "slack" {
		t.Errorf("expected service=slack, got %q", receivedMsg.Service)
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

func TestSlackWebhook_EventDeduplication(t *testing.T) {
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
			"user":    "U123ABC",
			"text":    "Duplicate event",
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
