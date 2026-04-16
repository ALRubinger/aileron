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
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
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
	srv.slackSender = func(_ context.Context, _, _, _ string) error { return nil }
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

func TestInteraction_ApproveDraft(t *testing.T) {
	srv := newInteractionTestServer()
	ctx := context.Background()

	responseServer := newMockResponseURLServer()
	defer responseServer.Close()

	// Seed a pending draft.
	srv.drafts.Create(ctx, model.Draft{
		ID:          "dft_1",
		UserID:      "usr_a",
		Status:      model.DraftStatusPending,
		Service:     "slack",
		Channel:     "C0BACKEND",
		Author:      "Sarah",
		DraftBody:   "No, the claims stay the same.",
	})

	// Seed connected account + vault token for sending.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive,
	})
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack",
		[]byte(`{"access_token":"xoxp-test","bot_access_token":"xoxb-test"}`), vault.Metadata{})

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

	// Wait for async processing.
	time.Sleep(50 * time.Millisecond)

	draft, _ := srv.drafts.Get(ctx, "dft_1")
	if draft.Status != model.DraftStatusApproved {
		t.Errorf("expected approved, got %s", draft.Status)
	}

	fb, _ := srv.feedback.List(ctx, store.DraftFeedbackFilter{UserID: "usr_a"})
	if len(fb) != 1 || fb[0].Signal != model.FeedbackSignalApproved {
		t.Error("expected approved feedback signal")
	}
}

func TestInteraction_DiscardDraft(t *testing.T) {
	srv := newInteractionTestServer()
	ctx := context.Background()

	responseServer := newMockResponseURLServer()
	defer responseServer.Close()

	srv.drafts.Create(ctx, model.Draft{
		ID: "dft_1", UserID: "usr_a", Status: model.DraftStatusPending,
		DraftBody: "draft text",
	})

	payload, _ := json.Marshal(map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": "U123"},
		"actions":      []map[string]any{{"action_id": "discard_draft", "value": "dft_1"}},
		"response_url": responseServer.URL,
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	time.Sleep(50 * time.Millisecond)

	draft, _ := srv.drafts.Get(ctx, "dft_1")
	if draft.Status != model.DraftStatusDiscarded {
		t.Errorf("expected discarded, got %s", draft.Status)
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

func TestInteraction_AlreadyActioned(t *testing.T) {
	srv := newInteractionTestServer()
	ctx := context.Background()

	srv.drafts.Create(ctx, model.Draft{
		ID: "dft_1", UserID: "usr_a", Status: model.DraftStatusApproved,
	})

	payload, _ := json.Marshal(map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U123"},
		"actions": []map[string]any{{"action_id": "approve_draft", "value": "dft_1"}},
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	time.Sleep(50 * time.Millisecond)
	// Draft status should remain approved, not re-processed.
	draft, _ := srv.drafts.Get(ctx, "dft_1")
	if draft.Status != model.DraftStatusApproved {
		t.Errorf("expected status unchanged, got %s", draft.Status)
	}
}

func TestInteraction_RespondToInteraction_EmptyURL(t *testing.T) {
	srv := newInteractionTestServer()
	// Should not panic with empty response URL.
	srv.respondToInteraction("", "test message")
}

func TestInteraction_EditDraft(t *testing.T) {
	srv := newInteractionTestServer()
	ctx := context.Background()

	responseServer := newMockResponseURLServer()
	defer responseServer.Close()

	srv.drafts.Create(ctx, model.Draft{
		ID: "dft_1", UserID: "usr_a", Status: model.DraftStatusPending,
		DraftBody: "Draft text for editing",
	})

	payload, _ := json.Marshal(map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": "U123"},
		"actions":      []map[string]any{{"action_id": "edit_draft", "value": "dft_1"}},
		"response_url": responseServer.URL,
	})

	w := httptest.NewRecorder()
	r, _ := signedInteractionRequest(string(payload))
	srv.handleSlackInteraction(w, r)

	// Edit just shows the text — draft stays pending for now.
	time.Sleep(50 * time.Millisecond)
	draft, _ := srv.drafts.Get(ctx, "dft_1")
	if draft.Status != model.DraftStatusPending {
		t.Errorf("expected pending (edit shows text), got %s", draft.Status)
	}
}
