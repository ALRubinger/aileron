package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/account"
	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/config"
	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
	"github.com/ALRubinger/aileron/enclave/local"
	"github.com/golang-jwt/jwt/v5"
)

func teeJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// --- JWKS proxy tests ---

func TestGetTeeJwks_Disabled(t *testing.T) {
	s := &apiServer{} // teeState is nil

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/jwks", nil)
	s.GetTeeJwks(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetTeeJwks_FetchesAndCaches(t *testing.T) {
	// Stand up fake JWKS and discovery endpoints.
	jwksBody := `{"keys":[{"kty":"RSA","kid":"test-kid","n":"abc","e":"AQAB"}]}`
	fetchCount := 0
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jwksBody))
	}))
	defer jwksServer.Close()

	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"jwks_uri": jwksServer.URL})
	}))
	defer discoveryServer.Close()

	// Temporarily override the discovery URL.
	origURL := googleDiscoveryURL
	setGoogleDiscoveryURL(discoveryServer.URL)
	defer setGoogleDiscoveryURL(origURL)

	s := &apiServer{
		log:      slog.Default(),
		teeState: newTeeState(),
	}

	// First request: should fetch from upstream.
	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodGet, "/v1/tee/jwks", nil)
	s.GetTeeJwks(w1, r1)

	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	if w1.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("expected application/json, got %q", w1.Header().Get("Content-Type"))
	}
	if w1.Body.String() != jwksBody {
		t.Fatalf("unexpected body: %s", w1.Body.String())
	}
	if fetchCount != 1 {
		t.Fatalf("expected 1 upstream fetch, got %d", fetchCount)
	}

	// Second request: should serve from cache (no additional fetch).
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/v1/tee/jwks", nil)
	s.GetTeeJwks(w2, r2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 from cache, got %d", w2.Code)
	}
	if fetchCount != 1 {
		t.Fatalf("expected cache hit (still 1 fetch), got %d fetches", fetchCount)
	}
}

