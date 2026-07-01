package app

import (
	"net/http"
	"strings"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/binding"
)

// Control-plane namespace prefixes. These two namespaces are tenant-keyed
// records the cloud-shaped control plane owns (a per-user connected OAuth
// account and a per-owner LLM config), not local single-user `aileron
// launch` credentials. They are excluded from the default union view so
// `aileron vault list` surfaces only the locally-owned namespaces; an
// explicit opt-in (`include_control_plane=true`) includes them.
const (
	connectedAccountsVaultPrefix = "connected-accounts/"
	llmConfigVaultPrefix         = "llm-config/"
	secretVaultPrefix            = "secret/"
)

// Vault entry scope labels surfaced in the union view's `scope` field.
// These mirror the documented values in the OpenAPI `VaultEntry.scope`
// description; the spec models the field as a free-form string (not an
// enum) so these literals do not collide with the same words in other
// enums and force a global enum-constant rename in the generated code.
const (
	vaultScopeAgent            = "agent"
	vaultScopeUser             = "user"
	vaultScopeBinding          = "binding"
	vaultScopeConnectedAccount = "connected-account"
	vaultScopeLlmConfig        = "llm-config"
	vaultScopeSecret           = "secret"
	vaultScopeOther            = "other"
)

// classifyVaultPath maps a raw vault path to its VaultEntry scope label and
// reports whether it belongs to a control-plane namespace.
//
// The locally-owned namespaces (`agents/`, `user/`, `secret/`, and
// capability bindings — which include connector credentials like
// `connectors/<provider>/default` and OAuth bindings like
// `oauth2/google/...`) are always part of the union. The two tenant-keyed
// control-plane namespaces (`connected-accounts/`, `llm-config/`) are
// classified too but flagged so the caller can exclude them by default.
//
// Classification order is load-bearing: the prefixed namespaces are
// checked before the generic three-segment binding shape, because an
// `agents/<name>/<purpose>` path also matches the binding name grammar
// (`<kind>/<service>/<identity>`) and must classify as `agent`, not
// `binding`.
//
// Every path receives a scope. A path matching none of the known
// namespaces classifies as `other` so the union never silently hides an
// entry — surfacing unknown entries is the whole point of the union view
// (#1402).
func classifyVaultPath(path string) (scope string, controlPlane bool) {
	switch {
	case strings.HasPrefix(path, connectedAccountsVaultPrefix):
		return vaultScopeConnectedAccount, true
	case strings.HasPrefix(path, llmConfigVaultPrefix):
		return vaultScopeLlmConfig, true
	case strings.HasPrefix(path, secretVaultPrefix):
		// Secrets set via `aileron secret set` are locally-owned, so they
		// are part of the default union (controlPlane=false). This is
		// checked before the binding grammar below because a slash-bearing
		// secret path could otherwise match the three-segment binding
		// shape.
		return vaultScopeSecret, false
	}
	if _, _, ok := agentNameAndPurposeFromVaultPath(path); ok {
		return vaultScopeAgent, false
	}
	if _, ok := userServiceFromVaultPath(path); ok {
		return vaultScopeUser, false
	}
	if binding.IsBindingPath(path) {
		return vaultScopeBinding, false
	}
	return vaultScopeOther, false
}

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
		scope, controlPlane := classifyVaultPath(e.Path)
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
