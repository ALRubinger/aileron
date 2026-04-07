package app

import (
	"encoding/json"
	"net/http"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
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

// zeroBytes overwrites a byte slice with zeros.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
