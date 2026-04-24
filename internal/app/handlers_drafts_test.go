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

	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
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
	enableVaultEncryption(srv, "usr_a")
	// Set up a connected Slack account + encrypted user token so send works.
	ctx := context.Background()
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_s1",
		UserID:         "usr_a",
		Provider:       model.ConnectedAccountProviderSlack,
		Status:         model.ConnectedAccountStatusActive,
		ExternalUserID: "U123ABC",
		ExternalTeamID: "T001TEST",
	})
	storeEncryptedToken(srv, "connected-accounts/usr_a/slack", []byte(`{"access_token":"xoxp-test"}`))
	// Store workspace-level bot token in the system vault (installed by admin).
	srv.systemVault = vault.NewMemVault()
	srv.systemVault.Put(ctx, "slack-workspaces/T001TEST/bot-token", []byte("xoxb-test"), vault.Metadata{
		Type: "slack_bot_token",
	})
	// Mock sender so we don't call real Slack.
	srv.slackSender = func(_ context.Context, token, channel, body, threadTS string) error {
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
	enableVaultEncryption(srv, "usr_a")
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	// Store invalid JSON as token.
	storeEncryptedToken(srv, "connected-accounts/usr_a/slack", []byte("not-json"))

	err := srv.sendDraftMessage(ctx, "usr_a", "C123", "hello", "")
	if err == nil {
		t.Fatal("expected error for bad token JSON")
	}
}

func TestSendDraftMessage_EmptyAccessToken(t *testing.T) {
	srv := newDraftsTestServer()
	enableVaultEncryption(srv, "usr_a")
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	storeEncryptedToken(srv, "connected-accounts/usr_a/slack", []byte(`{"token_type":"user"}`))

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

func TestRecordFeedback_NilStore(t *testing.T) {
	srv := newDraftsTestServer()
	srv.feedback = nil
	// Should not panic.
	srv.recordFeedback(context.Background(), model.Draft{ID: "dft_1"}, model.FeedbackSignalApproved)
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

func TestResolveSlackCredentials_Success(t *testing.T) {
	srv := newDraftsTestServerWithSend()
	creds, err := srv.resolveSlackCredentials(context.Background(), "usr_a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.BotToken != "xoxb-test" {
		t.Errorf("expected xoxb-test, got %s", creds.BotToken)
	}
	if creds.SlackUserID != "U123ABC" {
		t.Errorf("expected U123ABC, got %s", creds.SlackUserID)
	}
}

func TestResolveSlackCredentials_NoAccount(t *testing.T) {
	srv := newDraftsTestServer()
	_, err := srv.resolveSlackCredentials(context.Background(), "usr_a")
	if err == nil {
		t.Fatal("expected error when no slack account")
	}
}

func TestResolveSlackCredentials_NoBotToken(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U123", ExternalTeamID: "T001",
	})
	// No workspace bot token stored — admin hasn't installed the app yet.

	_, err := srv.resolveSlackCredentials(ctx, "usr_a")
	if err == nil {
		t.Fatal("expected error when workspace bot token missing")
	}
}

func TestResolveSlackCredentials_NoExternalUserID(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalTeamID: "T001",
		// No ExternalUserID
	})

	_, err := srv.resolveSlackCredentials(ctx, "usr_a")
	if err == nil {
		t.Fatal("expected error when external user ID missing")
	}
}

func TestResolveSlackCredentials_NoTeamID(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U123",
		// No ExternalTeamID
	})

	_, err := srv.resolveSlackCredentials(ctx, "usr_a")
	if err == nil {
		t.Fatal("expected error when team ID missing")
	}
}

func TestResolveSlackCredentials_UsesSystemVault(t *testing.T) {
	// Regression: bot token must come from systemVault, not vault (user vault).
	// The install handler writes to systemVault; resolveSlackCredentials must read from there.
	srv := newDraftsTestServer()
	ctx := context.Background()
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U123", ExternalTeamID: "T001",
	})

	// Store bot token ONLY in systemVault, not in vault.
	srv.systemVault = vault.NewMemVault()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-from-system"), vault.Metadata{
		Type: "slack_bot_token",
	})
	// Ensure user vault does NOT have the bot token.
	// (vault is already empty for this path)

	creds, err := srv.resolveSlackCredentials(ctx, "usr_a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.BotToken != "xoxb-from-system" {
		t.Errorf("expected bot token from systemVault, got %q", creds.BotToken)
	}
}

func TestResolveSlackCredentials_NilSystemVault(t *testing.T) {
	srv := newDraftsTestServer()
	ctx := context.Background()
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U123", ExternalTeamID: "T001",
	})
	// systemVault is nil — should return an error, not panic.
	srv.systemVault = nil

	_, err := srv.resolveSlackCredentials(ctx, "usr_a")
	if err == nil {
		t.Fatal("expected error when systemVault is nil")
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
