package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
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

const jwksCacheTTL = 1 * time.Hour

// googleDiscoveryURL is the OIDC discovery endpoint for Google account
// tokens (which is what Confidential Space attestation tokens are issued
// under). It is a var (not const) so that tests can override it.
var googleDiscoveryURL = "https://accounts.google.com/.well-known/openid-configuration"

// setGoogleDiscoveryURL overrides the discovery URL (for testing).
func setGoogleDiscoveryURL(url string) {
	googleDiscoveryURL = url
}

// teeState tracks attestation and session state for the host-side TEE flow.
type teeState struct {
	mu           sync.Mutex
	attested     bool
	nonce        []byte
	attestResp   enclave.AttestationResponse
	claims       *enclave.AttestationClaims // verified claims from server-side verification
	userSessions map[string]time.Time       // userID -> session expiry

	// JWKS cache
	jwksData      []byte
	jwksFetchedAt time.Time
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
		claims := s.teeState.claims
		s.teeState.mu.Unlock()

		status.Attested = &attested
		// Session is per-user; report aggregate active state.
		sessionActive := false
		status.SessionActive = &sessionActive

		if claims != nil {
			status.AttestationClaims = &struct {
				ExpiresAt   *time.Time `json:"expires_at,omitempty"`
				Hwmodel     *string    `json:"hwmodel,omitempty"`
				ImageDigest *string    `json:"image_digest,omitempty"`
				IssuedAt    *time.Time `json:"issued_at,omitempty"`
				Issuer      *string    `json:"issuer,omitempty"`
				ProjectId   *string    `json:"project_id,omitempty"`
			}{
				ImageDigest: &claims.ImageDigest,
				ProjectId:   &claims.ProjectID,
				Issuer:      &claims.Issuer,
				Hwmodel:     &claims.HWModel,
				IssuedAt:    &claims.IssuedAt,
				ExpiresAt:   &claims.ExpiresAt,
			}
		}
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *apiServer) GetTeeJwks(w http.ResponseWriter, r *http.Request) {
	if s.teeState == nil {
		writeError(w, http.StatusNotImplemented, "tee_disabled", "TEE is not enabled")
		return
	}

	s.teeState.mu.Lock()
	cached := s.teeState.jwksData
	fresh := time.Since(s.teeState.jwksFetchedAt) < jwksCacheTTL
	s.teeState.mu.Unlock()

	if fresh && cached != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(cached)
		return
	}

	// Fetch JWKS URI from OIDC discovery document.
	jwksURL, err := s.fetchJWKSURL(r.Context())
	if err != nil {
		s.log.Error("failed to fetch JWKS URL from OIDC discovery", "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "failed to fetch OIDC discovery document")
		return
	}

	// Fetch the JWKS.
	jwksData, err := s.fetchJWKS(r.Context(), jwksURL)
	if err != nil {
		s.log.Error("failed to fetch JWKS", "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "failed to fetch JWKS from upstream")
		return
	}

	// Cache the response.
	s.teeState.mu.Lock()
	s.teeState.jwksData = jwksData
	s.teeState.jwksFetchedAt = time.Now()
	s.teeState.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(jwksData)
}

// fetchJWKSURL retrieves the jwks_uri from the Confidential Space OIDC
// discovery document.
func (s *apiServer) fetchJWKSURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleDiscoveryURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching OIDC discovery: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC discovery returned status %d", resp.StatusCode)
	}

	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("parsing OIDC discovery: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("OIDC discovery document missing jwks_uri")
	}
	return doc.JWKSURI, nil
}

// fetchJWKS retrieves the raw JWKS JSON from the given URL.
func (s *apiServer) fetchJWKS(ctx context.Context, jwksURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading JWKS response: %w", err)
	}
	return data, nil
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

	// Server-side verification to extract claims for the status endpoint.
	// Best-effort: failure here does not block the attestation flow since
	// the client independently verifies the token.
	var verifiedClaims *enclave.AttestationClaims
	if s.enclaveVerifier != nil {
		c, verifyErr := s.enclaveVerifier.Verify(r.Context(), attestResp.Token, nonce)
		if verifyErr != nil {
			s.log.Warn("server-side attestation verification failed (non-fatal)", "error", verifyErr)
		} else {
			verifiedClaims = &c
		}
	}

	// Store state for the session establishment step.
	s.teeState.mu.Lock()
	s.teeState.nonce = nonce
	s.teeState.attestResp = attestResp
	s.teeState.attested = true
	s.teeState.claims = verifiedClaims
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
		if s.escrowIndexStore != nil {
			_ = s.escrowIndexStore.Upsert(ctx, acc.VaultPath(), resp.EscrowID, userID, expiresAt)
		}
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
