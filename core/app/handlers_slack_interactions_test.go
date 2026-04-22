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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func newInteractionTestServer() *apiServer {
	srv := &apiServer{
		log:                slog.Default(),
		drafts:             mem.NewDraftStore(),
		feedback:           mem.NewDraftFeedbackStore(),
		connectedAccounts:  mem.NewConnectedAccountStore(),
		vault:              vault.NewMemVault(),
		slackSigningSecret: testSigningSecret,
		users:              &stubUserStore{},
		newID:              func() string { return "test-id" },
	}
	srv.slackSender = func(_ context.Context, _, _, _, _ string) error { return nil }
	return srv
}

func signedInteractionRequest(payloadJSON string) (*http.Request, string) {
	formData := url.Values{"payload": {payloadJSON}}.Encode()
	body := []byte(formData)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/interactions",
		strings.NewReader(formData))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", sig)
	return r, ts
}

func newMockResponseURLServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestInteraction_ApproveDraft_Deprecated(t *testing.T) {
	srv := newInteractionTestServer()

	responseServer := newMockResponseURLServer()
	defer responseServer.Close()

	// Capture what gets posted to the response URL.
	var mu sync.Mutex
	var capturedBody string
	responseServer.Close()
	responseServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(nil)
		_ = body
		raw := make([]byte, r.ContentLength)
		r.Body.Read(raw)
		mu.Lock()
		capturedBody = string(raw)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": "U123"},
		"actions": []map[string]any{
			{"action_id": "approve_draft", "value": "dft_1"},
		},
		"response_url": responseServer.URL,
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(capturedBody, "replaced") {
		t.Errorf("expected deprecation message containing 'replaced', got %q", capturedBody)
	}
}

