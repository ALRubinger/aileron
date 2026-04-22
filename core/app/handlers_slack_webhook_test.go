package app

import (
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
	"testing"
	"time"

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

func TestSlackWebhook_AssistantThreadStarted(t *testing.T) {
	srv := newSlackWebhookServer()

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_thread_start",
		"event": map[string]any{
			"type": "assistant_thread_started",
			"assistant_thread": map[string]any{
				"channel_id": "D_DM_CHAN",
				"thread_ts":  "999.001",
				"context": map[string]any{
					"channel_id": "C_ORIGINAL",
					"team_id":    "T001TEAM",
				},
			},
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Currently a stub — no panic = pass. Will route to handler in PR 2.
	time.Sleep(50 * time.Millisecond)
}

func TestSlackWebhook_AssistantThreadContextChanged(t *testing.T) {
	srv := newSlackWebhookServer()

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_ctx_change",
		"event": map[string]any{
			"type": "assistant_thread_context_changed",
			"assistant_thread": map[string]any{
				"channel_id": "D_DM_CHAN",
				"thread_ts":  "999.001",
				"context": map[string]any{
					"channel_id": "C_NEW_CHANNEL",
					"team_id":    "T001TEAM",
				},
			},
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

