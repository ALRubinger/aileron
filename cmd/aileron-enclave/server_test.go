package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/connector"
	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/enclave"
)

// stubConnector returns a fixed result for testing.
type stubConnector struct{}

func (s *stubConnector) Type() string     { return "test" }
func (s *stubConnector) Provider() string { return "stub" }
func (s *stubConnector) Execute(_ context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	return connector.ExecutionResult{
		Status:     connector.ExecutionStatusSucceeded,
		Output:     map[string]any{"credential_type": req.Credential.Type},
		ReceiptRef: "test-receipt",
	}, nil
}

func setupTestEnclaveServer(t *testing.T) (*httptest.Server, *enclaveServer) {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	registry := connector.NewRegistry()
	registry.Register(context.Background(), &stubConnector{})
	srv, err := newEnclaveServer(log, registry, "local", "")
	if err != nil {
		t.Fatalf("newEnclaveServer: %v", err)
	}
	mux := http.NewServeMux()
	srv.registerRoutes(mux)
	return httptest.NewServer(mux), srv
}

func postJSON(t *testing.T, server *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	payload, _ := json.Marshal(body)
	resp, err := http.Post(server.URL+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func decodeResp[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return v
}

// establishTestSession performs attest → session and returns the session key.
func establishTestSession(t *testing.T, server *httptest.Server) []byte {
	t.Helper()

	// Attest.
	attestResp := postJSON(t, server, "/attest", enclave.AttestationRequest{
		Nonce: []byte("test"), Audience: "test",
	})
	attestResult := decodeResp[enclave.AttestationResponse](t, attestResp)
	if attestResult.Token != "dev-ok" {
		t.Fatalf("expected dev-ok, got %q", attestResult.Token)
	}

	// Host key.
	hostKey, _ := ecdh.P256().GenerateKey(rand.Reader)

	// Session.
	sessResp := postJSON(t, server, "/session", enclave.SessionRequest{
		PublicKey: hostKey.PublicKey().Bytes(),
	})
	sessResult := decodeResp[enclave.SessionResponse](t, sessResp)
	if sessResult.SessionID == "" {
		t.Fatal("empty session ID")
	}

	// Derive shared secret.
	enclavePub, _ := ecdh.P256().NewPublicKey(attestResult.PublicKey)
	raw, _ := hostKey.ECDH(enclavePub)
	h := sha256.Sum256(raw)
	return h[:]
}

// transmitTestKEK encrypts a KEK with the session key and sends it to the enclave for a user.
func transmitTestKEK(t *testing.T, server *httptest.Server, sessionKey []byte, userID string, kek []byte) {
	t.Helper()
	encrypted, _ := crypto.Encrypt(kek, sessionKey)
	resp := postJSON(t, server, "/kek", enclave.TransmitKEKRequest{
		UserID:       userID,
		EncryptedKEK: encrypted,
	})
	result := decodeResp[enclave.TransmitKEKResponse](t, resp)
	if !result.Stored {
		t.Fatal("expected Stored=true")
	}
}

func TestHealthEndpoint(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Fatalf("expected ok status, got %v", result["status"])
	}
}

func TestFullExecuteFlow(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)

	// Transmit a user KEK.
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	// Encrypt credential with the user's KEK (simulating vault storage).
	plainCred := []byte("test-api-key")
	kekEncrypted, err := crypto.Encrypt(plainCred, userKEK)
	if err != nil {
		t.Fatalf("encrypting with KEK: %v", err)
	}

	// Execute — enclave decrypts with stored KEK.
	execResp := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		RequestID:           "exec-1",
		UserID:              "user-1",
		GrantID:             "grant-1",
		ActionType:          "test.action",
		ConnectorID:         "test/stub",
		EncryptedCredential: kekEncrypted,
		CredentialType:      "api_key",
	})
	execResult := decodeResp[enclave.ExecuteResponse](t, execResp)
	if execResult.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q (error: %s)", execResult.Status, execResult.Error)
	}
	if execResult.ReceiptRef != "test-receipt" {
		t.Fatalf("expected test-receipt, got %q", execResult.ReceiptRef)
	}
}