func TestInteraction_DiscardDraft_Deprecated(t *testing.T) {
	srv := newInteractionTestServer()

	var mu sync.Mutex
	var capturedBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		r.Body.Read(raw)
		mu.Lock()
		capturedBody = string(raw)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	payload, _ := json.Marshal(map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": "U123"},
		"actions":      []map[string]any{{"action_id": "discard_draft", "value": "dft_1"}},
		"response_url": responseServer.URL,
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(capturedBody, "replaced") {
		t.Errorf("expected deprecation message containing 'replaced', got %q", capturedBody)
	}
}

func TestInteraction_InvalidSignature(t *testing.T) {
	srv := newInteractionTestServer()

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/interactions",
		strings.NewReader("payload={}"))
	r.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set("X-Slack-Signature", "v0=invalid")

	w := httptest.NewRecorder()
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestInteraction_MethodNotAllowed(t *testing.T) {
	srv := newInteractionTestServer()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/webhooks/slack/interactions", nil)
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestInteraction_MissingPayload(t *testing.T) {
	srv := newInteractionTestServer()
	body := []byte("other=data")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/interactions",
		strings.NewReader(string(body)))
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", sig)

	w := httptest.NewRecorder()
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestInteraction_NonBlockActions(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-block_actions, got %d", w.Code)
	}
}

func TestInteraction_DraftNotFound(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U123"},
		"actions": []map[string]any{{"action_id": "approve_draft", "value": "nonexistent"}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	// Should still return 200 (Slack requires it).
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestInteraction_RespondToInteraction_EmptyURL(t *testing.T) {
	srv := newInteractionTestServer()
	// Should not panic with empty response URL.
	srv.respondToInteraction("", "test message")
}

func TestInteraction_MessageAction_Parsed(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type":        "message_action",
		"callback_id": "draft_reply",
		"trigger_id":  "trig_12345",
		"user":        map[string]any{"id": "U_ALICE"},
		"team":        map[string]any{"id": "T001TEAM"},
		"channel":     map[string]any{"id": "C0BACKEND", "name": "backend"},
		"message": map[string]any{
			"text":      "Can someone review PR #42?",
			"user":      "U_BOB",
			"ts":        "111.222",
			"thread_ts": "111.000",
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Currently a stub — no panic, correct routing = pass.
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_MessageAction_NoActions(t *testing.T) {
	// message_action payloads have no actions array — ensure we don't crash.
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type":        "message_action",
		"callback_id": "draft_reply",
		"trigger_id":  "trig_999",
		"user":        map[string]any{"id": "U_ALICE"},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_EditDraft_Deprecated(t *testing.T) {
	srv := newInteractionTestServer()

	var mu sync.Mutex
	var capturedBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		r.Body.Read(raw)
		mu.Lock()
		capturedBody = string(raw)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	payload, _ := json.Marshal(map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": "U123"},
		"actions":      []map[string]any{{"action_id": "edit_draft", "value": "dft_1"}},
		"response_url": responseServer.URL,
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(capturedBody, "replaced") {
		t.Errorf("expected deprecation message containing 'replaced', got %q", capturedBody)
	}
}

func TestInteraction_ViewSubmission_SendsDraft(t *testing.T) {
	srv := newInteractionTestServer()
	srv.systemVault = vault.NewMemVault()
	enableVaultEncryption(srv, "usr_a")
	ctx := context.Background()

	// Seed connected account + encrypted user token.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})
	storeEncryptedToken(srv, "connected-accounts/usr_a/slack",
		[]byte(`{"access_token":"xoxp-test"}`))

	var mu sync.Mutex
	var sentBody string
	srv.slackSender = func(_ context.Context, _, _, body, _ string) error {
		mu.Lock()
		sentBody = body
		mu.Unlock()
		return nil
	}

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel:  "C0BACKEND",
		TargetThreadTS: "111.222",
		UserID:         "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{
							"value": "This is my edited draft",
						},
					},
				},
			},
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	got := sentBody
	mu.Unlock()
	if got != "This is my edited draft" {
		t.Errorf("expected draft text to be sent, got %q", got)
	}
}

func TestInteraction_ViewSubmission_WrongCallback(t *testing.T) {
	srv := newInteractionTestServer()

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"view": map[string]any{
			"id":          "V_123",
			"callback_id": "some_other_modal",
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Should be ignored — no crash.
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_ViewSubmission_EmptyDraft(t *testing.T) {
	srv := newInteractionTestServer()

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel: "C0BACKEND",
		UserID:        "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			"state": map[string]any{
				"values": map[string]any{
					draftInputBlockID: map[string]any{
						draftInputActionID: map[string]any{
							"value": "", // empty draft
						},
					},
				},
			},
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	// No crash, empty draft is logged and ignored.
}

func TestInteraction_ViewSubmission_NoState(t *testing.T) {
	srv := newInteractionTestServer()

	meta, _ := json.Marshal(DraftModalMeta{
		TargetChannel: "C0BACKEND",
		UserID:        "U_ALICE",
	})

	payload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U_ALICE"},
		"team": map[string]any{"id": "T001"},
		"view": map[string]any{
			"id":               "V_123",
			"callback_id":      draftModalCallbackID,
			"private_metadata": string(meta),
			// no state field
		},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestInteraction_SendDraftAgent(t *testing.T) {
	srv := newInteractionTestServer()
	enableVaultEncryption(srv, "usr_a")
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})
	storeEncryptedToken(srv, "connected-accounts/usr_a/slack",
		[]byte(`{"access_token":"xoxp-test"}`))

	var mu sync.Mutex
	var sentBody, sentChannel string
	srv.slackSender = func(_ context.Context, _, channel, body, _ string) error {
		mu.Lock()
		sentBody = body
		sentChannel = channel
		mu.Unlock()
		return nil
	}

	sendMeta, _ := json.Marshal(map[string]any{
		"channel":   "C_TARGET",
		"thread_ts": "111.222",
		"body":      "Draft from agent DM",
		"user_id":   "usr_a",
	})

	// Create a pending draft so processInteraction doesn't bail on draft lookup.
	srv.drafts.Create(ctx, model.Draft{
		ID: string(sendMeta), UserID: "usr_a", Status: model.DraftStatusPending,
	})

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{"action_id": "send_draft_agent", "value": string(sendMeta)}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if sentBody != "Draft from agent DM" {
		t.Errorf("expected 'Draft from agent DM', got %q", sentBody)
	}
	if sentChannel != "C_TARGET" {
		t.Errorf("expected C_TARGET, got %q", sentChannel)
	}
}

func TestInteraction_SendDraftAgent_BadJSON(t *testing.T) {
	srv := newInteractionTestServer()

	// Create a draft with ID matching the bad JSON so processInteraction finds it.
	srv.drafts.Create(context.Background(), model.Draft{
		ID: "not-valid-json", UserID: "usr_a", Status: model.DraftStatusPending,
	})

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{"action_id": "send_draft_agent", "value": "not-valid-json"}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	// No crash — bad JSON logged and returned.
}

func TestInteraction_UnknownAction(t *testing.T) {
	srv := newInteractionTestServer()

	srv.drafts.Create(context.Background(), model.Draft{
		ID: "dft_1", UserID: "usr_a", Status: model.DraftStatusPending,
	})

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U_ALICE"},
		"actions": []map[string]any{{"action_id": "some_unknown_action", "value": "dft_1"}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
}