func TestGetTeeJwks_UpstreamFailure(t *testing.T) {
	// Discovery endpoint returns an error.
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer discoveryServer.Close()

	origURL := googleDiscoveryURL
	setGoogleDiscoveryURL(discoveryServer.URL)
	defer setGoogleDiscoveryURL(origURL)

	s := &apiServer{
		log:      slog.Default(),
		teeState: newTeeState(),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/jwks", nil)
	s.GetTeeJwks(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetTeeJwks_JwksEndpointFailure(t *testing.T) {
	// Discovery succeeds but JWKS endpoint returns an error.
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer jwksServer.Close()

	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"jwks_uri": jwksServer.URL})
	}))
	defer discoveryServer.Close()

	origURL := googleDiscoveryURL
	setGoogleDiscoveryURL(discoveryServer.URL)
	defer setGoogleDiscoveryURL(origURL)

	s := &apiServer{
		log:      slog.Default(),
		teeState: newTeeState(),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/jwks", nil)
	s.GetTeeJwks(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetTeeJwks_DiscoveryMissingJwksUri(t *testing.T) {
	// Discovery returns valid JSON but no jwks_uri field.
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"issuer": "https://example.com"})
	}))
	defer discoveryServer.Close()

	origURL := googleDiscoveryURL
	setGoogleDiscoveryURL(discoveryServer.URL)
	defer setGoogleDiscoveryURL(origURL)

	s := &apiServer{
		log:      slog.Default(),
		teeState: newTeeState(),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/jwks", nil)
	s.GetTeeJwks(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetTeeJwks_DiscoveryInvalidJSON(t *testing.T) {
	// Discovery returns invalid JSON.
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	defer discoveryServer.Close()

	origURL := googleDiscoveryURL
	setGoogleDiscoveryURL(discoveryServer.URL)
	defer setGoogleDiscoveryURL(origURL)

	s := &apiServer{
		log:      slog.Default(),
		teeState: newTeeState(),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/jwks", nil)
	s.GetTeeJwks(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetTeeJwks_NoAuth(t *testing.T) {
	// The JWKS endpoint must be accessible without authentication.
	jwksBody := `{"keys":[]}`
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(jwksBody))
	}))
	defer jwksServer.Close()

	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"jwks_uri": jwksServer.URL})
	}))
	defer discoveryServer.Close()

	origURL := googleDiscoveryURL
	setGoogleDiscoveryURL(discoveryServer.URL)
	defer setGoogleDiscoveryURL(origURL)

	s := &apiServer{
		log:      slog.Default(),
		teeState: newTeeState(),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/jwks", nil)
	// No auth claims — simulating an unauthenticated request.
	s.GetTeeJwks(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 without auth, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetTeeStatusDisabled(t *testing.T) {
	s := &apiServer{
		teeCfg: &config.TEEConfig{},
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/status", nil)
	s.GetTeeStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var status api.TeeStatus
	json.NewDecoder(w.Body).Decode(&status)
	if status.Enabled {
		t.Fatal("expected enabled=false")
	}
	if status.Provider != api.None {
		t.Fatalf("expected provider none, got %q", status.Provider)
	}
}

func TestGetTeeStatusEnabled(t *testing.T) {
	s := &apiServer{
		teeCfg:   &config.TEEConfig{Provider: "local"},
		teeState: newTeeState(),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/status", nil)
	s.GetTeeStatus(w, r)

	var status api.TeeStatus
	json.NewDecoder(w.Body).Decode(&status)
	if !status.Enabled {
		t.Fatal("expected enabled=true")
	}
	if status.Provider != api.Local {
		t.Fatalf("expected provider local, got %q", status.Provider)
	}
}

func TestGetTeeStatus_PublicNoAuth(t *testing.T) {
	// The TEE status endpoint must be accessible without authentication.
	// This test verifies the handler returns 200 even with no claims in context.
	s := &apiServer{
		teeCfg:   &config.TEEConfig{Provider: "local"},
		teeState: newTeeState(),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/status", nil)
	// No auth claims — simulating an unauthenticated request.
	s.GetTeeStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 without auth, got %d: %s", w.Code, w.Body.String())
	}

	var status api.TeeStatus
	json.NewDecoder(w.Body).Decode(&status)
	if !status.Enabled {
		t.Fatal("expected enabled=true")
	}
}

func TestGetTeeStatusConfidentialSpace(t *testing.T) {
	s := &apiServer{
		teeCfg:   &config.TEEConfig{Provider: "confidential-space"},
		teeState: newTeeState(),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/tee/status", nil)
	s.GetTeeStatus(w, r)

	var status api.TeeStatus
	json.NewDecoder(w.Body).Decode(&status)
	if status.Provider != api.ConfidentialSpace {
		t.Fatalf("expected confidential-space, got %q", status.Provider)
	}
}

func TestInitiateAttestationDisabled(t *testing.T) {
	s := &apiServer{}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/tee/attestation", nil)
	s.InitiateAttestation(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestInitiateAttestationLocal(t *testing.T) {
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)
	s := &apiServer{
		enclaveClient: client,
		teeState:      newTeeState(),
		teeCfg:        &config.TEEConfig{Provider: "local"},
	}

	w := httptest.NewRecorder()
	r := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	s.InitiateAttestation(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp api.TeeAttestationResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Token != "dev-ok" {
		t.Fatalf("expected dev-ok, got %q", resp.Token)
	}
	if len(resp.Nonce) == 0 {
		t.Fatal("expected non-empty nonce")
	}
	if len(resp.PublicKey) == 0 {
		t.Fatal("expected non-empty public key")
	}
}

func TestEstablishTeeSessionDisabled(t *testing.T) {
	s := &apiServer{}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/tee/session", nil)
	s.EstablishTeeSession(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestEstablishTeeSessionUnauthenticated(t *testing.T) {
	client := local.New(nil)
	s := &apiServer{
		enclaveClient: client,
		teeState:      newTeeState(),
		teeCfg:        &config.TEEConfig{Provider: "local"},
	}

	w := httptest.NewRecorder()
	r := teeJSONRequest(t, http.MethodPost, "/v1/tee/session", api.TeeSessionRequest{
		EncryptedKek:    []byte("encrypted"),
		ClientPublicKey: []byte("pubkey"),
	})
	// No auth claims attached.
	s.EstablishTeeSession(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// attestAndEstablishSession performs the full client-side flow:
// 1. Attest (get enclave public key)
// 2. Generate client ECDH key pair
// 3. Derive shared secret
// 4. Encrypt KEK with shared secret
// 5. Call EstablishTeeSession with encrypted blob
func attestAndEstablishSession(t *testing.T, srv *apiServer, claims *auth.Claims, kek []byte) api.TeeSessionResponse {
	t.Helper()

	// Step 1: Attest.
	w1 := httptest.NewRecorder()
	r1 := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	srv.InitiateAttestation(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("attestation: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var attestResp api.TeeAttestationResponse
	json.NewDecoder(w1.Body).Decode(&attestResp)

	// Step 2: Generate client ECDH key pair and derive shared secret.
	clientKey, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generating client key pair: %v", err)
	}
	enclavePub, err := crypto.UnmarshalPublicKey(attestResp.PublicKey)
	if err != nil {
		t.Fatalf("unmarshaling enclave public key: %v", err)
	}
	sharedSecret, err := crypto.DeriveSharedSecret(clientKey, enclavePub)
	if err != nil {
		t.Fatalf("deriving shared secret: %v", err)
	}

	// Step 3: Encrypt KEK with shared secret.
	encryptedKEK, err := crypto.Encrypt(kek, sharedSecret)
	if err != nil {
		t.Fatalf("encrypting KEK: %v", err)
	}

	// Step 4: Establish session.
	clientPubBytes := crypto.MarshalPublicKey(clientKey.PublicKey())
	sessionReq := api.TeeSessionRequest{
		EncryptedKek:    encryptedKEK,
		ClientPublicKey: clientPubBytes,
	}
	w2 := httptest.NewRecorder()
	r2 := teeJSONRequest(t, http.MethodPost, "/v1/tee/session", sessionReq)
	r2 = r2.WithContext(auth.ContextWithClaims(r2.Context(), claims))
	srv.EstablishTeeSession(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("session: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var sessResp api.TeeSessionResponse
	json.NewDecoder(w2.Body).Decode(&sessResp)
	return sessResp
}

func TestEstablishTeeSessionLocal(t *testing.T) {
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)

	teeClaims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_tee_test"},
		EnterpriseID:     "ent_test",
		Email:            "tee@example.com",
		Role:             "owner",
	}

	srv := &apiServer{
		enclaveClient: client,
		teeState:      newTeeState(),
		teeCfg:        &config.TEEConfig{Provider: "local"},
	}

	// Generate a test KEK.
	kek := []byte("0123456789abcdef0123456789abcdef")

	sessResp := attestAndEstablishSession(t, srv, teeClaims, kek)

	if sessResp.SessionId == "" {
		t.Fatal("expected non-empty session ID")
	}
	if sessResp.ExpiresAt == nil {
		t.Fatal("expected non-nil expires_at")
	}

	// Verify the user session was tracked.
	srv.teeState.mu.Lock()
	_, tracked := srv.teeState.userSessions[teeClaims.Subject]
	srv.teeState.mu.Unlock()
	if !tracked {
		t.Fatal("expected user session to be tracked in teeState")
	}
}

func TestEstablishTeeSession_TransmitKEKFails(t *testing.T) {
	// Use a client that fails on TransmitKEK.
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)

	teeClaims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_tee_fail"},
		EnterpriseID:     "ent_test",
		Email:            "fail@example.com",
		Role:             "owner",
	}

	srv := &apiServer{
		enclaveClient: client,
		teeState:      newTeeState(),
		teeCfg:        &config.TEEConfig{Provider: "local"},
	}

	// Attest first.
	w1 := httptest.NewRecorder()
	r1 := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	srv.InitiateAttestation(w1, r1)
	var attestResp api.TeeAttestationResponse
	json.NewDecoder(w1.Body).Decode(&attestResp)

	// Generate keys and encrypt a blob, but use wrong/garbage data that
	// the enclave can't decrypt.
	clientKey, _ := crypto.GenerateKeyPair()
	clientPubBytes := crypto.MarshalPublicKey(clientKey.PublicKey())

	w2 := httptest.NewRecorder()
	r2 := teeJSONRequest(t, http.MethodPost, "/v1/tee/session", api.TeeSessionRequest{
		EncryptedKek:    []byte("garbage-encrypted-kek-data"),
		ClientPublicKey: clientPubBytes,
	})
	r2 = r2.WithContext(auth.ContextWithClaims(r2.Context(), teeClaims))
	srv.EstablishTeeSession(w2, r2)

	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestEstablishTeeSession_AutoEscrows(t *testing.T) {
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)

	teeClaims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_escrow_test"},
		EnterpriseID:     "ent_test",
		Email:            "escrow@example.com",
		Role:             "owner",
	}

	connAccounts := mem.NewConnectedAccountStore()
	ctx := context.Background()
	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: teeClaims.Subject,
		Provider: model.ConnectedAccountProviderGmail,
		Status:   model.ConnectedAccountStatusActive,
	})

	v := vault.NewMemVault()
	encMeta := vault.Metadata{Type: "oauth_refresh_token", Labels: map[string]string{vault.EncryptedLabel: "true"}}

	// The KEK we'll transmit.
	passphrase := "correct horse battery staple"
	salt, _ := crypto.GenerateSalt()
	kek, _ := crypto.DeriveKEK([]byte(passphrase), salt)

	// Encrypt a credential with this KEK so escrow can decrypt it.
	encryptedCred, _ := crypto.Encrypt([]byte(`{"refresh_token":"rt"}`), kek)
	v.Put(ctx, "connected-accounts/"+teeClaims.Subject+"/gmail", encryptedCred, encMeta)

	srv := &apiServer{
		enclaveClient:     client,
		teeState:          newTeeState(),
		teeCfg:            &config.TEEConfig{Provider: "local"},
		connectedAccounts: connAccounts,
		vault:             v,
		escrowTTL:         7 * 24 * 3600e9, // 7 days
	}

	sessResp := attestAndEstablishSession(t, srv, teeClaims, kek)
	if sessResp.EscrowedCount == nil || *sessResp.EscrowedCount != 1 {
		escrowed := 0
		if sessResp.EscrowedCount != nil {
			escrowed = *sessResp.EscrowedCount
		}
		t.Fatalf("expected escrowed_count=1, got %d", escrowed)
	}
}

func TestInitiateAttestationBadJSON(t *testing.T) {
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)
	s := &apiServer{
		enclaveClient: client,
		teeState:      newTeeState(),
		teeCfg:        &config.TEEConfig{Provider: "local"},
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/tee/attestation", bytes.NewReader([]byte("not json")))
	r.Header.Set("Content-Type", "application/json")
	s.InitiateAttestation(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEstablishTeeSessionBadJSON(t *testing.T) {
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)
	s := &apiServer{
		enclaveClient: client,
		teeState:      newTeeState(),
		teeCfg:        &config.TEEConfig{Provider: "local"},
	}

	teeClaims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_bad_json"},
		EnterpriseID:     "ent_test",
		Email:            "bad@example.com",
		Role:             "owner",
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/tee/session", bytes.NewReader([]byte("not json")))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.ContextWithClaims(r.Context(), teeClaims))
	s.EstablishTeeSession(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConnectAccountCallbackTEEBranch(t *testing.T) {
	// Create mock token and userinfo endpoints.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	userinfoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"email": "testuser@gmail.com",
		})
	}))
	defer userinfoServer.Close()

	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)

	callbackClaims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_cb_test"},
		EnterpriseID:     "ent_test",
		Email:            "cbtest@example.com",
		Role:             "owner",
	}

	connectedAccounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	googleSvc := account.NewGoogleService("test-client-id", "test-client-secret", connectedAccounts, v).
		WithEndpoints(tokenServer.URL, userinfoServer.URL)
	accountReg := account.NewRegistry()
	accountReg.Register(googleSvc)

	srv := &apiServer{
		log:               slog.Default(),
		enclaveClient:     client,
		teeState:          newTeeState(),
		teeCfg:            &config.TEEConfig{Provider: "local"},
		connectedAccounts: connectedAccounts,
		vault:             v,
		accountService:    accountReg,
		users:             &stubUserStore{},
		newID:             func() string { return "test-id" },
		userKeyMaterials:  mem.NewUserKeyMaterialStore(),
	}

	// Step 1: Transmit KEK to enclave using the client-side flow.
	passphrase := "correct horse battery staple"
	salt, _ := crypto.GenerateSalt()
	kek, _ := crypto.DeriveKEK([]byte(passphrase), salt)
	_ = attestAndEstablishSession(t, srv, callbackClaims, kek)

	// Step 2: Set passphrase (for vault metadata).
	verification, _ := crypto.Encrypt(testVerificationConstant, kek)
	setBody := map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(salt),
		"kek_verification": base64.StdEncoding.EncodeToString(verification),
	}
	setJSON, _ := json.Marshal(setBody)
	setReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(setJSON), callbackClaims)
	wSet := httptest.NewRecorder()
	srv.SetPassphrase(wSet, setReq)
	if wSet.Code != http.StatusOK {
		t.Fatalf("set passphrase: expected 200, got %d: %s", wSet.Code, wSet.Body.String())
	}

	// Step 3: Call ConnectAccountCallback in TEE mode.
	state := "test-state"
	w4 := httptest.NewRecorder()
	r4 := mcpRequest("GET", "/v1/connect/gmail/callback?code=test-code&state="+state, "", callbackClaims)
	r4.AddCookie(&http.Cookie{Name: "aileron_connect_state", Value: state})
	srv.ConnectAccountCallback(w4, r4, "gmail", api.ConnectAccountCallbackParams{
		Code:  "test-code",
		State: &state,
	})

	if w4.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", w4.Code, w4.Body.String())
	}

	// Verify the connected account was created.
	accounts, err := connectedAccounts.List(r4.Context(), store.ConnectedAccountFilter{UserID: "usr_cb_test"})
	if err != nil {
		t.Fatalf("listing accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 connected account, got %d", len(accounts))
	}
	if accounts[0].ExternalUserID != "testuser@gmail.com" {
		t.Fatalf("expected ExternalUserID testuser@gmail.com, got %q", accounts[0].ExternalUserID)
	}

	// Verify the encrypted token was stored in the vault.
	secret, err := v.Get(r4.Context(), accounts[0].VaultPath())
	if err != nil {
		t.Fatalf("getting vault secret: %v", err)
	}
	if len(secret.Value) == 0 {
		t.Fatal("expected non-empty encrypted token in vault")
	}
	if secret.Metadata.Labels["encrypted"] != "true" {
		t.Fatal("expected encrypted=true label on vault secret")
	}

	// Verify the credential was auto-escrowed for async access.
	vaultPath := accounts[0].VaultPath()
	escrowID, ok := srv.escrowIndex.Load(vaultPath)
	if !ok {
		t.Fatalf("expected escrow index entry for %q after connect", vaultPath)
	}
	if escrowID.(string) == "" {
		t.Fatal("expected non-empty escrow ID")
	}
}