func TestExecuteWithoutKEK(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	// Execute without transmitting KEK.
	resp := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		UserID:              "no-kek-user",
		ConnectorID:         "test/stub",
		EncryptedCredential: []byte("data"),
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSessionWithoutAttest(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp := postJSON(t, server, "/session", enclave.SessionRequest{
		PublicKey: make([]byte, 65),
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestExecuteUnknownConnector(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("cred"), userKEK)
	resp := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		UserID:              "user-1",
		ConnectorID:         "unknown/provider",
		EncryptedCredential: encrypted,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEscrowFlow(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("escrowed-cred"), userKEK)

	// Store.
	storeResp := postJSON(t, server, "/escrow", enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "grant-1",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	storeResult := decodeResp[enclave.EscrowStoreResponse](t, storeResp)
	if storeResult.EscrowID == "" {
		t.Fatal("empty escrow ID")
	}

	// Execute with escrow.
	execResp := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		RequestID:   "exec-escrow",
		ConnectorID: "test/stub",
		EscrowID:    storeResult.EscrowID,
	})
	execResult := decodeResp[enclave.ExecuteResponse](t, execResp)
	if execResult.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q", execResult.Status)
	}

	// Revoke.
	revokeResp := postJSON(t, server, "/escrow/revoke", enclave.EscrowRevokeRequest{
		EscrowID: storeResult.EscrowID,
		GrantID:  "grant-1",
	})
	if revokeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", revokeResp.StatusCode)
	}
	revokeResp.Body.Close()

	// Execute after revoke should fail.
	execResp2 := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		ConnectorID: "test/stub",
		EscrowID:    storeResult.EscrowID,
	})
	if execResp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after revoke, got %d", execResp2.StatusCode)
	}
	execResp2.Body.Close()
}

func TestAttestEndpoint(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp := postJSON(t, server, "/attest", enclave.AttestationRequest{
		Nonce:    []byte("test-nonce"),
		Audience: "test-audience",
	})
	result := decodeResp[enclave.AttestationResponse](t, resp)
	if result.Token != "dev-ok" {
		t.Fatalf("expected dev-ok, got %q", result.Token)
	}
	if len(result.PublicKey) == 0 {
		t.Fatal("expected non-empty public key")
	}
}

func TestSessionEndpoint(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	// Attest first.
	postJSON(t, server, "/attest", enclave.AttestationRequest{Nonce: []byte("n"), Audience: "a"})

	hostKey, _ := ecdh.P256().GenerateKey(rand.Reader)
	resp := postJSON(t, server, "/session", enclave.SessionRequest{
		PublicKey: hostKey.PublicKey().Bytes(),
	})
	result := decodeResp[enclave.SessionResponse](t, resp)
	if result.SessionID == "" {
		t.Fatal("empty session ID")
	}
	if result.ExpiresAt == "" {
		t.Fatal("empty expires_at")
	}
}

