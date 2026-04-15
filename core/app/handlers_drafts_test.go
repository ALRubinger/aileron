package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func newDraftsTestServer() *apiServer {
	return &apiServer{
		log:               slog.Default(),
		drafts:            mem.NewDraftStore(),
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

func TestDraftAction_UnknownAction(t *testing.T) {
	srv := newDraftsTestServer()

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/drafts/dft_1/unknown", "", userAClaims)
	srv.handleDraftAction(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown action, got %d", w.Code)
	}
}
