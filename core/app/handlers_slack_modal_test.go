package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
)

// slackViewOK returns a Slack API response with an "ok" status and a view ID.
func slackViewOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":   true,
		"view": map[string]any{"id": "V_MODAL_123"},
	})
}

func TestOpenDraftModal_Success(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		slackViewOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	meta := DraftModalMeta{
		TargetChannel:   "C123",
		TargetThreadTS:  "111.222",
		OriginalMessage: "Help needed",
		UserID:          "U_ALICE",
	}

	viewResp, err := openDraftModal(context.Background(), "xoxb-test", "trigger_123", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if viewResp == nil {
		t.Fatal("expected non-nil view response")
	}
	if viewResp.ID != "V_MODAL_123" {
		t.Errorf("expected view ID V_MODAL_123, got %q", viewResp.ID)
	}
	// Verify it called the views.open endpoint.
	if !strings.Contains(receivedPath, "views.open") {
		t.Errorf("expected views.open call, got path %q", receivedPath)
	}
}

func TestUpdateDraftModalWithDraft_Success(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		slackViewOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	meta := DraftModalMeta{
		TargetChannel: "C123",
		UserID:        "U_ALICE",
	}

	err := updateDraftModalWithDraft(context.Background(), "xoxb-test", "V_123", "Here is your draft.", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(receivedPath, "views.update") {
		t.Errorf("expected views.update call, got path %q", receivedPath)
	}
}

func TestUpdateDraftModalWithDraft_Truncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackViewOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	meta := DraftModalMeta{TargetChannel: "C123", UserID: "U_ALICE"}

	// Create a string longer than maxModalTextLength (3000).
	longDraft := strings.Repeat("A", 4000)
	err := updateDraftModalWithDraft(context.Background(), "xoxb-test", "V_123", longDraft, meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No panic, truncation handled internally.
}

func TestOpenDraftInstructionsModal_Success(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		slackViewOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	meta := DraftModalMeta{
		TargetChannel:   "C123",
		TargetThreadTS:  "111.222",
		OriginalMessage: "Can someone review PR #42?",
		UserID:          "U_ALICE",
		TeamID:          "T001",
	}

	viewResp, err := openDraftInstructionsModal(context.Background(), "xoxb-test", "trigger_456", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if viewResp == nil {
		t.Fatal("expected non-nil view response")
	}
	if viewResp.ID != "V_MODAL_123" {
		t.Errorf("expected view ID V_MODAL_123, got %q", viewResp.ID)
	}
	if !strings.Contains(receivedPath, "views.open") {
		t.Errorf("expected views.open call, got path %q", receivedPath)
	}
}

func TestOpenDraftInstructionsModal_LongMessageTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackViewOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	longMessage := strings.Repeat("A", 300)
	meta := DraftModalMeta{
		TargetChannel:   "C123",
		OriginalMessage: longMessage,
		UserID:          "U_ALICE",
	}

	viewResp, err := openDraftInstructionsModal(context.Background(), "xoxb-test", "trigger_789", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if viewResp == nil {
		t.Fatal("expected non-nil view response")
	}
}

func TestBuildDraftLoadingView(t *testing.T) {
	meta := DraftModalMeta{
		TargetChannel:   "C123",
		OriginalMessage: "Test message",
		UserID:          "U_ALICE",
		Instructions:    "Make it polite",
	}

	view := buildDraftLoadingView(meta)

	if view.CallbackID != draftModalCallbackID {
		t.Errorf("expected callback_id %q, got %q", draftModalCallbackID, view.CallbackID)
	}
	if len(view.Blocks.BlockSet) != 1 {
		t.Fatalf("expected 1 block, got %d", len(view.Blocks.BlockSet))
	}

	// Verify metadata round-trips correctly including instructions.
	parsed, err := parseDraftModalMeta(view.PrivateMetadata)
	if err != nil {
		t.Fatalf("failed to parse metadata: %v", err)
	}
	if parsed.Instructions != "Make it polite" {
		t.Errorf("expected instructions 'Make it polite', got %q", parsed.Instructions)
	}
}

func TestUpdateDraftModalError_Success(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		slackViewOK(w)
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	meta := DraftModalMeta{
		TargetChannel: "C123",
		UserID:        "U_ALICE",
	}

	err := updateDraftModalError(context.Background(), "xoxb-test", "V_123", "Something went wrong.", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(receivedPath, "views.update") {
		t.Errorf("expected views.update call, got path %q", receivedPath)
	}
}
