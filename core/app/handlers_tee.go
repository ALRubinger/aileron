package app

import (
	"crypto/rand"
	"net/http"
	"sync"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/enclave"
)

// teeState tracks attestation and session state for the host-side TEE flow.
type teeState struct {
	mu            sync.Mutex
	attested      bool
	nonce         []byte
	attestResp    enclave.AttestationResponse
	sessionActive bool
	sessionID     string
	sessionKey    []byte // ECDH-derived session key for KEK transit encryption
	expiresAt     time.Time
}

// getSessionKey returns a copy of the TEE session key, used to encrypt
// the KEK before transmitting it to the enclave.
func (s *apiServer) getSessionKey() []byte {
	if s.teeState == nil {
		return nil
	}
	s.teeState.mu.Lock()
	defer s.teeState.mu.Unlock()
	if s.teeState.sessionKey == nil {
		return nil
	}
	cpy := make([]byte, len(s.teeState.sessionKey))
	copy(cpy, s.teeState.sessionKey)
	return cpy
}

func (s *apiServer) GetTeeStatus(w http.ResponseWriter, _ *http.Request) {
	provider := api.None
	enabled := false

	if s.teeCfg != nil && s.teeCfg.TEEEnabled() {
		enabled = true
		switch s.teeCfg.Provider {
		case "local":
			provider = api.Local
		case "confidential-space":
			provider = api.ConfidentialSpace
		}
	}

	status := api.TeeStatus{
		Enabled:  enabled,
		Provider: provider,
	}

	if s.teeState != nil {
		s.teeState.mu.Lock()
		attested := s.teeState.attested
		sessionActive := s.teeState.sessionActive
		expiresAt := s.teeState.expiresAt
		s.teeState.mu.Unlock()

		status.Attested = &attested
		status.SessionActive = &sessionActive
		if sessionActive {
			status.SessionExpiresAt = &expiresAt
		}
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *apiServer) InitiateAttestation(w http.ResponseWriter, r *http.Request) {
	if s.enclaveClient == nil {
		writeError(w, http.StatusNotImplemented, "tee_disabled", "TEE is not enabled")
		return
	}

	var req api.TeeAttestationRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Generate nonce.
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		writeError(w, http.StatusInternalServerError, "nonce_error", "failed to generate nonce")
		return
	}

	audience := "aileron-enclave"
	if req.Audience != nil {
		audience = *req.Audience
	}

	attestResp, err := s.enclaveClient.Attest(r.Context(), enclave.AttestationRequest{
		Nonce:    nonce,
		Audience: audience,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "attestation_error", err.Error())
		return
	}

	// Store state for verification.
	s.teeState.mu.Lock()
	s.teeState.nonce = nonce
	s.teeState.attestResp = attestResp
	s.teeState.attested = true
	s.teeState.mu.Unlock()

	// []byte fields are automatically base64-encoded by json.Marshal.
	writeJSON(w, http.StatusOK, api.TeeAttestationResponse{
		Nonce:     nonce,
		Token:     attestResp.Token,
		PublicKey: attestResp.PublicKey,
	})
}

func (s *apiServer) EstablishTeeSession(w http.ResponseWriter, r *http.Request) {
	if s.enclaveClient == nil {
		writeError(w, http.StatusNotImplemented, "tee_disabled", "TEE is not enabled")
		return
	}

	var req api.TeeSessionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Verify attestation.
	claims, err := s.enclaveVerifier.Verify(r.Context(), req.Token, req.Nonce)
	if err != nil {
		writeError(w, http.StatusBadRequest, "attestation_failed", err.Error())
		return
	}

	// Generate host's ephemeral ECDH key pair.
	hostKey, err := crypto.GenerateKeyPair()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key_error", "failed to generate host key pair")
		return
	}

	// Establish session with enclave using host's public key.
	sessResp, err := s.enclaveClient.EstablishSession(r.Context(), enclave.SessionRequest{
		PublicKey: crypto.MarshalPublicKey(hostKey.PublicKey()),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_error", err.Error())
		return
	}

	// Derive the same shared secret on the host side using the enclave's
	// public key (received during attestation).
	s.teeState.mu.Lock()
	enclavePubKeyBytes := s.teeState.attestResp.PublicKey
	s.teeState.mu.Unlock()

	enclavePub, err := crypto.UnmarshalPublicKey(enclavePubKeyBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key_error", "failed to parse enclave public key")
		return
	}

	sessionKey, err := crypto.DeriveSharedSecret(hostKey, enclavePub)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key_error", "ECDH failed")
		return
	}

	// Parse expiry.
	expiresAt, _ := time.Parse(time.RFC3339, sessResp.ExpiresAt)

	// Update state — store session key for KEK transit encryption.
	s.teeState.mu.Lock()
	zeroBytes(s.teeState.sessionKey)
	s.teeState.sessionActive = true
	s.teeState.sessionID = sessResp.SessionID
	s.teeState.sessionKey = sessionKey
	s.teeState.expiresAt = expiresAt
	s.teeState.mu.Unlock()

	claimsMap := map[string]interface{}{
		"image_digest": claims.ImageDigest,
		"project_id":   claims.ProjectID,
	}

	writeJSON(w, http.StatusOK, api.TeeSessionResponse{
		Verified:  true,
		SessionId: sessResp.SessionID,
		ExpiresAt: &expiresAt,
		Claims:    &claimsMap,
	})
}
