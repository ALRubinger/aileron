package app

import (
	"net/http"
	"os"
	"path/filepath"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/version"
)

// GetStatus returns the daemon's runtime snapshot — version, commit,
// vault state, action / connector / binding counts, and the embedded
// webapp URL when running under `aileron launch`. Read-only; safe to
// poll. Surfaced through the `aileron status` CLI and (per ADR-0009)
// the `/aileron status` agent slash-command.
//
// Distinct from `GET /v1/health` (a liveness probe). This is the
// operator's "what's installed and what's the daemon doing right now"
// snapshot per ADR-0004's operational-primitives requirement.
func (s *apiServer) GetStatus(w http.ResponseWriter, r *http.Request) {
	resp := api.StatusResponse{
		Version:        version.Version,
		ActionCount:    s.actionCount(),
		ConnectorCount: s.connectorCount(),
		BindingCount:   s.bindingCount(r),
		VaultState:     api.StatusResponseVaultState(s.vaultState()),
	}
	if version.Commit != "" {
		commit := version.Commit
		resp.Commit = &commit
	}
	if s.webappURL != "" {
		url := s.webappURL
		resp.GatewayUrl = &url
	}
	if sid := os.Getenv("AILERON_SESSION_ID"); sid != "" {
		resp.SessionId = &sid
	}
	writeJSON(w, http.StatusOK, resp)
}

// actionCount returns the number of installed action manifests in
// `~/.aileron/actions/`. Zero when the store is unconfigured (e.g.
// dev-mode harness with no actions wired).
func (s *apiServer) actionCount() int {
	if s.actions == nil {
		return 0
	}
	return len(s.actions.List())
}

// connectorCount returns the number of installed connector tarballs
// in the content-addressed store. Zero when the installer is
// unconfigured.
func (s *apiServer) connectorCount() int {
	if s.installer == nil || s.installer.Store == nil {
		return 0
	}
	return s.installer.Store.Count()
}

// bindingCount returns the number of credential bindings in the
// vault. Vault-metadata-only — no decryption required, so this works
// even when the vault is locked. Errors return 0 (the count is a
// best-effort surface; we don't want a misbehaving vault to break
// `aileron status`).
func (s *apiServer) bindingCount(r *http.Request) int {
	if s.bindings == nil {
		return 0
	}
	bs, err := s.bindings.List(r.Context())
	if err != nil {
		return 0
	}
	return len(bs)
}

// vaultState classifies the daemon's vault posture for the operator.
// Mirrors the `StatusResponseVaultState` enum in the OpenAPI spec —
// keep these in sync if the enum grows.
func (s *apiServer) vaultState() string {
	if s.vaultLocked {
		return "locked"
	}
	if s.vault == nil {
		return "missing"
	}
	// Two unlocked cases:
	//
	//  1. The persistent file vault (the production path under
	//     `aileron launch` and under the standalone server when a
	//     vault file exists at the canonical path) — distinguishable
	//     by the file existing on disk at DefaultVaultPath().
	//
	//  2. The in-memory dev-mode vault (random per-process KEK,
	//     created when no file exists). Surfaced as `dev` so the
	//     operator can tell the daemon isn't using their persistent
	//     credentials.
	if state, err := vault.CheckState(defaultVaultPath()); err == nil && state != vault.StateMissing {
		return "unlocked"
	}
	return "dev"
}

// defaultVaultPath returns the canonical persistent-vault path. Mirror
// of `launch.DefaultVaultPath` — duplicated here because importing
// `internal/launch` from `internal/app` would create an import cycle
// (launch's gateway.go already imports app). Two callers compute the
// same path in two places; the path itself is stable across both.
func defaultVaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".aileron", "secrets.json")
	}
	return filepath.Join(home, ".aileron", "secrets.json")
}
