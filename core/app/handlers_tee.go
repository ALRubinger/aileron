package app

import (
	"context"
	"crypto/rand"
	"net/http"
	"sync"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
)

// teeState tracks attestation and session state for the host-side TEE flow.
type teeState struct {
	mu           sync.Mutex
	attested     bool
	nonce        []byte
	attestResp   enclave.AttestationResponse
	userSessions map[string]time.Time // userID -> session expiry
}

func newTeeState() *teeState {
	return &teeState{
		userSessions: make(map[string]time.Time),
	}
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
		s.teeState.mu.Unlock()

		status.Attested = &attested
		// Session is per-user; report aggregate active state.
		sessionActive := false
		status.SessionActive = &sessionActive
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

	// Store state for the session establishment step.
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

	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	var req api.TeeSessionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Forward the client's public key to the enclave for ECDH key exchange.
	// The server never generates its own key pair — it's a pass-through.
	sessResp, err := s.enclaveClient.EstablishSession(r.Context(), enclave.SessionRequest{
		PublicKey: req.ClientPublicKey,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_error", err.Error())
		return
	}

	// Forward the encrypted KEK blob to the enclave. The server cannot
	// decrypt this — it doesn't have the ECDH private key.
	userID := claims.Subject
	_, transmitErr := s.enclaveClient.TransmitKEK(r.Context(), enclave.TransmitKEKRequest{
		UserID:       userID,
		EncryptedKEK: req.EncryptedKek,
	})
	if transmitErr != nil {
		writeError(w, http.StatusInternalServerError, "kek_error", "failed to transmit KEK to enclave: "+transmitErr.Error())
		return
	}

	// Parse expiry.
	expiresAt, _ := time.Parse(time.RFC3339, sessResp.ExpiresAt)

	// Track session existence per user (no session key stored).
	s.teeState.mu.Lock()
	s.teeState.userSessions[userID] = expiresAt
	s.teeState.mu.Unlock()

	// Auto-escrow active credentials now that the enclave holds the KEK.
	escrowed := s.autoEscrowCredentials(r.Context(), userID)

	resp := api.TeeSessionResponse{
		SessionId: sessResp.SessionID,
		ExpiresAt: &expiresAt,
	}
	if escrowed > 0 {
		resp.EscrowedCount = &escrowed
	}

	writeJSON(w, http.StatusOK, resp)
}

// autoEscrowCredentials escrows all active connected account credentials into
// the TEE for autonomous execution. Returns the number of successfully
// escrowed credentials.
func (s *apiServer) autoEscrowCredentials(ctx context.Context, userID string) int {
	if s.connectedAccounts == nil {
		return 0
	}

	accounts, err := s.connectedAccounts.List(ctx, store.ConnectedAccountFilter{UserID: userID})
	if err != nil {
		return 0
	}

	escrowed := 0
	for _, acc := range accounts {
		if acc.Status != model.ConnectedAccountStatusActive {
			continue
		}

		secret, err := s.vault.Get(ctx, acc.VaultPath())
		if err != nil {
			continue
		}

		if !vault.IsEncrypted(secret.Metadata) {
			continue
		}

		expiresAt := time.Now().Add(s.escrowTTL)
		grantID := "auto-escrow-" + acc.ID

		resp, err := s.enclaveClient.EscrowStore(ctx, enclave.EscrowStoreRequest{
			UserID:              userID,
			GrantID:             grantID,
			EncryptedCredential: secret.Value,
			CredentialType:      secret.Metadata.Type,
			ExpiresAt:           expiresAt.Format(time.RFC3339),
			ActionTypes:         actionTypesForProvider(acc.Provider),
		})
		if err != nil {
			continue
		}

		s.escrowIndex.Store(acc.VaultPath(), resp.EscrowID)
		escrowed++
	}
	return escrowed
}

// actionTypesForProvider returns the action types allowed for a connected
// account provider.
func actionTypesForProvider(provider model.ConnectedAccountProvider) []string {
	switch provider {
	case model.ConnectedAccountProviderGmail:
		return []string{"email.send"}
	case model.ConnectedAccountProviderGoogleCalendar:
		return []string{"calendar.create"}
	case model.ConnectedAccountProviderOutlook:
		return []string{"email.send"}
	case model.ConnectedAccountProviderMicrosoftCalendar:
		return []string{"calendar.create"}
	default:
		return nil
	}
}
