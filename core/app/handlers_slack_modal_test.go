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
