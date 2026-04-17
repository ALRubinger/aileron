package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func newDraftsTestServer() *apiServer {
	return &apiServer{
		log:               slog.Default(),
		drafts:            mem.NewDraftStore(),
		feedback:          mem.NewDraftFeedbackStore(),
		connectedAccounts: mem.NewConnectedAccountStore(),
		vault:             vault.NewMemVault(),
		users:             &stubUserStore{},
		newID:             func() string { return "test-id" },
	}
}

func seedDraft(ctx context.Context, s store.DraftStore, id, userID string, status model.DraftStatus) {
	s.Create(ctx, model.Draft{
		ID:          id,
		UserID:      userID,
		Status:      status,
		Service:     "slack",
		Channel:     "C0BACKEND",
		Author:      "Sarah",
		MessageBody: "Does the JWT refactor change the claims?",
		MessageTS:   "1234567890.123456",
		DraftBody:   "No, the claims stay the same.",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	})
}

func TestListDrafts_Pending(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()

	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)
	seedDraft(ctx, srv.drafts, "dft_2", "usr_a", model.DraftStatusApproved)
	seedDraft(ctx, srv.drafts, "dft_3", "usr_b", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/drafts?status=pending", "", userAClaims)
	srv.handleListDrafts(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []model.Draft `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 pending draft for usr_a, got %d", len(resp.Items))
	}
	if resp.Items[0].ID != "dft_1" {
		t.Errorf("expected dft_1, got %s", resp.Items[0].ID)
	}
}

func TestListDrafts_AllForUser(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()

	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)
	seedDraft(ctx, srv.drafts, "dft_2", "usr_a", model.DraftStatusApproved)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/drafts", "", userAClaims)
	srv.handleListDrafts(w, r)

	var resp struct {
		Items []model.Draft `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 drafts for usr_a, got %d", len(resp.Items))
	}
}

