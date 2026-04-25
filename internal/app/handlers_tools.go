package app

import (
	"encoding/json"
	"net/http"

	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store"
)

// handleListTools returns the available tool definitions for the authenticated
// user based on their connected accounts and registered source connectors.
func (s *apiServer) handleListTools(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	if s.sourceRegistry == nil {
		writeJSON(w, http.StatusOK, map[string]any{"tools": []source.ToolDefinition{}})
		return
	}

	// Get the user's connected accounts to determine which source connectors
	// they have access to.
	accounts, err := s.connectedAccounts.List(r.Context(), store.ConnectedAccountFilter{UserID: userID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Collect tools from source connectors that match connected accounts.
	connectedProviders := make(map[string]bool)
	for _, acct := range accounts {
		if acct.Status == model.ConnectedAccountStatusActive {
			connectedProviders[string(acct.Provider)] = true
		}
	}

	var tools []source.ToolDefinition
	for _, provider := range s.sourceRegistry.Providers() {
		if connectedProviders[provider] {
			tools = append(tools, s.sourceRegistry.ToolsForProvider(provider)...)
		}
	}

	if tools == nil {
		tools = []source.ToolDefinition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

// toolExecuteRequest is the request body for POST /v1/tools/execute.
type toolExecuteRequest struct {
	Tool   string         `json:"tool"`
	Params map[string]any `json:"params"`
}

// handleExecuteTool executes a source connector tool using the authenticated
// user's credentials.
func (s *apiServer) handleExecuteTool(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	if s.sourceRegistry == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "source connectors not configured")
		return
	}

	var req toolExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "tool field is required")
		return
	}

	// Resolve the tool to a provider.
	provider, err := s.sourceRegistry.ResolveToolProvider(req.Tool)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown_tool", err.Error())
		return
	}

	// Get the user's connected account for this provider.
	providerEnum := model.ConnectedAccountProvider(provider)
	accounts, err := s.connectedAccounts.List(r.Context(), store.ConnectedAccountFilter{
		UserID:   userID,
		Provider: &providerEnum,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if len(accounts) == 0 {
		writeError(w, http.StatusBadRequest, "not_connected", "no connected "+provider+" account")
		return
	}

	acct := accounts[0]
	if acct.Status != model.ConnectedAccountStatusActive {
		writeError(w, http.StatusBadRequest, "account_inactive", provider+" account is not active")
		return
	}

	vaultPath := acct.VaultPath()

	// Enclave path: if the credential is escrowed in the TEE, execute the
	// tool inside the enclave so plaintext never reaches the host.
	if s.enclaveClient != nil {
		if escrowIDVal, ok := s.escrowIndex.Load(vaultPath); ok {
			resp, err := s.enclaveClient.SourceExecute(r.Context(), enclave.SourceExecuteRequest{
				EscrowID: escrowIDVal.(string),
				Tool:     req.Tool,
				Params:   req.Params,
			})
			if err != nil {
				s.log.Error("enclave tool execution failed", "tool", req.Tool, "error", err)
				writeError(w, http.StatusInternalServerError, "execution_error", err.Error())
				return
			}
			if resp.Error != "" {
				writeError(w, http.StatusInternalServerError, "execution_error", resp.Error)
				return
			}
			if resp.Result == nil {
				writeJSON(w, http.StatusOK, map[string]any{})
				return
			}
			var result map[string]any
			if err := json.Unmarshal(resp.Result, &result); err != nil {
				writeError(w, http.StatusInternalServerError, "execution_error", "failed to parse tool result")
				return
			}
			writeJSON(w, http.StatusOK, result)
			return
		}

		// Enclave is active but no escrow entry — vault is locked.
		writeError(w, http.StatusForbidden, "vault_locked", "credentials are not available; unlock your vault to escrow them into the enclave")
		return
	}

	// Non-enclave path: read credentials from the vault on the host.
	secret, err := s.vault.Get(r.Context(), vaultPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_error", "failed to retrieve credentials")
		return
	}

	result, err := s.sourceRegistry.ExecuteTool(r.Context(), req.Tool, req.Params, secret.Value)
	if err != nil {
		s.log.Error("tool execution failed", "tool", req.Tool, "error", err)
		writeError(w, http.StatusInternalServerError, "execution_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}
