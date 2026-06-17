package app

import (
	"encoding/json"
	"net/http"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

const saltLength = 16

func (s *apiServer) SetPassphrase(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.userKeyMaterials == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	var req api.SetPassphraseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	if len(req.Salt) != saltLength {
		writeError(w, http.StatusBadRequest, "invalid_salt", "salt must be exactly 16 bytes")
		return
	}
	if len(req.KekVerification) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_verification", "kek_verification must not be empty")
		return
	}

	userID := claims.Subject
	now := time.Now().UTC()
	material := model.UserKeyMaterial{
		UserID:          userID,
		Salt:            req.Salt,
		KEKVerification: req.KekVerification,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	// Check if key material already exists (rotation case).
	ctx := r.Context()
	_, err := s.userKeyMaterials.Get(ctx, userID)
	if err != nil {
		if isNotFound(err) {
			// First time — create.
			if err := s.userKeyMaterials.Create(ctx, material); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to store key material")
				return
			}
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to check key material")
			return
		}
	} else {
		// Rotation — update.
		if err := s.userKeyMaterials.Update(ctx, material); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to update key material")
			return
		}
	}

	salt := req.Salt
	writeJSON(w, http.StatusOK, api.PassphraseSaltResponse{
		HasPassphrase: true,
		Salt:          &salt,
	})
}

func (s *apiServer) GetPassphraseVerification(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.userKeyMaterials == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	userID := claims.Subject
	material, err := s.userKeyMaterials.Get(r.Context(), userID)
	if err != nil {
		if isNotFound(err) {
			writeJSON(w, http.StatusOK, api.PassphraseVerificationResponse{HasPassphrase: false})
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to retrieve key material")
		return
	}

	verification := material.KEKVerification
	writeJSON(w, http.StatusOK, api.PassphraseVerificationResponse{
		HasPassphrase:   true,
		KekVerification: &verification,
	})
}

func (s *apiServer) GetPassphraseSalt(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.userKeyMaterials == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	userID := claims.Subject
	material, err := s.userKeyMaterials.Get(r.Context(), userID)
	if err != nil {
		if isNotFound(err) {
			writeJSON(w, http.StatusOK, api.PassphraseSaltResponse{HasPassphrase: false})
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to retrieve key material")
		return
	}

	salt := material.Salt
	writeJSON(w, http.StatusOK, api.PassphraseSaltResponse{
		HasPassphrase: true,
		Salt:          &salt,
	})
}

func (s *apiServer) UnlockVault(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.userKeyMaterials == nil || s.kekSessionCache == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	var req api.UnlockVaultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	kek := []byte(req.Kek)
	if len(kek) != crypto.KEKLen {
		writeError(w, http.StatusBadRequest, "invalid_kek", "KEK must be exactly 32 bytes")
		return
	}

	userID := claims.Subject
	material, err := s.userKeyMaterials.Get(r.Context(), userID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusBadRequest, "passphrase_required", "no passphrase set — call POST /v1/users/me/passphrase first")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to retrieve key material")
		return
	}

	// Verify the KEK by attempting to decrypt the verification blob.
	_, err = crypto.Decrypt(material.KEKVerification, kek)
	if err != nil {
		writeError(w, http.StatusForbidden, "incorrect_passphrase", "KEK verification failed — wrong passphrase")
		return
	}

	s.kekSessionCache.Set(userID, kek)
	zeroBytes(kek)

	expiresAt := s.kekSessionCache.ExpiresAt(userID)
	writeJSON(w, http.StatusOK, api.VaultStatusResponse{
		Locked:        false,
		HasPassphrase: true,
		ExpiresAt:     expiresAt,
	})
}

func (s *apiServer) LockVault(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.kekSessionCache == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	s.kekSessionCache.Clear(claims.Subject)
	writeJSON(w, http.StatusOK, api.VaultStatusResponse{
		Locked:        true,
		HasPassphrase: true,
	})
}

func (s *apiServer) GetVaultStatus(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.userKeyMaterials == nil || s.kekSessionCache == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	userID := claims.Subject
	hasPassphrase := true
	if _, err := s.userKeyMaterials.Get(r.Context(), userID); err != nil {
		hasPassphrase = false
	}

	expiresAt := s.kekSessionCache.ExpiresAt(userID)
	locked := expiresAt == nil

	writeJSON(w, http.StatusOK, api.VaultStatusResponse{
		Locked:        locked,
		HasPassphrase: hasPassphrase,
		ExpiresAt:     expiresAt,
	})
}

// requireKEK retrieves the user's cached KEK. Returns 423 if vault is locked,
// 400 if no passphrase is set. The caller must zero the returned KEK when done.
func (s *apiServer) requireKEK(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return nil, false
	}

	if s.userKeyMaterials == nil || s.kekSessionCache == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return nil, false
	}

	if _, err := s.userKeyMaterials.Get(r.Context(), userID); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusBadRequest, "passphrase_required", "set a vault passphrase before connecting accounts")
			return nil, false
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to check key material")
		return nil, false
	}

	kek := s.kekSessionCache.Get(userID)
	if kek == nil {
		writeError(w, 423, "vault_locked", "vault is locked — unlock with POST /v1/users/me/passphrase/unlock")
		return nil, false
	}
	return kek, true
}

// getUserKEK returns the user's cached KEK for non-HTTP paths (e.g. Slack webhooks).
// Returns nil if vault is locked.
func (s *apiServer) getUserKEK(userID string) []byte {
	if s.kekSessionCache == nil {
		return nil
	}
	return s.kekSessionCache.Get(userID)
}

// userVault returns a vault scoped to the user's KEK for decryption.
// Returns a UserScopedVault if KEK is cached, or a nil-KEK vault that
// will fail on any operation (forcing callers to handle the locked state).
func (s *apiServer) userVault(userID string) vault.Vault {
	// Tier 1: KEK in session cache.
	if kek := s.getUserKEK(userID); kek != nil {
		return vault.NewUserScopedVault(s.vault, kek)
	}
	// No KEK available — UserScopedVault will return a clear error.
	return vault.NewUserScopedVault(s.vault, nil)
}

// zeroBytes overwrites a byte slice with zeros.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
