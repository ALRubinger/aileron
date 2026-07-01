package app

import (
	"net/http"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/vaultscope"
)

// ListVaultEntries returns the union of every entry in the local vault,
// each classified by namespace scope, with plaintext metadata only — never
// the credential value (ADR-0011). It walks vault.List() exactly once, so
// it works on a locked vault (List never decrypts a stored value).
//
// The two tenant-keyed control-plane namespaces (`connected-accounts/`,
// `llm-config/`) are excluded from the default view; passing
// `include_control_plane=true` includes them.
func (s *apiServer) ListVaultEntries(w http.ResponseWriter, r *http.Request, params api.ListVaultEntriesParams) {
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

	includeControlPlane := params.IncludeControlPlane != nil && *params.IncludeControlPlane

	list := api.VaultEntriesList{Entries: []api.VaultEntry{}}
	for _, e := range entries {
		scope, controlPlane := vaultscope.Classify(e.Path)
		if controlPlane && !includeControlPlane {
			continue
		}
		list.Entries = append(list.Entries, api.VaultEntry{
			Path:     e.Path,
			Scope:    scope,
			Metadata: agentMetadataToWire(e.Metadata),
		})
	}

	writeJSON(w, http.StatusOK, list)
}
