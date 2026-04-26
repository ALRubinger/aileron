package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

// handleUpsertUserLLMConfig creates or updates the calling user's LLM config.
// PUT /v1/llm-config
func (s *apiServer) handleUpsertUserLLMConfig(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	var req struct {
		Provider       string `json:"provider"`
		ModelResearch  string `json:"model_research"`
		ModelSynthesis string `json:"model_synthesis"`
		APIKey         string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}
	if req.Provider == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "provider is required")
		return
	}
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "api_key is required")
		return
	}

	// Production enclave mode: reject plaintext API key ingress. In TEE
	// deployments, credentials must never traverse host memory as plaintext.
	// The local dev provider is exempt since it has no real isolation.
	if s.enclaveClient != nil && s.teeCfg != nil && s.teeCfg.Provider != "local" {
		writeError(w, http.StatusForbidden, "enclave_mode", "plaintext API key submission is not permitted in enclave mode")
		return
	}

	now := time.Now().UTC()
	cfg := model.LLMConfig{
		ID:             "llm_" + s.newID(),
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        userID,
		Provider:       model.LLMProvider(req.Provider),
		ModelResearch:  req.ModelResearch,
		ModelSynthesis: req.ModelSynthesis,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// Store API key in vault.
	if err := s.vault.Put(r.Context(), cfg.VaultPath(), []byte(req.APIKey), vault.Metadata{
		Type:   "llm_api_key",
		Labels: map[string]string{"owner_type": "user", "owner_id": userID},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to store API key")
		return
	}

	result, err := s.llmConfigs.Upsert(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, llmConfigResponse(result, true))
}

// handleGetUserLLMConfig returns the calling user's LLM config.
// GET /v1/llm-config
func (s *apiServer) handleGetUserLLMConfig(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	cfg, err := s.llmConfigs.GetByOwner(r.Context(), model.LLMConfigOwnerUser, userID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "no LLM configuration found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, llmConfigResponse(cfg, true))
}

// handleDeleteUserLLMConfig deletes the calling user's LLM config.
// DELETE /v1/llm-config
func (s *apiServer) handleDeleteUserLLMConfig(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	cfg, err := s.llmConfigs.GetByOwner(r.Context(), model.LLMConfigOwnerUser, userID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "no LLM configuration found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Remove API key from vault.
	_ = s.vault.Delete(r.Context(), cfg.VaultPath())

	if err := s.llmConfigs.Delete(r.Context(), cfg.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleUpsertEnterpriseLLMConfig creates or updates an enterprise's LLM config.
// PUT /v1/enterprises/{id}/llm-config
func (s *apiServer) handleUpsertEnterpriseLLMConfig(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	enterpriseID := extractEnterpriseLLMConfigID(r.URL.Path)
	if enterpriseID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "enterprise_id is required")
		return
	}

	// Verify caller has admin/owner access to this enterprise.
	if !s.requireEnterpriseAdmin(w, r, enterpriseID) {
		return
	}

	var req struct {
		Provider       string `json:"provider"`
		ModelResearch  string `json:"model_research"`
		ModelSynthesis string `json:"model_synthesis"`
		APIKey         string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}
	if req.Provider == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "provider is required")
		return
	}
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "api_key is required")
		return
	}

	// Enclave mode: reject plaintext API key ingress.
	if s.enclaveClient != nil {
		writeError(w, http.StatusForbidden, "enclave_mode", "plaintext API key submission is not permitted in enclave mode")
		return
	}

	now := time.Now().UTC()
	cfg := model.LLMConfig{
		ID:             "llm_" + s.newID(),
		OwnerType:      model.LLMConfigOwnerEnterprise,
		OwnerID:        enterpriseID,
		Provider:       model.LLMProvider(req.Provider),
		ModelResearch:  req.ModelResearch,
		ModelSynthesis: req.ModelSynthesis,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := s.vault.Put(r.Context(), cfg.VaultPath(), []byte(req.APIKey), vault.Metadata{
		Type:   "llm_api_key",
		Labels: map[string]string{"owner_type": "enterprise", "owner_id": enterpriseID},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to store API key")
		return
	}

	result, err := s.llmConfigs.Upsert(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, llmConfigResponse(result, true))
}

// handleGetEnterpriseLLMConfig returns an enterprise's LLM config.
// GET /v1/enterprises/{id}/llm-config
func (s *apiServer) handleGetEnterpriseLLMConfig(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	enterpriseID := extractEnterpriseLLMConfigID(r.URL.Path)
	if enterpriseID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "enterprise_id is required")
		return
	}

	if !s.requireEnterpriseAdmin(w, r, enterpriseID) {
		return
	}

	cfg, err := s.llmConfigs.GetByOwner(r.Context(), model.LLMConfigOwnerEnterprise, enterpriseID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "no LLM configuration found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, llmConfigResponse(cfg, true))
}

// handleDeleteEnterpriseLLMConfig deletes an enterprise's LLM config.
// DELETE /v1/enterprises/{id}/llm-config
func (s *apiServer) handleDeleteEnterpriseLLMConfig(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	enterpriseID := extractEnterpriseLLMConfigID(r.URL.Path)
	if enterpriseID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "enterprise_id is required")
		return
	}

	if !s.requireEnterpriseAdmin(w, r, enterpriseID) {
		return
	}

	cfg, err := s.llmConfigs.GetByOwner(r.Context(), model.LLMConfigOwnerEnterprise, enterpriseID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "no LLM configuration found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	_ = s.vault.Delete(r.Context(), cfg.VaultPath())

	if err := s.llmConfigs.Delete(r.Context(), cfg.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// requireEnterpriseAdmin checks that the calling user has admin or owner role
// in the specified enterprise. Returns false and writes an error if not.
func (s *apiServer) requireEnterpriseAdmin(w http.ResponseWriter, r *http.Request, enterpriseID string) bool {
	if s.users == nil {
		return true // auth disabled (dev mode)
	}

	userID, callerEnterpriseID, _ := s.requireAuth(w, r)

	// Check enterprise membership.
	if callerEnterpriseID != enterpriseID {
		writeError(w, http.StatusForbidden, "forbidden", "not a member of this enterprise")
		return false
	}

	user, err := s.users.Get(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", "user not found")
		return false
	}

	if user.Role != model.UserRoleOwner && user.Role != model.UserRoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden", "admin or owner role required")
		return false
	}

	return true
}

// llmConfigResponse builds the API response for an LLM config.
// Never includes the actual API key — just whether one is configured.
func llmConfigResponse(cfg model.LLMConfig, apiKeyConfigured bool) map[string]any {
	return map[string]any{
		"id":                 cfg.ID,
		"owner_type":         cfg.OwnerType,
		"owner_id":           cfg.OwnerID,
		"provider":           cfg.Provider,
		"model_research":     cfg.ModelResearch,
		"model_synthesis":    cfg.ModelSynthesis,
		"api_key_configured": apiKeyConfigured,
		"created_at":         cfg.CreatedAt,
		"updated_at":         cfg.UpdatedAt,
	}
}

// extractEnterpriseLLMConfigID extracts the enterprise ID from paths like
// /v1/enterprises/{id}/llm-config
func extractEnterpriseLLMConfigID(path string) string {
	const prefix = "/v1/enterprises/"
	const suffix = "/llm-config"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}
	id := path[len(prefix) : len(path)-len(suffix)]
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}
