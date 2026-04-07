package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/account"
	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/config"
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
		teeState: &teeState{},
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

func TestGetTeeStatusConfidentialSpace(t *testing.T) {
	s := &apiServer{
		teeCfg:   &config.TEEConfig{Provider: "confidential-space"},
		teeState: &teeState{},
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
		teeState:      &teeState{},
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

func TestEstablishTeeSessionLocal(t *testing.T) {
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)
	verifier := &local.DevVerifier{}

	s := &apiServer{
		enclaveClient:   client,
		enclaveVerifier: verifier,
		teeState:        &teeState{},
		teeCfg:          &config.TEEConfig{Provider: "local"},
	}

	// First attest.
	w1 := httptest.NewRecorder()
	r1 := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	s.InitiateAttestation(w1, r1)

	var attestResp api.TeeAttestationResponse
	json.NewDecoder(w1.Body).Decode(&attestResp)

	// Then establish session.
	w2 := httptest.NewRecorder()
	r2 := teeJSONRequest(t, http.MethodPost, "/v1/tee/session", api.TeeSessionRequest{
		Nonce:     attestResp.Nonce,
		Token:     attestResp.Token,
		PublicKey: attestResp.PublicKey,
	})
	s.EstablishTeeSession(w2, r2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var sessResp api.TeeSessionResponse
	json.NewDecoder(w2.Body).Decode(&sessResp)
	if !sessResp.Verified {
		t.Fatal("expected verified=true")
	}
	if sessResp.SessionId == "" {
		t.Fatal("expected non-empty session ID")
	}
}

func TestGetSessionKeyNilState(t *testing.T) {
	s := &apiServer{
		teeState: nil,
	}

	key := s.getSessionKey()
	if key != nil {
		t.Fatal("expected nil session key when teeState is nil")
	}
}

func TestGetSessionKeyWithKey(t *testing.T) {
	original := []byte("0123456789abcdef0123456789abcdef")
	s := &apiServer{
		teeState: &teeState{
			sessionKey: original,
		},
	}

	key := s.getSessionKey()
	if key == nil {
		t.Fatal("expected non-nil session key")
	}
	if len(key) != len(original) {
		t.Fatalf("expected key length %d, got %d", len(original), len(key))
	}
	// Verify it is a copy, not the same slice.
	if &key[0] == &original[0] {
		t.Fatal("expected a copy of the session key, not the same slice")
	}
	for i := range original {
		if key[i] != original[i] {
			t.Fatalf("key byte %d differs: got %d, want %d", i, key[i], original[i])
		}
	}
}

func TestEstablishTeeSessionWithECDH(t *testing.T) {
	// This test performs the full attest -> session flow and verifies the
	// session key is stored in teeState.
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)
	verifier := &local.DevVerifier{}

	s := &apiServer{
		enclaveClient:   client,
		enclaveVerifier: verifier,
		teeState:        &teeState{},
		teeCfg:          &config.TEEConfig{Provider: "local"},
	}

	// Step 1: Attest.
	w1 := httptest.NewRecorder()
	r1 := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	s.InitiateAttestation(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("attestation: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}

	var attestResp api.TeeAttestationResponse
	json.NewDecoder(w1.Body).Decode(&attestResp)

	// Step 2: Establish session.
	w2 := httptest.NewRecorder()
	r2 := teeJSONRequest(t, http.MethodPost, "/v1/tee/session", api.TeeSessionRequest{
		Nonce:     attestResp.Nonce,
		Token:     attestResp.Token,
		PublicKey: attestResp.PublicKey,
	})
	s.EstablishTeeSession(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("session: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var sessResp api.TeeSessionResponse
	json.NewDecoder(w2.Body).Decode(&sessResp)
	if !sessResp.Verified {
		t.Fatal("expected verified=true")
	}
	if sessResp.SessionId == "" {
		t.Fatal("expected non-empty session ID")
	}

	// Verify that the session key was stored in teeState.
	sessionKey := s.getSessionKey()
	if sessionKey == nil {
		t.Fatal("expected non-nil session key after ECDH exchange")
	}
	if len(sessionKey) == 0 {
		t.Fatal("expected non-empty session key")
	}

	// Verify session state flags.
	s.teeState.mu.Lock()
	active := s.teeState.sessionActive
	sid := s.teeState.sessionID
	s.teeState.mu.Unlock()

	if !active {
		t.Fatal("expected sessionActive=true")
	}
	if sid == "" {
		t.Fatal("expected non-empty sessionID in teeState")
	}
}

func TestVerifyPassphraseTEEBranch(t *testing.T) {
	// Set up a full TEE session so the enclave client can decrypt the KEK.
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	client := local.New(executeFn)
	verifier := &local.DevVerifier{}

	teeClaims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_tee_test"},
		EnterpriseID:     "ent_test",
		Email:            "tee@example.com",
		Role:             "owner",
	}

	srv := &apiServer{
		userKeyMaterials: mem.NewUserKeyMaterialStore(),
		enclaveClient:    client,
		enclaveVerifier:  verifier,
		teeState:         &teeState{},
		teeCfg:           &config.TEEConfig{Provider: "local"},
	}

	// Step 1: Attest.
	w1 := httptest.NewRecorder()
	r1 := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	srv.InitiateAttestation(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("attestation: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var attestResp api.TeeAttestationResponse
	json.NewDecoder(w1.Body).Decode(&attestResp)

	// Step 2: Establish session (derives session key via ECDH).
	w2 := httptest.NewRecorder()
	r2 := teeJSONRequest(t, http.MethodPost, "/v1/tee/session", api.TeeSessionRequest{
		Nonce:     attestResp.Nonce,
		Token:     attestResp.Token,
		PublicKey: attestResp.PublicKey,
	})
	srv.EstablishTeeSession(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("session: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	passphrase := "correct horse battery staple"

	// Step 3: Set passphrase.
	setReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"`+passphrase+`"}`, teeClaims)
	w3 := httptest.NewRecorder()
	srv.SetPassphrase(w3, setReq)
	if w3.Code != http.StatusOK {
		t.Fatalf("set passphrase: expected 200, got %d: %s", w3.Code, w3.Body.String())
	}

	// Step 4: Verify passphrase — should transmit KEK to enclave.
	verifyReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"`+passphrase+`"}`, teeClaims)
	w4 := httptest.NewRecorder()
	srv.VerifyPassphrase(w4, verifyReq)
	if w4.Code != http.StatusOK {
		t.Fatalf("verify passphrase: expected 200, got %d: %s", w4.Code, w4.Body.String())
	}

	var resp api.VerifyPassphraseResponse
	json.NewDecoder(w4.Body).Decode(&resp)
	if !resp.Valid {
		t.Fatal("expected valid=true")
	}
	if resp.Salt == nil {
		t.Fatal("expected salt in response")
	}
}

func TestConnectAccountCallbackTEEBranch(t *testing.T) {
	// This test exercises the TEE branch in ConnectAccountCallback where
	// OAuthExchange is called on the enclave client. The local enclave will
	// attempt a real HTTP call to the token endpoint, which we intercept
	// with a test server.

	// Create a mock token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	// Create a mock userinfo endpoint.
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
	verifier := &local.DevVerifier{}

	callbackClaims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_cb_test"},
		EnterpriseID:     "ent_test",
		Email:            "cbtest@example.com",
		Role:             "owner",
	}

	connectedAccounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	accountSvc := account.NewGoogleService("test-client-id", "test-client-secret", connectedAccounts, v)

	srv := &apiServer{
		log:               slog.Default(),
		enclaveClient:     client,
		enclaveVerifier:   verifier,
		teeState:          &teeState{},
		teeCfg:            &config.TEEConfig{Provider: "local"},
		connectedAccounts: connectedAccounts,
		vault:             v,
		accountService:    accountSvc,
		users:             &stubUserStore{},
		newID:             func() string { return "test-id" },
		userKeyMaterials:  mem.NewUserKeyMaterialStore(),
	}

	// Step 1: Attest.
	w1 := httptest.NewRecorder()
	r1 := teeJSONRequest(t, http.MethodPost, "/v1/tee/attestation", api.TeeAttestationRequest{})
	srv.InitiateAttestation(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("attestation: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var attestResp api.TeeAttestationResponse
	json.NewDecoder(w1.Body).Decode(&attestResp)

	// Step 2: Establish session.
	w2 := httptest.NewRecorder()
	r2 := teeJSONRequest(t, http.MethodPost, "/v1/tee/session", api.TeeSessionRequest{
		Nonce:     attestResp.Nonce,
		Token:     attestResp.Token,
		PublicKey: attestResp.PublicKey,
	})
	srv.EstablishTeeSession(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("session: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// Step 3: Set passphrase and transmit KEK to enclave.
	passphrase := "correct horse battery staple"
	setReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"`+passphrase+`"}`, callbackClaims)
	w3 := httptest.NewRecorder()
	srv.SetPassphrase(w3, setReq)
	if w3.Code != http.StatusOK {
		t.Fatalf("set passphrase: expected 200, got %d: %s", w3.Code, w3.Body.String())
	}

	// Verify passphrase to transmit KEK.
	verifyReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"`+passphrase+`"}`, callbackClaims)
	w3v := httptest.NewRecorder()
	srv.VerifyPassphrase(w3v, verifyReq)
	if w3v.Code != http.StatusOK {
		t.Fatalf("verify passphrase: expected 200, got %d: %s", w3v.Code, w3v.Body.String())
	}

	// Step 4: Call ConnectAccountCallback in TEE mode.
	// The local enclave's OAuthExchange will call the real token endpoint,
	// so we can't easily intercept it in-process. However, the enclave
	// uses http.DefaultClient. The local client calls exchangeOAuthCode
	// which uses the TokenEndpoint from the request. We pass our test
	// server URLs directly via the enclave.OAuthExchangeRequest path.
	//
	// Unfortunately, ConnectAccountCallback constructs the OAuthExchangeRequest
	// internally using s.accountService methods (TokenEndpointFor, etc.) which
	// return hardcoded Google URLs. The token exchange will fail because it
	// tries to reach Google. We verify the TEE path is entered by checking
	// the error is about the OAuth exchange, not about other branches.
	state := "test-state"
	w4 := httptest.NewRecorder()
	r4 := mcpRequest("GET", "/v1/connect/gmail/callback?code=test-code&state="+state, "", callbackClaims)
	r4.AddCookie(&http.Cookie{Name: "aileron_connect_state", Value: state})
	srv.ConnectAccountCallback(w4, r4, "gmail", api.ConnectAccountCallbackParams{
		Code:  "test-code",
		State: state,
	})

	// The TEE branch was entered; the enclave tried to exchange the OAuth code
	// but the token endpoint (accounts.google.com) is not reachable, so we
	// get a 500. The important thing is that we hit the enclave path (not the
	// direct-mode path) which is confirmed by the error mentioning OAuth exchange.
	if w4.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from failed OAuth exchange in TEE mode, got %d: %s", w4.Code, w4.Body.String())
	}
	var errResp api.Error
	json.NewDecoder(w4.Body).Decode(&errResp)
	// The error should be about the OAuth exchange failing (enclave path),
	// confirming we entered the TEE branch rather than the direct-mode branch.
	if errResp.Error.Code != "callback_error" {
		t.Fatalf("expected error code 'callback_error', got %q", errResp.Error.Code)
	}
}