func TestListDrafts_Unauthorized(t *testing.T) {
	srv := newDraftsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/drafts", "", nil)
	srv.handleListDrafts(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestGetDraft_Success(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/drafts/dft_1", "", userAClaims)
	srv.handleGetDraft(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var d model.Draft
	json.NewDecoder(w.Body).Decode(&d)
	if d.ID != "dft_1" {
		t.Errorf("expected dft_1, got %s", d.ID)
	}
}

func TestGetDraft_OwnershipCheck(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/drafts/dft_1", "", userBClaims)
	srv.handleGetDraft(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for ownership violation, got %d", w.Code)
	}
}

func TestGetDraft_NotFound(t *testing.T) {
	srv := newDraftsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/drafts/nonexistent", "", userAClaims)
	srv.handleGetDraft(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDiscardDraft_Success(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/discard", "", userAClaims)
	srv.handleDiscardDraft(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var d model.Draft
	json.NewDecoder(w.Body).Decode(&d)
	if d.Status != model.DraftStatusDiscarded {
		t.Errorf("expected discarded, got %s", d.Status)
	}
}

func TestDiscardDraft_AlreadyActioned(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusApproved)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/discard", "", userAClaims)
	srv.handleDiscardDraft(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditDraft_MissingBody(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/edit", `{"body":""}`, userAClaims)
	srv.handleEditDraft(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDraftAction_Routing(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/discard", "", userAClaims)
	srv.handleDraftAction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 via action router, got %d: %s", w.Code, w.Body.String())
	}
}

func newDraftsTestServerWithSend() *apiServer {
	srv := newDraftsTestServer()
	// Set up a connected Slack account + vault token so send works.
	ctx := context.Background()
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_a",
		Provider:       model.ConnectedAccountProviderSlack,
		Status:         model.ConnectedAccountStatusActive,
		ExternalUserID: "U123ABC",
	})
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack", []byte(`{"access_token":"xoxp-test","bot_access_token":"xoxb-test"}`), vault.Metadata{})
	// Mock sender so we don't call real Slack.
	srv.slackSender = func(_ context.Context, token, channel, body, threadTS string) error {
		return nil
	}
	// Mock ephemeral poster.
	srv.ephemeralPoster = func(_ context.Context, msg comms.SlackDraftMessage) error {
		return nil
	}
	return srv
}

func TestApproveDraft_Success(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims)
	srv.handleApproveDraft(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var d model.Draft
	json.NewDecoder(w.Body).Decode(&d)
	if d.Status != model.DraftStatusApproved {
		t.Errorf("expected approved, got %s", d.Status)
	}
	if d.SentBody != "No, the claims stay the same." {
		t.Errorf("expected SentBody to match DraftBody, got %q", d.SentBody)
	}
}

func TestApproveDraft_PassesThreadTS(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	var capturedThreadTS string
	srv.slackSender = func(_ context.Context, _, _, _, threadTS string) error {
		capturedThreadTS = threadTS
		return nil
	}

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims)
	srv.handleApproveDraft(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedThreadTS != "1234567890.123456" {
		t.Errorf("expected threadTS = %q, got %q", "1234567890.123456", capturedThreadTS)
	}
}

func TestEditDraft_PassesThreadTS(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	var capturedThreadTS string
	srv.slackSender = func(_ context.Context, _, _, _, threadTS string) error {
		capturedThreadTS = threadTS
		return nil
	}

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/edit", `{"body":"edited reply"}`, userAClaims)
	srv.handleEditDraft(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedThreadTS != "1234567890.123456" {
		t.Errorf("expected threadTS = %q, got %q", "1234567890.123456", capturedThreadTS)
	}
}

func TestDeliverEphemeralDraft_ThreadTS_WhenRepliesExist(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()

	// Mock thread checker returns true — thread has replies.
	srv.threadChecker = func(_ context.Context, _, _, _ string) bool { return true }

	var capturedThreadTS string
	srv.ephemeralPoster = func(_ context.Context, msg comms.SlackDraftMessage) error {
		capturedThreadTS = msg.ThreadTS
		return nil
	}

	draft := model.Draft{
		ID:        "dft_1",
		UserID:    "usr_a",
		Service:   "slack",
		Channel:   "C0BACKEND",
		Author:    "Sarah",
		DraftBody: "Draft reply",
		MessageTS: "1234567890.123456",
	}
	srv.deliverEphemeralDraft(ctx, "usr_a", draft)

	if capturedThreadTS != "1234567890.123456" {
		t.Errorf("expected ThreadTS when thread has replies, got %q", capturedThreadTS)
	}
}

func TestDeliverEphemeralDraft_NoThreadTS_WhenNoReplies(t *testing.T) {
	// Ephemeral drafts should NOT be posted in-thread when the thread has
	// no replies yet — Slack silently drops them. ThreadTS should be empty
	// so the ephemeral appears at channel level.
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()

	var capturedThreadTS string
	srv.ephemeralPoster = func(_ context.Context, msg comms.SlackDraftMessage) error {
		capturedThreadTS = msg.ThreadTS
		return nil
	}

	draft := model.Draft{
		ID:        "dft_1",
		UserID:    "usr_a",
		Status:    model.DraftStatusPending,
		Service:   "slack",
		Channel:   "C0BACKEND",
		Author:    "Sarah",
		DraftBody: "Draft reply",
		MessageTS: "1234567890.123456",
	}
	srv.deliverEphemeralDraft(ctx, "usr_a", draft)

	// SlackThreadHasReplies returns false in tests (no real Slack API),
	// so ThreadTS should be empty — ephemeral at channel level.
	if capturedThreadTS != "" {
		t.Errorf("expected empty ThreadTS (no thread replies), got %q", capturedThreadTS)
	}
}

func TestApproveDraft_OwnershipCheck(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userBClaims)
	srv.handleApproveDraft(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestApproveDraft_AlreadyActioned(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusDiscarded)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims)
	srv.handleApproveDraft(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestApproveDraft_NotFound(t *testing.T) {
	srv := newDraftsTestServerWithSend()

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/nonexistent/approve", "", userAClaims)
	srv.handleApproveDraft(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestApproveDraft_SendFails(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	srv.slackSender = func(_ context.Context, _, _, _, _ string) error {
		return fmt.Errorf("slack API unavailable")
	}
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims)
	srv.handleApproveDraft(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveDraft_NoSlackAccount(t *testing.T) {
	srv := newDraftsTestServer() // no connected account seeded
	srv.slackSender = func(_ context.Context, _, _, _, _ string) error { return nil }
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims)
	srv.handleApproveDraft(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditDraft_Success(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/edit", `{"body":"Actually, yes it does change."}`, userAClaims)
	srv.handleEditDraft(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var d model.Draft
	json.NewDecoder(w.Body).Decode(&d)
	if d.Status != model.DraftStatusEdited {
		t.Errorf("expected edited, got %s", d.Status)
	}
	if d.SentBody != "Actually, yes it does change." {
		t.Errorf("expected edited SentBody, got %q", d.SentBody)
	}
	// DraftBody should still be the original.
	if d.DraftBody != "No, the claims stay the same." {
		t.Errorf("expected original DraftBody preserved, got %q", d.DraftBody)
	}
}

func TestEditDraft_SendFails(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	srv.slackSender = func(_ context.Context, _, _, _, _ string) error {
		return fmt.Errorf("slack error")
	}
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/edit", `{"body":"revised"}`, userAClaims)
	srv.handleEditDraft(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestDraftAction_UnknownAction(t *testing.T) {
	srv := newDraftsTestServer()

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/unknown", "", userAClaims)
	srv.handleDraftAction(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown action, got %d", w.Code)
	}
}

func TestDraftAction_ApproveRouting(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims)
	srv.handleDraftAction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 via action router for approve, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDraftAction_EditRouting(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/edit", `{"body":"revised"}`, userAClaims)
	srv.handleDraftAction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 via action router for edit, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateDraftFromMessage(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()

	msg := comms.IncomingMessage{
		ID:      "1234567890.123456",
		Service: "slack",
		Channel: "C0BACKEND",
		Author:  "Sarah",
		Body:    "Does the JWT refactor change the claims?",
	}

	d, err := srv.createDraftFromMessage(ctx, "usr_a", msg, "No, the claims stay the same.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ID == "" {
		t.Error("expected non-empty ID")
	}
	if d.Status != model.DraftStatusPending {
		t.Errorf("expected pending, got %s", d.Status)
	}
	if d.UserID != "usr_a" {
		t.Errorf("expected usr_a, got %s", d.UserID)
	}
	if d.DraftBody != "No, the claims stay the same." {
		t.Errorf("unexpected draft body: %s", d.DraftBody)
	}
	if d.MessageTS != "1234567890.123456" {
		t.Errorf("expected message timestamp preserved, got %s", d.MessageTS)
	}

	// Verify it's in the store.
	got, err := srv.drafts.Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("draft not in store: %v", err)
	}
	if got.Channel != "C0BACKEND" {
		t.Errorf("expected C0BACKEND, got %s", got.Channel)
	}
}

func TestSendDraftMessage_BadTokenJSON(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	// Store invalid JSON as token.
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack", []byte("not-json"), vault.Metadata{})

	err := srv.sendDraftMessage(ctx, "usr_a", "C123", "hello", "")
	if err == nil {
		t.Fatal("expected error for bad token JSON")
	}
}

func TestSendDraftMessage_EmptyAccessToken(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack", []byte(`{"token_type":"user"}`), vault.Metadata{})

	err := srv.sendDraftMessage(ctx, "usr_a", "C123", "hello", "")
	if err == nil {
		t.Fatal("expected error for missing access_token")
	}
}

func TestListDrafts_Empty(t *testing.T) {
	srv := newDraftsTestServer()

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/drafts", "", userAClaims)
	srv.handleListDrafts(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Items []model.Draft `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 0 {
		t.Fatalf("expected empty list, got %d", len(resp.Items))
	}
}

func TestGetDraft_EmptyID(t *testing.T) {
	srv := newDraftsTestServer()

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/drafts/", "", userAClaims)
	srv.handleGetDraft(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty ID, got %d", w.Code)
	}
}

func TestDeliverEphemeralDraft_Success(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	var posted bool
	srv.ephemeralPoster = func(_ context.Context, msg comms.SlackDraftMessage) error {
		posted = true
		if msg.BotToken != "xoxb-test" {
			t.Errorf("expected bot token xoxb-test, got %s", msg.BotToken)
		}
		if msg.UserID != "U123ABC" {
			t.Errorf("expected user ID U123ABC, got %s", msg.UserID)
		}
		if msg.DraftID != "dft_eph" {
			t.Errorf("expected draft ID dft_eph, got %s", msg.DraftID)
		}
		return nil
	}

	ctx := context.Background()
	srv.deliverEphemeralDraft(ctx, "usr_a", model.Draft{
		ID:        "dft_eph",
		UserID:    "usr_a",
		Channel:   "C0BACKEND",
		Author:    "Sarah",
		DraftBody: "Draft reply text",
	})

	if !posted {
		t.Error("expected ephemeral to be posted")
	}
}

func TestDeliverEphemeralDraft_NoBotToken(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()

	// Connected account without bot token.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U123",
	})
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{})

	var posted bool
	srv.ephemeralPoster = func(_ context.Context, _ comms.SlackDraftMessage) error {
		posted = true
		return nil
	}

	srv.deliverEphemeralDraft(ctx, "usr_a", model.Draft{ID: "dft_1", Channel: "C123", DraftBody: "test"})
	if posted {
		t.Error("should not post when bot token missing")
	}
}

func TestDeliverEphemeralDraft_NoSlackAccount(t *testing.T) {
	srv := newDraftsTestServer()
	var posted bool
	srv.ephemeralPoster = func(_ context.Context, _ comms.SlackDraftMessage) error {
		posted = true
		return nil
	}

	srv.deliverEphemeralDraft(context.Background(), "usr_a", model.Draft{ID: "dft_1"})
	if posted {
		t.Error("should not post when no slack account")
	}
}

func TestDeliverEphemeralDraft_BadTokenJSON(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U123",
	})
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack", []byte("not-json"), vault.Metadata{})

	var posted bool
	srv.ephemeralPoster = func(_ context.Context, _ comms.SlackDraftMessage) error {
		posted = true
		return nil
	}

	srv.deliverEphemeralDraft(ctx, "usr_a", model.Draft{ID: "dft_1", Channel: "C123", DraftBody: "test"})
	if posted {
		t.Error("should not post when token JSON is invalid")
	}
}

func TestDeliverEphemeralDraft_PosterError(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	srv.ephemeralPoster = func(_ context.Context, _ comms.SlackDraftMessage) error {
		return fmt.Errorf("slack API unavailable")
	}

	// Should not panic — just logs the error.
	srv.deliverEphemeralDraft(context.Background(), "usr_a", model.Draft{
		ID: "dft_1", Channel: "C0BACKEND", Author: "Sarah", DraftBody: "test",
	})
}

func TestRecordFeedback_NilStore(t *testing.T) {
	srv := newDraftsTestServer()
	srv.feedback = nil
	// Should not panic.
	srv.recordFeedback(context.Background(), model.Draft{ID: "dft_1"}, model.FeedbackSignalApproved)
}

func TestDeliverEphemeralDraft_NoExternalUserID(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive,
		// ExternalUserID is empty
	})
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack", []byte(`{"access_token":"xoxp","bot_access_token":"xoxb"}`), vault.Metadata{})

	var posted bool
	srv.ephemeralPoster = func(_ context.Context, _ comms.SlackDraftMessage) error {
		posted = true
		return nil
	}

	srv.deliverEphemeralDraft(ctx, "usr_a", model.Draft{ID: "dft_1", Channel: "C123", DraftBody: "test"})
	if posted {
		t.Error("should not post when external user ID missing")
	}
}

func TestExtractDraftID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/drafts/dft_123", "dft_123"},
		{"/v1/drafts/dft_123/", "dft_123"},
		{"/v1/drafts/", ""},
		{"/other/path", ""},
	}
	for _, tt := range tests {
		got := extractDraftID(tt.path)
		if got != tt.want {
			t.Errorf("extractDraftID(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestExtractDraftIDFromAction(t *testing.T) {
	tests := []struct {
		path   string
		action string
		want   string
	}{
		{"/v1/drafts/dft_123/approve", "/approve", "dft_123"},
		{"/v1/drafts/dft_123/edit", "/edit", "dft_123"},
		{"/other/path/approve", "/approve", ""},
	}
	for _, tt := range tests {
		got := extractDraftIDFromAction(tt.path, tt.action)
		if got != tt.want {
			t.Errorf("extractDraftIDFromAction(%q, %q) = %q, want %q", tt.path, tt.action, got, tt.want)
		}
	}
}

func TestApproveDraft_RecordsFeedback(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims)
	srv.handleApproveDraft(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	fb, _ := srv.feedback.List(ctx, store.DraftFeedbackFilter{UserID: "usr_a"})
	if len(fb) != 1 {
		t.Fatalf("expected 1 feedback signal, got %d", len(fb))
	}
	if fb[0].Signal != model.FeedbackSignalApproved {
		t.Errorf("expected approved, got %s", fb[0].Signal)
	}
	if fb[0].DraftID != "dft_1" {
		t.Errorf("expected dft_1, got %s", fb[0].DraftID)
	}
	if fb[0].SentBody != "No, the claims stay the same." {
		t.Errorf("expected SentBody = DraftBody, got %q", fb[0].SentBody)
	}
}

func TestEditDraft_RecordsFeedback(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/edit", `{"body":"Actually yes it does."}`, userAClaims)
	srv.handleEditDraft(w, r)

	fb, _ := srv.feedback.List(ctx, store.DraftFeedbackFilter{UserID: "usr_a"})
	if len(fb) != 1 {
		t.Fatalf("expected 1 feedback, got %d", len(fb))
	}
	if fb[0].Signal != model.FeedbackSignalEdited {
		t.Errorf("expected edited, got %s", fb[0].Signal)
	}
	if fb[0].DraftBody != "No, the claims stay the same." {
		t.Errorf("expected original DraftBody preserved, got %q", fb[0].DraftBody)
	}
	if fb[0].SentBody != "Actually yes it does." {
		t.Errorf("expected edited SentBody, got %q", fb[0].SentBody)
	}
}

func TestDiscardDraft_RecordsFeedback(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/discard", "", userAClaims)
	srv.handleDiscardDraft(w, r)

	fb, _ := srv.feedback.List(ctx, store.DraftFeedbackFilter{UserID: "usr_a"})
	if len(fb) != 1 {
		t.Fatalf("expected 1 feedback, got %d", len(fb))
	}
	if fb[0].Signal != model.FeedbackSignalDiscarded {
		t.Errorf("expected discarded, got %s", fb[0].Signal)
	}
	if fb[0].SentBody != "" {
		t.Errorf("expected empty SentBody for discard, got %q", fb[0].SentBody)
	}
}

func TestListFeedback_Success(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()

	// Generate feedback by approving and discarding drafts.
	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)
	w := httptest.NewRecorder()
	srv.handleApproveDraft(w, mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims))

	seedDraft(ctx, srv.drafts, "dft_2", "usr_a", model.DraftStatusPending)
	w = httptest.NewRecorder()
	srv.handleDiscardDraft(w, mcpRequest("POST", "/v1/drafts/dft_2/discard", "", userAClaims))

	// List all feedback.
	w = httptest.NewRecorder()
	srv.handleListFeedback(w, mcpRequest("GET", "/v1/feedback", "", userAClaims))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Items []model.DraftFeedback `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 feedback items, got %d", len(resp.Items))
	}
}

func TestListFeedback_FilterBySignal(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	ctx := context.Background()

	seedDraft(ctx, srv.drafts, "dft_1", "usr_a", model.DraftStatusPending)
	w := httptest.NewRecorder()
	srv.handleApproveDraft(w, mcpRequest("POST", "/v1/drafts/dft_1/approve", "", userAClaims))

	seedDraft(ctx, srv.drafts, "dft_2", "usr_a", model.DraftStatusPending)
	w = httptest.NewRecorder()
	srv.handleDiscardDraft(w, mcpRequest("POST", "/v1/drafts/dft_2/discard", "", userAClaims))

	// Filter by approved only.
	w = httptest.NewRecorder()
	srv.handleListFeedback(w, mcpRequest("GET", "/v1/feedback?signal=approved", "", userAClaims))

	var resp struct {
		Items []model.DraftFeedback `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 approved feedback, got %d", len(resp.Items))
	}
}

func TestListFeedback_Unauthorized(t *testing.T) {
	srv := newDraftsTestServer()
	w := httptest.NewRecorder()
	srv.handleListFeedback(w, mcpRequest("GET", "/v1/feedback", "", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestListFeedback_Empty(t *testing.T) {
	srv := newDraftsTestServer()
	w := httptest.NewRecorder()
	srv.handleListFeedback(w, mcpRequest("GET", "/v1/feedback", "", userAClaims))

	var resp struct {
		Items []model.DraftFeedback `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 0 {
		t.Fatalf("expected empty, got %d", len(resp.Items))
	}
}

func TestListFeedback_NilStore(t *testing.T) {
	srv := newDraftsTestServer()
	srv.feedback = nil

	w := httptest.NewRecorder()
	srv.handleListFeedback(w, mcpRequest("GET", "/v1/feedback", "", userAClaims))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