func TestSessionBadPublicKey(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	postJSON(t, server, "/attest", enclave.AttestationRequest{Nonce: []byte("n"), Audience: "a"})

	resp := postJSON(t, server, "/session", enclave.SessionRequest{
		PublicKey: []byte("not-a-valid-key"),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestExecuteBadDecryption(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	resp := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		UserID:              "user-1",
		ConnectorID:         "test/stub",
		EncryptedCredential: []byte("not-encrypted"),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEscrowStoreWithoutKEK(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	resp := postJSON(t, server, "/escrow", enclave.EscrowStoreRequest{
		UserID:  "no-kek-user",
		GrantID: "g1",
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEscrowStoreBadDecryption(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	resp := postJSON(t, server, "/escrow", enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "g1",
		EncryptedCredential: []byte("bad"),
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEscrowStoreBadExpiry(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("cred"), userKEK)
	resp := postJSON(t, server, "/escrow", enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "g1",
		EncryptedCredential: encrypted,
		ExpiresAt:           "not-a-date",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEscrowRevokeNotFoundViaHTTP(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp := postJSON(t, server, "/escrow/revoke", enclave.EscrowRevokeRequest{
		EscrowID: "nonexistent",
		GrantID:  "g1",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHealthWithActiveSession(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["session_active"] != true {
		t.Fatalf("expected session_active true, got %v", result["session_active"])
	}
}

func TestAttestBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/attest", "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("POST /attest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestSessionBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/session", "application/json", bytes.NewReader([]byte("{")))
	if err != nil {
		t.Fatalf("POST /session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestExecuteBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/execute", "application/json", bytes.NewReader([]byte("bad")))
	if err != nil {
		t.Fatalf("POST /execute: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestEscrowStoreBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/escrow", "application/json", bytes.NewReader([]byte("[")))
	if err != nil {
		t.Fatalf("POST /escrow: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestEscrowRevokeBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/escrow/revoke", "application/json", bytes.NewReader([]byte("}")))
	if err != nil {
		t.Fatalf("POST /escrow/revoke: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestExecuteWithExpiredEscrow(t *testing.T) {
	server, srv := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("cred"), userKEK)

	// Store with past expiry.
	storeResp := postJSON(t, server, "/escrow", enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "g1",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
		ExpiresAt:           time.Now().Add(-time.Second).Format(time.RFC3339),
	})
	storeResult := decodeResp[enclave.EscrowStoreResponse](t, storeResp)

	// Execute with expired escrow.
	_ = srv // keep reference
	execResp := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		ConnectorID: "test/stub",
		EscrowID:    storeResult.EscrowID,
	})
	if execResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for expired escrow, got %d", execResp.StatusCode)
	}
	execResp.Body.Close()
}

func TestEscrowRetrieveEndpoint(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("my-token"), userKEK)
	storeResp := postJSON(t, server, "/escrow", enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "g1",
		EncryptedCredential: encrypted,
		CredentialType:      "oauth_token",
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	storeResult := decodeResp[enclave.EscrowStoreResponse](t, storeResp)

	retrieveResp := postJSON(t, server, "/escrow/retrieve", enclave.EscrowRetrieveRequest{
		EscrowID: storeResult.EscrowID,
	})
	if retrieveResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", retrieveResp.StatusCode)
	}
	result := decodeResp[enclave.EscrowRetrieveResponse](t, retrieveResp)
	if string(result.Credential) != "my-token" {
		t.Errorf("credential = %q, want my-token", result.Credential)
	}
}

func TestEscrowRetrieveEndpoint_NotFound(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp := postJSON(t, server, "/escrow/retrieve", enclave.EscrowRetrieveRequest{
		EscrowID: "nonexistent",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTransmitKEKEndpoint(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)

	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)
}

func TestTransmitKEKWithoutSession(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp := postJSON(t, server, "/kek", enclave.TransmitKEKRequest{
		UserID:       "user-1",
		EncryptedKEK: []byte("data"),
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTransmitKEKBadDecryption(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	resp := postJSON(t, server, "/kek", enclave.TransmitKEKRequest{
		UserID:       "user-1",
		EncryptedKEK: []byte("not-encrypted"),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTransmitKEKBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/kek", "application/json", bytes.NewReader([]byte("bad")))
	if err != nil {
		t.Fatalf("POST /kek: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestOAuthExchangeEndpoint(t *testing.T) {
	// Mock token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "at-123",
			"refresh_token": "rt-456",
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	// Mock userinfo endpoint.
	userinfoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"email": "test@example.com"})
	}))
	defer userinfoServer.Close()

	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	resp := postJSON(t, server, "/oauth/exchange", enclave.OAuthExchangeRequest{
		UserID:           "user-1",
		Provider:         "test",
		Code:             "auth-code",
		RedirectURI:      "http://localhost/cb",
		ClientID:         "cid",
		ClientSecret:     "csec",
		TokenEndpoint:    tokenServer.URL,
		UserInfoEndpoint: userinfoServer.URL,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	result := decodeResp[enclave.OAuthExchangeResponse](t, resp)
	if result.Email != "test@example.com" {
		t.Fatalf("expected test@example.com, got %q", result.Email)
	}
	if result.TokenType != "Bearer" {
		t.Fatalf("expected Bearer, got %q", result.TokenType)
	}
	if len(result.EncryptedToken) == 0 {
		t.Fatal("expected non-empty encrypted token")
	}

	// Verify the token can be decrypted with the user's KEK.
	tokenJSON, err := crypto.Decrypt(result.EncryptedToken, userKEK)
	if err != nil {
		t.Fatalf("decrypting token: %v", err)
	}
	var tokenData map[string]string
	if err := json.Unmarshal(tokenJSON, &tokenData); err != nil {
		t.Fatalf("unmarshaling token: %v", err)
	}
	if tokenData["refresh_token"] != "rt-456" {
		t.Fatalf("expected rt-456, got %q", tokenData["refresh_token"])
	}
}

func TestOAuthExchangeNoRefreshToken(t *testing.T) {
	// Slack-like provider: returns access_token but no refresh_token.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "xoxu-slack-token",
			"token_type":   "bearer",
		})
	}))
	defer tokenServer.Close()

	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	resp := postJSON(t, server, "/oauth/exchange", enclave.OAuthExchangeRequest{
		UserID:        "user-1",
		Provider:      "slack",
		Code:          "auth-code",
		RedirectURI:   "http://localhost/cb",
		ClientID:      "cid",
		ClientSecret:  "csec",
		TokenEndpoint: tokenServer.URL,
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	result := decodeResp[enclave.OAuthExchangeResponse](t, resp)
	if result.TokenType != "bearer" {
		t.Fatalf("expected bearer, got %q", result.TokenType)
	}
	if len(result.EncryptedToken) == 0 {
		t.Fatal("expected non-empty encrypted token")
	}
}

func TestOAuthExchangeSlackNestedUserToken(t *testing.T) {
	// Slack oauth.v2.access returns the user token nested under authed_user,
	// not at the top level. The enclave must extract the user token.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"token_type":  "bot",
			"bot_user_id": "U_BOT",
			"authed_user": map[string]string{
				"id":           "U_USER",
				"access_token": "xoxp-user-token",
				"scope":        "channels:history,channels:read",
				"token_type":   "user",
			},
			"team": map[string]string{
				"id":   "T001",
				"name": "Test Workspace",
			},
		})
	}))
	defer tokenServer.Close()

	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	resp := postJSON(t, server, "/oauth/exchange", enclave.OAuthExchangeRequest{
		UserID:        "user-1",
		Provider:      "slack",
		Code:          "auth-code",
		RedirectURI:   "http://localhost/cb",
		ClientID:      "cid",
		ClientSecret:  "csec",
		TokenEndpoint: tokenServer.URL,
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	result := decodeResp[enclave.OAuthExchangeResponse](t, resp)
	if result.TokenType != "user" {
		t.Fatalf("expected token_type 'user', got %q", result.TokenType)
	}

	// Decrypt and verify the stored token is the normalized user token.
	tokenJSON, err := crypto.Decrypt(result.EncryptedToken, userKEK)
	if err != nil {
		t.Fatalf("decrypting token: %v", err)
	}
	var tokenData map[string]string
	if err := json.Unmarshal(tokenJSON, &tokenData); err != nil {
		t.Fatalf("unmarshaling token: %v", err)
	}
	if tokenData["access_token"] != "xoxp-user-token" {
		t.Fatalf("expected xoxp-user-token, got %q", tokenData["access_token"])
	}
	if tokenData["user_id"] != "U_USER" {
		t.Fatalf("expected U_USER, got %q", tokenData["user_id"])
	}
	if tokenData["team_id"] != "T001" {
		t.Fatalf("expected T001, got %q", tokenData["team_id"])
	}

	// Regression: ExternalUserID and ExternalTeamID must be populated in the
	// response so the host can store them on the connected account. Without
	// these, resolveAileronUserBySlack fails with "Could not find your
	// Aileron account" on every slash command, message shortcut, and agent DM.
	if result.ExternalUserID != "U_USER" {
		t.Fatalf("expected ExternalUserID 'U_USER', got %q", result.ExternalUserID)
	}
	if result.ExternalTeamID != "T001" {
		t.Fatalf("expected ExternalTeamID 'T001', got %q", result.ExternalTeamID)
	}
}

func TestOAuthExchangeStandardProvider_NoExternalIDs(t *testing.T) {
	// Standard OAuth providers (Google, GitHub) should NOT populate
	// ExternalUserID/ExternalTeamID — those are Slack-specific.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ya29-google-token",
			"token_type":    "Bearer",
			"refresh_token": "1//google-refresh",
		})
	}))
	defer tokenServer.Close()

	userinfoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"email": "alice@example.com"})
	}))
	defer userinfoServer.Close()

	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	resp := postJSON(t, server, "/oauth/exchange", enclave.OAuthExchangeRequest{
		UserID:           "user-1",
		Provider:         "google",
		Code:             "auth-code",
		RedirectURI:      "http://localhost/cb",
		ClientID:         "cid",
		ClientSecret:     "csec",
		TokenEndpoint:    tokenServer.URL,
		UserInfoEndpoint: userinfoServer.URL,
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	result := decodeResp[enclave.OAuthExchangeResponse](t, resp)
	if result.Email != "alice@example.com" {
		t.Fatalf("expected email 'alice@example.com', got %q", result.Email)
	}
	if result.ExternalUserID != "" {
		t.Fatalf("expected empty ExternalUserID for Google, got %q", result.ExternalUserID)
	}
	if result.ExternalTeamID != "" {
		t.Fatalf("expected empty ExternalTeamID for Google, got %q", result.ExternalTeamID)
	}
}

func TestOAuthExchangeAcceptJSON(t *testing.T) {
	// GitHub-like provider: returns JSON only when Accept header is set.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		if accept != "application/json" {
			// GitHub returns form-encoded without Accept header.
			w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
			w.Write([]byte("access_token=gho_xxx&token_type=bearer"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "gho_xxx",
			"token_type":   "bearer",
		})
	}))
	defer tokenServer.Close()

	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	resp := postJSON(t, server, "/oauth/exchange", enclave.OAuthExchangeRequest{
		UserID:        "user-1",
		Provider:      "github",
		Code:          "auth-code",
		RedirectURI:   "http://localhost/cb",
		ClientID:      "cid",
		ClientSecret:  "csec",
		TokenEndpoint: tokenServer.URL,
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	result := decodeResp[enclave.OAuthExchangeResponse](t, resp)
	if len(result.EncryptedToken) == 0 {
		t.Fatal("expected non-empty encrypted token")
	}

	// Verify decrypted token has the right access_token.
	tokenJSON, err := crypto.Decrypt(result.EncryptedToken, userKEK)
	if err != nil {
		t.Fatalf("decrypting token: %v", err)
	}
	var tokenData map[string]string
	if err := json.Unmarshal(tokenJSON, &tokenData); err != nil {
		t.Fatalf("unmarshaling token: %v", err)
	}
	if tokenData["access_token"] != "gho_xxx" {
		t.Fatalf("expected gho_xxx, got %q", tokenData["access_token"])
	}
}

func TestOAuthExchangeWithoutKEK(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	resp := postJSON(t, server, "/oauth/exchange", enclave.OAuthExchangeRequest{
		UserID: "no-kek",
		Code:   "code",
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestOAuthExchangeTokenEndpointError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer tokenServer.Close()

	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	resp := postJSON(t, server, "/oauth/exchange", enclave.OAuthExchangeRequest{
		UserID:        "user-1",
		Code:          "bad-code",
		TokenEndpoint: tokenServer.URL,
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestOAuthExchangeBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/oauth/exchange", "application/json", bytes.NewReader([]byte("bad")))
	if err != nil {
		t.Fatalf("POST /oauth/exchange: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestEscrowListEndpoint(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	// Empty list.
	resp, err := http.Post(server.URL+"/escrow/list", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST /escrow/list: %v", err)
	}
	result := decodeResp[enclave.EscrowListResponse](t, resp)
	if len(result.Entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(result.Entries))
	}

	// Store some entries, then list.
	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("cred-1"), userKEK)
	storeResp := postJSON(t, server, "/escrow", enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "g1",
		EncryptedCredential: encrypted,
		CredentialType:      "oauth_token",
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	storeResult := decodeResp[enclave.EscrowStoreResponse](t, storeResp)

	resp2, err := http.Post(server.URL+"/escrow/list", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST /escrow/list: %v", err)
	}
	result2 := decodeResp[enclave.EscrowListResponse](t, resp2)
	if len(result2.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result2.Entries))
	}
	if result2.Entries[0].EscrowID != storeResult.EscrowID {
		t.Fatalf("expected %q, got %q", storeResult.EscrowID, result2.Entries[0].EscrowID)
	}
	if result2.Entries[0].GrantID != "g1" {
		t.Fatalf("expected grant g1, got %q", result2.Entries[0].GrantID)
	}
}

func TestEscrowRetrieveBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/escrow/retrieve", "application/json", bytes.NewReader([]byte("bad")))
	if err != nil {
		t.Fatalf("POST /escrow/retrieve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestResolveConnectorID(t *testing.T) {
	tests := []struct {
		input      string
		wantType   string
		wantProv   string
	}{
		{"payments/stripe", "payments", "stripe"},
		{"git/github", "git", "github"},
		{"single", "single", ""},
	}
	for _, tt := range tests {
		connType, connProv := resolveConnectorID(tt.input)
		if connType != tt.wantType || connProv != tt.wantProv {
			t.Errorf("resolveConnectorID(%q) = (%q, %q), want (%q, %q)",
				tt.input, connType, connProv, tt.wantType, tt.wantProv)
		}
	}
}
