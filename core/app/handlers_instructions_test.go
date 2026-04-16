package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func newInstructionsTestServer() *apiServer {
	return &apiServer{
		log:          slog.Default(),
		instructions: mem.NewUserInstructionStore(),
		users:        &stubUserStore{},
		newID:        func() string { return "test-id" },
	}
}

func TestCreateInstruction_Success(t *testing.T) {
	srv := newInstructionsTestServer()
	body := `{"body":"Always reference PR numbers","scope":"#backend"}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/instructions", body, userAClaims)
	srv.handleCreateInstruction(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var ins model.UserInstruction
	json.NewDecoder(w.Body).Decode(&ins)
	if ins.Body != "Always reference PR numbers" {
		t.Errorf("unexpected body: %s", ins.Body)
	}
	if ins.Scope != "#backend" {
		t.Errorf("unexpected scope: %s", ins.Scope)
	}
	if !ins.Active {
		t.Error("expected active by default")
	}
	if ins.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestCreateInstruction_EmptyBody(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/instructions", `{"body":""}`, userAClaims)
	srv.handleCreateInstruction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateInstruction_InvalidJSON(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/instructions", "not-json", userAClaims)
	srv.handleCreateInstruction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateInstruction_Unauthorized(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/instructions", `{"body":"test"}`, nil)
	srv.handleCreateInstruction(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestListInstructions_Active(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule 1", Active: true})
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_2", UserID: "usr_a", Body: "Rule 2", Active: false})

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/instructions?active=true", "", userAClaims)
	srv.handleListInstructions(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Items []model.UserInstruction `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 active, got %d", len(resp.Items))
	}
}

func TestListInstructions_All(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule 1", Active: true})
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_2", UserID: "usr_a", Body: "Rule 2", Active: false})

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/instructions", "", userAClaims)
	srv.handleListInstructions(w, r)

	var resp struct {
		Items []model.UserInstruction `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2, got %d", len(resp.Items))
	}
}

func TestListInstructions_Empty(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/instructions", "", userAClaims)
	srv.handleListInstructions(w, r)

	var resp struct {
		Items []model.UserInstruction `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 0 {
		t.Fatalf("expected empty, got %d", len(resp.Items))
	}
}

func TestGetInstruction_Success(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule 1", Active: true})

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/instructions/ins_1", "", userAClaims)
	srv.handleGetInstruction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetInstruction_OwnershipCheck(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule 1"})

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/instructions/ins_1", "", userBClaims)
	srv.handleGetInstruction(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetInstruction_NotFound(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/instructions/nonexistent", "", userAClaims)
	srv.handleGetInstruction(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetInstruction_EmptyID(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/instructions/", "", userAClaims)
	srv.handleGetInstruction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateInstruction_Body(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Original", Active: true})

	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/ins_1", `{"body":"Updated"}`, userAClaims)
	srv.handleUpdateInstruction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var ins model.UserInstruction
	json.NewDecoder(w.Body).Decode(&ins)
	if ins.Body != "Updated" {
		t.Errorf("expected Updated, got %s", ins.Body)
	}
}

func TestUpdateInstruction_Toggle(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule", Active: true})

	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/ins_1", `{"active":false}`, userAClaims)
	srv.handleUpdateInstruction(w, r)

	var ins model.UserInstruction
	json.NewDecoder(w.Body).Decode(&ins)
	if ins.Active {
		t.Error("expected inactive")
	}
}

func TestUpdateInstruction_EmptyBody(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule", Active: true})

	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/ins_1", `{"body":""}`, userAClaims)
	srv.handleUpdateInstruction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateInstruction_NotFound(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/nonexistent", `{"body":"x"}`, userAClaims)
	srv.handleUpdateInstruction(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateInstruction_OwnershipCheck(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule"})

	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/ins_1", `{"body":"hacked"}`, userBClaims)
	srv.handleUpdateInstruction(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteInstruction_Success(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule"})

	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/instructions/ins_1", "", userAClaims)
	srv.handleDeleteInstruction(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestDeleteInstruction_NotFound(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/instructions/nonexistent", "", userAClaims)
	srv.handleDeleteInstruction(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteInstruction_OwnershipCheck(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule"})

	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/instructions/ins_1", "", userBClaims)
	srv.handleDeleteInstruction(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestInstructionByID_Routing(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule", Active: true})

	// GET routes correctly.
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/instructions/ins_1", "", userAClaims)
	srv.handleInstructionByID(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", w.Code)
	}

	// Unknown method.
	w = httptest.NewRecorder()
	r = mcpRequest("PUT", "/v1/instructions/ins_1", "", userAClaims)
	srv.handleInstructionByID(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT expected 405, got %d", w.Code)
	}
}

func TestUpdateInstruction_InvalidJSON(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule", Active: true})

	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/ins_1", "not-json", userAClaims)
	srv.handleUpdateInstruction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateInstruction_EmptyID(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/", `{"body":"x"}`, userAClaims)
	srv.handleUpdateInstruction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateInstruction_Scope(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule", Scope: "old", Active: true})

	scope := "new scope"
	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/ins_1", `{"scope":"`+scope+`"}`, userAClaims)
	srv.handleUpdateInstruction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var ins model.UserInstruction
	json.NewDecoder(w.Body).Decode(&ins)
	if ins.Scope != "new scope" {
		t.Errorf("expected 'new scope', got %s", ins.Scope)
	}
}

func TestDeleteInstruction_EmptyID(t *testing.T) {
	srv := newInstructionsTestServer()
	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/instructions/", "", userAClaims)
	srv.handleDeleteInstruction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestInstructionByID_PatchRouting(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule", Active: true})

	w := httptest.NewRecorder()
	r := mcpRequest("PATCH", "/v1/instructions/ins_1", `{"body":"Updated via router"}`, userAClaims)
	srv.handleInstructionByID(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionByID_DeleteRouting(t *testing.T) {
	srv := newInstructionsTestServer()
	ctx := context.Background()
	srv.instructions.Create(ctx, model.UserInstruction{ID: "ins_1", UserID: "usr_a", Body: "Rule"})

	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/instructions/ins_1", "", userAClaims)
	srv.handleInstructionByID(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestExtractInstructionID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/instructions/ins_123", "ins_123"},
		{"/v1/instructions/ins_123/", "ins_123"},
		{"/v1/instructions/", ""},
		{"/other", ""},
	}
	for _, tt := range tests {
		got := extractInstructionID(tt.path)
		if got != tt.want {
			t.Errorf("extractInstructionID(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
