package app

import (
	"encoding/json"
	"net/http"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/core/model"
)

// kekVerificationConstant is the known plaintext encrypted with the KEK.
// On passphrase verification, we re-derive the KEK and attempt to decrypt
// this value. If decryption succeeds and the result matches, the passphrase
// is correct. The constant itself is not secret — it's a fixed sentinel.
var kekVerificationConstant = []byte("aileron-kek-verification-ok")

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

	if len(req.Passphrase) < 8 {
		writeError(w, http.StatusBadRequest, "weak_passphrase", "passphrase must be at least 8 characters")
		return
	}

	userID := claims.Subject

	// Generate salt and derive KEK.
	salt, err := crypto.GenerateSalt()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to generate salt")
		return
	}

	kek, err := crypto.DeriveKEK([]byte(req.Passphrase), salt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to derive KEK")
		return
	}
	defer zeroBytes(kek)

	// Encrypt the verification constant with the KEK.
	verification, err := crypto.Encrypt(kekVerificationConstant, kek)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to create verification blob")
		return
	}

	now := time.Now().UTC()
	material := model.UserKeyMaterial{
		UserID:          userID,
		Salt:            salt,
		KEKVerification: verification,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	// Check if key material already exists (rotation case).
	ctx := r.Context()
	_, err = s.userKeyMaterials.Get(ctx, userID)
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

	writeJSON(w, http.StatusOK, api.PassphraseSaltResponse{
		HasPassphrase: true,
		Salt:          &salt,
	})
}

func (s *apiServer) VerifyPassphrase(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.userKeyMaterials == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	var req api.VerifyPassphraseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	userID := claims.Subject
	ctx := r.Context()

	material, err := s.userKeyMaterials.Get(ctx, userID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "no_passphrase", "no passphrase set for this user")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to retrieve key material")
		return
	}

	// Re-derive KEK from passphrase + stored salt.
	kek, err := crypto.DeriveKEK([]byte(req.Passphrase), material.Salt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to derive KEK")
		return
	}
	defer zeroBytes(kek)

	// Attempt to decrypt the verification blob.
	plaintext, err := crypto.Decrypt(material.KEKVerification, kek)
	if err != nil {
		// Decryption failed — wrong passphrase.
		writeJSON(w, http.StatusOK, api.VerifyPassphraseResponse{Valid: false})
		return
	}

	// Check that decrypted value matches the expected constant.
	valid := string(plaintext) == string(kekVerificationConstant)
	resp := api.VerifyPassphraseResponse{Valid: valid}
	if valid {
		salt := material.Salt
		resp.Salt = &salt
	}
	writeJSON(w, http.StatusOK, resp)
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

// zeroBytes overwrites a byte slice with zeros.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
