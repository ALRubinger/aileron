package app

import (
	"errors"
	"net/http"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/vault"
)

// GetLocalVaultStatus reports the lifecycle state of the local file
// vault that backs `aileron launch`. Single-user, no auth — the local
// vault belongs to the operator running the daemon.
//
// The webapp polls this endpoint to drive the passphrase modal: on
// `locked` the modal opens; on `unlocked` it closes. `missing` means
// no vault file exists at the canonical path — the user must run a
// flow that creates one (today, the stderr fallback under launch),
// since the webapp does not yet handle first-launch creation.
func (s *apiServer) GetLocalVaultStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.localVaultStatusResponse())
}

// UnlockLocalVault accepts a passphrase, derives the KEK, verifies it
// against the on-disk verification blob, and on success swaps the
// unlocked vault into the daemon's [vault.LockableVault] wrapper.
// Vault-needing endpoints stop returning 423 Locked.
//
// 401 means wrong passphrase (per [vault.ErrPassphraseRejected]).
// 404 means no vault file exists at the canonical path. 409 means the
// vault was already unlocked — surfaced as a successful state read so
// the webapp can recover from races (two tabs both submitting).
func (s *apiServer) UnlockLocalVault(w http.ResponseWriter, r *http.Request) {
	if s.lockableVault == nil || s.localVaultPath == "" {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a local file vault")
		return
	}

	var req api.LocalVaultUnlockRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Passphrase == "" {
		writeError(w, http.StatusBadRequest, "missing_passphrase",
			"passphrase is required")
		return
	}

	s.localVaultMu.Lock()
	defer s.localVaultMu.Unlock()

	if !s.vaultLocked {
		writeJSON(w, http.StatusConflict, s.localVaultStatusResponseLocked())
		return
	}

	state, err := vault.CheckState(s.localVaultPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_check_failed", err.Error())
		return
	}
	if state == vault.StateMissing {
		writeError(w, http.StatusNotFound, "no_local_vault_file",
			"no vault file at the canonical path; create one before unlocking")
		return
	}

	v, err := vault.Unlock(s.localVaultPath, req.Passphrase)
	if errors.Is(err, vault.ErrPassphraseRejected) {
		writeError(w, http.StatusUnauthorized, "wrong_passphrase",
			"passphrase did not unlock the vault")
		return
	}
	if errors.Is(err, vault.ErrVaultTampered) {
		writeError(w, http.StatusInternalServerError, "vault_tampered",
			"vault verification blob does not match expected plaintext; refusing to unlock")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_unlock_failed", err.Error())
		return
	}

	s.lockableVault.Set(v)
	s.vaultLocked = false
	s.log.Info("local vault unlocked via webapp")

	// Fire the vault-unlock callback so daemon-wide subsystems that
	// were waiting on credentials (Slack/Discord listener startup,
	// per ADR-0012 step 9B-2) can resolve tokens and come online.
	// The callback runs synchronously here; listener startup itself
	// is fire-and-forget inside the callback so this handler never
	// blocks on Slack's websocket handshake. A panicking callback
	// must not poison the unlock — caller might still want the
	// vault open.
	if s.onVaultUnlock != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Warn("vault-unlock callback panicked", "panic", r)
				}
			}()
			s.onVaultUnlock(v)
		}()
	}

	writeJSON(w, http.StatusOK, s.localVaultStatusResponseLocked())
}

// localVaultStatusResponse computes the status payload from the
// daemon's current state. Held under the localVaultMu read-side
// implicitly via vaultLocked's read; callers that need a coherent
// snapshot under contention should hold the lock themselves.
func (s *apiServer) localVaultStatusResponse() api.LocalVaultStatusResponse {
	if s.lockableVault == nil || s.localVaultPath == "" {
		// Daemon not running under launch with a local file vault —
		// surface as `unlocked` since the in-memory dev vault (or a
		// caller-supplied vault) is always usable. The webapp uses
		// `locked == false` to keep the modal closed.
		return api.LocalVaultStatusResponse{
			Locked: false,
			State:  api.LocalVaultStatusResponseStateUnlocked,
		}
	}
	return s.localVaultStatusResponseLocked()
}

// localVaultStatusResponseLocked is the variant called from inside
// the localVaultMu critical section in UnlockLocalVault, plus the
// non-locking computation in localVaultStatusResponse. Splitting the
// two avoids the lock-then-call pattern producing a stale
// state on concurrent unlocks.
func (s *apiServer) localVaultStatusResponseLocked() api.LocalVaultStatusResponse {
	if !s.vaultLocked {
		return api.LocalVaultStatusResponse{
			Locked: false,
			State:  api.LocalVaultStatusResponseStateUnlocked,
		}
	}
	state, err := vault.CheckState(s.localVaultPath)
	if err != nil || state == vault.StateMissing {
		return api.LocalVaultStatusResponse{
			Locked: true,
			State:  api.LocalVaultStatusResponseStateMissing,
		}
	}
	return api.LocalVaultStatusResponse{
		Locked: true,
		State:  api.LocalVaultStatusResponseStateLocked,
	}
}
