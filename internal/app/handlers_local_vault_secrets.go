package app

import (
	"errors"
	"net/http"
	"strings"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/vault"
)

// agentCredentialVaultPath is the canonical vault path scheme for
// per-agent OAuth/credential envelopes. The HTTP path uses the
// stable URL word `credentials`; the vault stays on `oauth` so
// existing `aileron vault list`-style tooling continues to surface
// entries by their established key. Plan-pinned (ADR-0025 path
// scheme: `agents/<name>/<purpose>`).
func agentCredentialVaultPath(name string) string {
	return "agents/" + name + "/oauth"
}

// agentNameFromVaultPath is the inverse of agentCredentialVaultPath:
// it extracts the agent name from an `agents/<name>/oauth` vault path.
// It returns false for any path that does not match the scheme so the
// list filter ignores non-agent entries (secrets, bindings, etc.).
func agentNameFromVaultPath(path string) (string, bool) {
	const prefix = "agents/"
	const suffix = "/oauth"
	// Guard the length before slicing: "agents/oauth" passes both
	// HasPrefix and HasSuffix but the prefix and suffix overlap, so
	// path[len(prefix):len(path)-len(suffix)] would slice out of range.
	if len(path) < len(prefix)+len(suffix) ||
		!strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	name := path[len(prefix) : len(path)-len(suffix)]
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// GetAgentCredentials returns the per-agent credential envelope for
// the launcher's Render lifecycle. Per ADR-0011/0012 the daemon owns
// the unlocked vault; the launcher never opens the file itself.
//
// Error envelopes are named (`vault_not_found`, `vault_locked`) so
// the launcher discriminates by code rather than status alone — a
// future status-code change would not silently re-route the
// fallthrough-to-in-container-login path documented in ADR-0025.
func (s *apiServer) GetAgentCredentials(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "agent name is required")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		return
	}

	secret, err := s.vault.Get(r.Context(), agentCredentialVaultPath(name))
	if vault.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "vault_not_found",
			"no credential entry for agent "+name)
		return
	}
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before reading agent credentials")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_get_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, agentCredentialsResponse(secret))
}

// PutAgentCredentials accepts a credential envelope from the
// launcher's Capture pass (on clean container exit) or from a
// pre-launch refresh hook (Codex's PreLaunchRefresh rotates the
// token against the vendor's auth server and persists the new
// bundle before starting the container per AE6).
func (s *apiServer) PutAgentCredentials(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "agent name is required")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		return
	}

	var req api.AgentCredentials
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(req.Value) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"value is required and must be non-empty")
		return
	}

	meta := agentCredentialsMetadataFromRequest(req.Metadata)
	err := s.vault.Put(r.Context(), agentCredentialVaultPath(name), req.Value, meta)
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before writing agent credentials")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_put_failed", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// DeleteAgentCredentials removes the per-agent credential envelope at
// `agents/<name>/oauth`. This is the operator-facing recovery path
// behind `aileron vault delete`: dropping the entry makes the next
// launch fall through to the in-container login bootstrap per ADR-0025.
//
// The store-layer Delete is idempotent across every implementation
// (it returns nil whether or not the path existed), so this handler
// performs an explicit existence check via Get before deleting. That
// is the only way to honor the 404-on-miss contract while reusing the
// existing store primitive. A locked vault makes Get return
// ErrCredentialUnavailable, which yields 423 before any delete.
func (s *apiServer) DeleteAgentCredentials(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "agent name is required")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		return
	}

	path := agentCredentialVaultPath(name)
	_, err := s.vault.Get(r.Context(), path)
	if vault.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "vault_not_found",
			"no credential entry for agent "+name)
		return
	}
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before deleting agent credentials")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_get_failed", err.Error())
		return
	}

	if err := s.vault.Delete(r.Context(), path); err != nil {
		if errors.Is(err, vault.ErrCredentialUnavailable) {
			writeError(w, http.StatusLocked, "vault_locked",
				"unlock the vault before deleting agent credentials")
			return
		}
		writeError(w, http.StatusInternalServerError, "vault_delete_failed", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ListAgentCredentials returns the agent names that have a credential
// envelope stored, with plaintext metadata only — never the credential
// value (ADR-0011). It works on a locked vault because List never
// decrypts a stored value (see vault.Vault.List).
func (s *apiServer) ListAgentCredentials(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		return
	}

	entries, err := s.vault.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_list_failed", err.Error())
		return
	}

	list := api.AgentCredentialsList{Agents: []api.AgentCredentialSummary{}}
	for _, e := range entries {
		name, ok := agentNameFromVaultPath(e.Path)
		if !ok {
			continue
		}
		list.Agents = append(list.Agents, api.AgentCredentialSummary{
			Name:     name,
			Metadata: agentMetadataToWire(e.Metadata),
		})
	}

	writeJSON(w, http.StatusOK, list)
}

// agentCredentialsResponse builds the JSON envelope returned by GET.
// oapi-codegen's []byte field is base64-encoded by encoding/json
// (the standard library default for byte slices), so the wire
// shape matches the spec's `format: byte` declaration.
func agentCredentialsResponse(s vault.Secret) api.AgentCredentials {
	return api.AgentCredentials{
		Value:    s.Value,
		Metadata: agentMetadataToWire(s.Metadata),
	}
}

// agentMetadataToWire maps a vault.Metadata into the wire-shaped
// optional-pointer struct shared by the GET envelope and the list
// summary. Returns nil when every field is empty so a credential with
// no metadata serializes without an empty `metadata` object.
func agentMetadataToWire(meta vault.Metadata) *api.AgentCredentialsMetadata {
	if meta.Type == "" && meta.Environment == "" && len(meta.Labels) == 0 {
		return nil
	}
	respMeta := api.AgentCredentialsMetadata{}
	if meta.Type != "" {
		t := meta.Type
		respMeta.Type = &t
	}
	if meta.Environment != "" {
		e := meta.Environment
		respMeta.Environment = &e
	}
	if len(meta.Labels) > 0 {
		labels := make(map[string]string, len(meta.Labels))
		for k, v := range meta.Labels {
			labels[k] = v
		}
		respMeta.Labels = &labels
	}
	return &respMeta
}

// agentCredentialsMetadataFromRequest is the inverse of
// agentCredentialsResponse: maps the wire-shaped optional pointers
// into the vault's plain Metadata struct. A nil request metadata
// stores a zero-value Metadata; explicit empty fields stay empty.
func agentCredentialsMetadataFromRequest(req *api.AgentCredentialsMetadata) vault.Metadata {
	if req == nil {
		return vault.Metadata{}
	}
	meta := vault.Metadata{}
	if req.Type != nil {
		meta.Type = *req.Type
	}
	if req.Environment != nil {
		meta.Environment = *req.Environment
	}
	if req.Labels != nil {
		meta.Labels = *req.Labels
	}
	return meta
}
