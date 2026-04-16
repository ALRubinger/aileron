package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// handleCreateInstruction creates a new user instruction.
// POST /v1/instructions
func (s *apiServer) handleCreateInstruction(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	var req struct {
		Body  string `json:"body"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "body field is required")
		return
	}

	now := time.Now().UTC()
	ins := model.UserInstruction{
		ID:        "ins_" + s.newID(),
		UserID:    userID,
		Body:      req.Body,
		Scope:     req.Scope,
		Active:    true,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.instructions.Create(r.Context(), ins); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, ins)
}

// handleListInstructions returns the user's instructions.
// GET /v1/instructions?active=true
func (s *apiServer) handleListInstructions(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	filter := store.UserInstructionFilter{UserID: userID}
	if activeParam := r.URL.Query().Get("active"); activeParam != "" {
		active := activeParam == "true"
		filter.Active = &active
	}

	instructions, err := s.instructions.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if instructions == nil {
		instructions = []model.UserInstruction{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": instructions})
}

// handleGetInstruction returns a specific instruction with ownership check.
// GET /v1/instructions/{id}
func (s *apiServer) handleGetInstruction(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	insID := extractInstructionID(r.URL.Path)
	if insID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "instruction_id is required")
		return
	}

	ins, err := s.instructions.Get(r.Context(), insID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "instruction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if ins.UserID != userID {
		writeError(w, http.StatusNotFound, "not_found", "instruction not found")
		return
	}

	writeJSON(w, http.StatusOK, ins)
}

// handleUpdateInstruction updates an instruction's body, scope, or active flag.
// PATCH /v1/instructions/{id}
func (s *apiServer) handleUpdateInstruction(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	insID := extractInstructionID(r.URL.Path)
	if insID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "instruction_id is required")
		return
	}

	ins, err := s.instructions.Get(r.Context(), insID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "instruction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if ins.UserID != userID {
		writeError(w, http.StatusNotFound, "not_found", "instruction not found")
		return
	}

	var req struct {
		Body   *string `json:"body"`
		Scope  *string `json:"scope"`
		Active *bool   `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}

	if req.Body != nil {
		if *req.Body == "" {
			writeError(w, http.StatusBadRequest, "invalid_body", "body cannot be empty")
			return
		}
		ins.Body = *req.Body
	}
	if req.Scope != nil {
		ins.Scope = *req.Scope
	}
	if req.Active != nil {
		ins.Active = *req.Active
	}
	ins.UpdatedAt = time.Now().UTC()

	if err := s.instructions.Update(r.Context(), ins); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ins)
}

// handleDeleteInstruction permanently removes an instruction.
// DELETE /v1/instructions/{id}
func (s *apiServer) handleDeleteInstruction(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	insID := extractInstructionID(r.URL.Path)
	if insID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "instruction_id is required")
		return
	}

	ins, err := s.instructions.Get(r.Context(), insID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "instruction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if ins.UserID != userID {
		writeError(w, http.StatusNotFound, "not_found", "instruction not found")
		return
	}

	if err := s.instructions.Delete(r.Context(), insID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleInstructionByID routes GET/PATCH/DELETE to the correct handler based on method.
func (s *apiServer) handleInstructionByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetInstruction(w, r)
	case http.MethodPatch:
		s.handleUpdateInstruction(w, r)
	case http.MethodDelete:
		s.handleDeleteInstruction(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// extractInstructionID extracts the instruction ID from /v1/instructions/{id}
func extractInstructionID(path string) string {
	const prefix = "/v1/instructions/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	id := strings.TrimPrefix(path, prefix)
	id = strings.TrimSuffix(id, "/")
	return id
}
