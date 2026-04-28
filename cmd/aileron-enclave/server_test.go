package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/source"
)

type testSessionAuth struct {
	sessionID string
	authKey   []byte
	grantKey  []byte
}

var testSessions sync.Map

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

// stubSourceConnector is a minimal source connector for testing.
type stubSourceConnector struct {
	result map[string]any
	err    error
}

func (s *stubSourceConnector) Provider() string { return "test_provider" }
func (s *stubSourceConnector) Tools() []source.ToolDefinition {
	return []source.ToolDefinition{
		{Name: "test_search", Description: "Test search tool",
			Parameters: []source.ToolParam{{Name: "query", Type: "string", Required: true}}},
	}
}
func (s *stubSourceConnector) Execute(_ context.Context, _ string, _ map[string]any, _ []byte) (map[string]any, error) {
	return s.result, s.err
}

func setupTestEnclaveServer(t *testing.T) (*httptest.Server, *enclaveServer) {
	return setupTestEnclaveServerWithSource(t, &stubSourceConnector{
		result: map[string]any{"messages": []string{"hello"}},
	})
}

func setupTestEnclaveServerWithSource(t *testing.T, sc source.SourceConnector) (*httptest.Server, *enclaveServer) {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	registry := connector.NewRegistry()
	registry.Register(context.Background(), &stubConnector{})
	sourceReg := source.NewRegistry()
	if sc != nil {
		sourceReg.Register(sc)
	}
	srv, err := newEnclaveServer(log, registry, sourceReg, "local", "")
	if err != nil {
		t.Fatalf("newEnclaveServer: %v", err)
	}
	mux := http.NewServeMux()
	srv.registerRoutes(mux)
	return httptest.NewServer(mux), srv
}

func postJSON(t *testing.T, server *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	if sessAny, ok := testSessions.Load(server.URL); ok {
		body = attachTestCapability(t, path, body, sessAny.(testSessionAuth))
	}
	payload, _ := json.Marshal(body)
	return postRaw(t, server, path, payload, true)
}

func postJSONWithoutSession(t *testing.T, server *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	return postJSONWithSessionID(t, server, path, body, "")
}

func postJSONWithSessionID(t *testing.T, server *httptest.Server, path string, body any, sessionID string) *http.Response {
	t.Helper()
	payload, _ := json.Marshal(body)
	return postRawWithExplicitSessionID(t, server, path, payload, sessionID)
}

func postRawWithSession(t *testing.T, server *httptest.Server, path string, body []byte) *http.Response {
	t.Helper()
	return postRaw(t, server, path, body, true)
}

func postRaw(t *testing.T, server *httptest.Server, path string, body []byte, sign bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("creating POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessAny, ok := testSessions.Load(server.URL); ok {
		sess := sessAny.(testSessionAuth)
		req.Header.Set(enclave.HeaderSessionID, sess.sessionID)
		if sign {
			signTestRequest(t, req, path, body, sess)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func postRawWithExplicitSessionID(t *testing.T, server *httptest.Server, path string, body []byte, sessionID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("creating POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(enclave.HeaderSessionID, sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func signTestRequest(t *testing.T, req *http.Request, path string, body []byte, sess testSessionAuth) string {
	t.Helper()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatalf("generating nonce: %v", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	signTestRequestWith(t, req, path, body, sess, timestamp, nonce)
	return nonce
}

func signTestRequestWith(t *testing.T, req *http.Request, path string, body []byte, sess testSessionAuth, timestamp, nonce string) {
	t.Helper()
	req.Header.Set(enclave.HeaderRequestTimestamp, timestamp)
	req.Header.Set(enclave.HeaderRequestNonce, nonce)
	req.Header.Set(enclave.HeaderRequestMAC, enclave.RequestMAC(sess.authKey, req.Method, path, body, sess.sessionID, timestamp, nonce))
}

func attachTestCapability(t *testing.T, path string, body any, sess testSessionAuth) any {
	t.Helper()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatalf("generating capability nonce: %v", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	switch req := body.(type) {
	case enclave.ExecuteRequest:
		capability, err := enclave.SignGrantCapability(sess.authKey, enclave.ExecuteGrantCapability(req, nonce))
		if err != nil {
			t.Fatalf("signing execute capability: %v", err)
		}
		req.Capability = capability
		if req.IssuedCapability == nil {
			issued, err := enclave.SignGrantCapability(sess.grantKey, enclave.IssuedExecuteGrantCapability(req, nonce+"-issued", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
			if err != nil {
				t.Fatalf("signing issued execute capability: %v", err)
			}
			req.IssuedCapability = issued
		}
		return req
	case enclave.EscrowStoreRequest:
		capability, err := enclave.SignGrantCapability(sess.authKey, enclave.EscrowStoreGrantCapability(req, nonce))
		if err != nil {
			t.Fatalf("signing escrow capability: %v", err)
		}
		req.Capability = capability
		if req.IssuedCapability == nil {
			issued, err := enclave.SignGrantCapability(sess.grantKey, enclave.EscrowStoreGrantCapability(req, nonce+"-issued"))
			if err != nil {
				t.Fatalf("signing issued escrow capability: %v", err)
			}
			req.IssuedCapability = issued
		}
		return req
	case enclave.SourceExecuteRequest:
		capability, err := enclave.SignGrantCapability(sess.authKey, enclave.SourceExecuteGrantCapability(req, nonce))
		if err != nil {
			t.Fatalf("signing source capability: %v", err)
		}
		req.Capability = capability
		return req
	default:
		return body
	}
}

func attachTestRequestCapabilityOnly(t *testing.T, body any, sess testSessionAuth) any {
	t.Helper()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatalf("generating capability nonce: %v", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	switch req := body.(type) {
	case enclave.ExecuteRequest:
		capability, err := enclave.SignGrantCapability(sess.authKey, enclave.ExecuteGrantCapability(req, nonce))
		if err != nil {
			t.Fatalf("signing execute capability: %v", err)
		}
		req.Capability = capability
		return req
	case enclave.EscrowStoreRequest:
		capability, err := enclave.SignGrantCapability(sess.authKey, enclave.EscrowStoreGrantCapability(req, nonce))
		if err != nil {
			t.Fatalf("signing escrow capability: %v", err)
		}
		req.Capability = capability
		return req
	default:
		return body
	}
}

func testSession(t *testing.T, server *httptest.Server) testSessionAuth {
	t.Helper()
	sessAny, ok := testSessions.Load(server.URL)
	if !ok {
		t.Fatal("test session not established")
	}
	return sessAny.(testSessionAuth)
}

func signIssuedExecuteCapability(t *testing.T, sess testSessionAuth, req enclave.ExecuteRequest, expiresAt time.Time) *enclave.SignedGrantCapability {
	t.Helper()
	capability, err := enclave.SignGrantCapability(sess.grantKey, enclave.IssuedExecuteGrantCapability(req, "issued-"+req.RequestID, expiresAt.UTC().Format(time.RFC3339)))
	if err != nil {
		t.Fatalf("signing issued execute capability: %v", err)
	}
	return capability
}

func signIssuedEscrowCapability(t *testing.T, sess testSessionAuth, req enclave.EscrowStoreRequest) *enclave.SignedGrantCapability {
	t.Helper()
	capability, err := enclave.SignGrantCapability(sess.grantKey, enclave.EscrowStoreGrantCapability(req, "issued-"+req.GrantID))
	if err != nil {
		t.Fatalf("signing issued escrow capability: %v", err)
	}
	return capability
}

func signedTestRequest(t *testing.T, server *httptest.Server, path string, body []byte, sess testSessionAuth) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("creating POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(enclave.HeaderSessionID, sess.sessionID)
	signTestRequest(t, req, path, body, sess)
	return req
}

func mustJSON(t *testing.T, body any) []byte {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling JSON: %v", err)
	}
	return payload
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

func validEscrowStoreRequest(encrypted []byte) enclave.EscrowStoreRequest {
	return enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "grant-1",
		EnforceGrantID:      true,
		VaultPath:           "connected-accounts/user-1/gmail",
		Provider:            "gmail",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
		ActionTypes:         []string{"test.action"},
		SourceTools:         []string{"test_search"},
	}
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
	if len(sessResult.RequestAuthKey) == 0 {
		t.Fatal("empty request auth key")
	}
	if len(sessResult.GrantCapabilityKey) == 0 {
		t.Fatal("empty grant capability key")
	}
	testSessions.Store(server.URL, testSessionAuth{
		sessionID: sessResult.SessionID,
		authKey:   sessResult.RequestAuthKey,
		grantKey:  sessResult.GrantCapabilityKey,
	})
	t.Cleanup(func() {
		testSessions.Delete(server.URL)
	})

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

func TestExecuteRequiresIssuedCapability(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("cred"), userKEK)
	req := enclave.ExecuteRequest{
		RequestID:           "exec-missing-issued",
		UserID:              "user-1",
		GrantID:             "grant-1",
		ActionType:          "test.action",
		ConnectorID:         "test/stub",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
	}
	req = attachTestRequestCapabilityOnly(t, req, testSession(t, server)).(enclave.ExecuteRequest)

	resp := postRaw(t, server, "/execute", mustJSON(t, req), true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for missing issued capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestExecuteRejectsIssuedCapabilityScopeMismatch(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("cred"), userKEK)
	req := enclave.ExecuteRequest{
		RequestID:           "exec-scope-mismatch",
		UserID:              "user-1",
		GrantID:             "grant-1",
		ActionType:          "test.action",
		ConnectorID:         "test/stub",
		VaultPath:           "connected-accounts/user-1/gmail",
		Provider:            "gmail",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
	}
	issuedReq := req
	issuedReq.Provider = "drive"
	req.IssuedCapability = signIssuedExecuteCapability(t, testSession(t, server), issuedReq, time.Now().Add(time.Hour))

	resp := postJSON(t, server, "/execute", req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for issued capability scope mismatch, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestExecuteRejectsReplayedIssuedCapability(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("cred"), userKEK)
	req := enclave.ExecuteRequest{
		RequestID:           "exec-replay",
		UserID:              "user-1",
		GrantID:             "grant-1",
		ActionType:          "test.action",
		ConnectorID:         "test/stub",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
	}
	req.IssuedCapability = signIssuedExecuteCapability(t, testSession(t, server), req, time.Now().Add(time.Hour))

	resp := postJSON(t, server, "/execute", req)
	result := decodeResp[enclave.ExecuteResponse](t, resp)
	if result.Status != "succeeded" {
		t.Fatalf("expected first execution succeeded, got %q", result.Status)
	}

	resp = postJSON(t, server, "/execute", req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for replayed issued capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVerifyCapabilityExpiryRejectsMissingAndInvalidExpiry(t *testing.T) {
	tests := []struct {
		name       string
		capability *enclave.SignedGrantCapability
	}{
		{
			name:       "missing",
			capability: &enclave.SignedGrantCapability{Capability: enclave.GrantCapability{Nonce: "nonce"}, Signature: "sig"},
		},
		{
			name:       "invalid",
			capability: &enclave.SignedGrantCapability{Capability: enclave.GrantCapability{Nonce: "nonce", ExpiresAt: "not-a-date"}, Signature: "sig"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := verifyCapabilityExpiry(tt.capability); err == nil {
				t.Fatal("expected expiry validation error")
			}
		})
	}
}

func TestCapabilityReplayKeyHandlesNil(t *testing.T) {
	if got := capabilityReplayKey(nil); got != "" {
		t.Fatalf("expected empty replay key for nil capability, got %q", got)
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
	storeResp := postJSON(t, server, "/escrow", validEscrowStoreRequest(encrypted))
	storeResult := decodeResp[enclave.EscrowStoreResponse](t, storeResp)
	if storeResult.EscrowID == "" {
		t.Fatal("empty escrow ID")
	}

	// Execute with escrow.
	execResp := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		RequestID:      "exec-escrow",
		UserID:         "user-1",
		GrantID:        "grant-1",
		ActionType:     "test.action",
		ConnectorID:    "test/stub",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "api_key",
		EscrowID:       storeResult.EscrowID,
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
		UserID:         "user-1",
		GrantID:        "grant-1",
		ActionType:     "test.action",
		ConnectorID:    "test/stub",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "api_key",
		EscrowID:       storeResult.EscrowID,
	})
	if execResp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after revoke, got %d", execResp2.StatusCode)
	}
	execResp2.Body.Close()
}

func TestEscrowStoreRejectsIssuedCapabilityScopeMismatch(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("escrowed-cred"), userKEK)
	req := validEscrowStoreRequest(encrypted)
	issuedReq := req
	issuedReq.VaultPath = "connected-accounts/user-1/drive"
	req.IssuedCapability = signIssuedEscrowCapability(t, testSession(t, server), issuedReq)

	resp := postJSON(t, server, "/escrow", req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for escrow issued capability scope mismatch, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestExecuteWithEscrowRejectsScopeMismatch(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("escrowed-cred"), userKEK)
	storeResp := postJSON(t, server, "/escrow", validEscrowStoreRequest(encrypted))
	storeResult := decodeResp[enclave.EscrowStoreResponse](t, storeResp)

	execResp := postJSON(t, server, "/execute", enclave.ExecuteRequest{
		RequestID:      "exec-escrow",
		UserID:         "user-2",
		GrantID:        "grant-1",
		ActionType:     "test.action",
		ConnectorID:    "test/stub",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "api_key",
		EscrowID:       storeResult.EscrowID,
	})
	if execResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for scope mismatch, got %d", execResp.StatusCode)
	}
	execResp.Body.Close()
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
	grantKey := []byte("01234567890123456789012345678901")
	resp := postJSON(t, server, "/session", enclave.SessionRequest{
		PublicKey:          hostKey.PublicKey().Bytes(),
		GrantCapabilityKey: grantKey,
	})
	result := decodeResp[enclave.SessionResponse](t, resp)
	if result.SessionID == "" {
		t.Fatal("empty session ID")
	}
	if result.ExpiresAt == "" {
		t.Fatal("empty expires_at")
	}
	if len(result.GrantCapabilityKey) == 0 {
		t.Fatal("empty grant capability key")
	}
	if !bytes.Equal(result.GrantCapabilityKey, grantKey) {
		t.Fatal("expected enclave to use provided grant capability key")
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
		UserID:         "no-kek-user",
		GrantID:        "g1",
		VaultPath:      "connected-accounts/no-kek-user/gmail",
		Provider:       "gmail",
		CredentialType: "api_key",
		ExpiresAt:      time.Now().Add(time.Hour).Format(time.RFC3339),
		ActionTypes:    []string{"test.action"},
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEscrowStoreRequiresScopeFields(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	base := validEscrowStoreRequest([]byte("encrypted"))
	tests := []struct {
		name   string
		mutate func(*enclave.EscrowStoreRequest)
	}{
		{"user_id", func(r *enclave.EscrowStoreRequest) { r.UserID = "" }},
		{"grant_id", func(r *enclave.EscrowStoreRequest) { r.GrantID = "" }},
		{"vault_path", func(r *enclave.EscrowStoreRequest) { r.VaultPath = "" }},
		{"provider", func(r *enclave.EscrowStoreRequest) { r.Provider = "" }},
		{"credential_type", func(r *enclave.EscrowStoreRequest) { r.CredentialType = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			resp := postJSON(t, server, "/escrow", req)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
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
		VaultPath:           "connected-accounts/user-1/gmail",
		Provider:            "gmail",
		EncryptedCredential: []byte("bad"),
		CredentialType:      "api_key",
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
		VaultPath:           "connected-accounts/user-1/gmail",
		Provider:            "gmail",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
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

	establishTestSession(t, server)

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

	establishTestSession(t, server)

	resp := postRawWithSession(t, server, "/execute", []byte("bad"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestEscrowStoreBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	resp := postRawWithSession(t, server, "/escrow", []byte("["))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestEscrowRevokeBadJSON(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	resp := postRawWithSession(t, server, "/escrow/revoke", []byte("}"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestExecuteWithExpiredEscrow(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	sessionKey := establishTestSession(t, server)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitTestKEK(t, server, sessionKey, "user-1", userKEK)

	encrypted, _ := crypto.Encrypt([]byte("cred"), userKEK)

	// Store with past expiry.
	expiredReq := validEscrowStoreRequest(encrypted)
	expiredReq.GrantID = "g1"
	expiredReq.ExpiresAt = time.Now().Add(-time.Second).Format(time.RFC3339)
	storeResp := postJSON(t, server, "/escrow", expiredReq)
	if storeResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for expired issued capability, got %d", storeResp.StatusCode)
	}
	storeResp.Body.Close()
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

func TestSensitiveEndpointsRequireMatchingSession(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	body := enclave.ExecuteRequest{
		UserID:              "user-1",
		ConnectorID:         "test/stub",
		EncryptedCredential: []byte("data"),
	}

	missing := postJSONWithoutSession(t, server, "/execute", body)
	if missing.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing session, got %d", missing.StatusCode)
	}
	missing.Body.Close()

	wrong := postJSONWithSessionID(t, server, "/execute", body, "wrong-session")
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong session, got %d", wrong.StatusCode)
	}
	wrong.Body.Close()

	matching := postJSON(t, server, "/execute", body)
	if matching.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 after valid session reaches KEK check, got %d", matching.StatusCode)
	}
	matching.Body.Close()
}

func TestSensitiveEndpointsRequireRequestAuthentication(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	body := enclave.ExecuteRequest{
		UserID:              "user-1",
		ConnectorID:         "test/stub",
		EncryptedCredential: []byte("data"),
	}

	unsigned := postRaw(t, server, "/execute", mustJSON(t, body), false)
	if unsigned.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unsigned request, got %d", unsigned.StatusCode)
	}
	unsigned.Body.Close()
}

func TestExecuteRejectsMissingGrantCapability(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	body := mustJSON(t, enclave.ExecuteRequest{
		UserID:              "user-1",
		ConnectorID:         "test/stub",
		EncryptedCredential: []byte("data"),
	})

	resp := postRaw(t, server, "/execute", body, true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for missing capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSensitiveEndpointsRejectTamperedRequest(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	sessAny, _ := testSessions.Load(server.URL)
	sess := sessAny.(testSessionAuth)
	originalReq := attachTestCapability(t, "/execute", enclave.ExecuteRequest{
		UserID:              "user-1",
		ConnectorID:         "test/stub",
		EncryptedCredential: []byte("data"),
	}, sess).(enclave.ExecuteRequest)
	tamperedReq := originalReq
	tamperedReq.UserID = "user-2"
	originalBody := mustJSON(t, originalReq)
	tamperedBody := mustJSON(t, tamperedReq)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/execute", bytes.NewReader(tamperedBody))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(enclave.HeaderSessionID, sess.sessionID)
	signTestRequest(t, req, "/execute", originalBody, sess)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /execute: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered body, got %d", resp.StatusCode)
	}
}

func TestSensitiveEndpointsRejectReplay(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	sessAny, _ := testSessions.Load(server.URL)
	sess := sessAny.(testSessionAuth)
	body := mustJSON(t, attachTestCapability(t, "/execute", enclave.ExecuteRequest{
		UserID:              "user-1",
		ConnectorID:         "test/stub",
		EncryptedCredential: []byte("data"),
	}, sess))
	req1 := signedTestRequest(t, server, "/execute", body, sess)

	first, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("POST /execute: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected first request to pass auth and fail at KEK check with 412, got %d", first.StatusCode)
	}

	req2 := signedTestRequest(t, server, "/execute", body, sess)
	req2.Header.Set(enclave.HeaderRequestTimestamp, req1.Header.Get(enclave.HeaderRequestTimestamp))
	req2.Header.Set(enclave.HeaderRequestNonce, req1.Header.Get(enclave.HeaderRequestNonce))
	req2.Header.Set(enclave.HeaderRequestMAC, req1.Header.Get(enclave.HeaderRequestMAC))
	second, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST /execute replay: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for replay, got %d", second.StatusCode)
	}
}

func TestSensitiveEndpointsRejectInvalidAndStaleTimestamps(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	sessAny, _ := testSessions.Load(server.URL)
	sess := sessAny.(testSessionAuth)
	body := mustJSON(t, attachTestCapability(t, "/execute", enclave.ExecuteRequest{
		UserID:              "user-1",
		ConnectorID:         "test/stub",
		EncryptedCredential: []byte("data"),
	}, sess))

	invalid := signedTestRequest(t, server, "/execute", body, sess)
	invalid.Header.Set(enclave.HeaderRequestTimestamp, "not-a-time")
	resp, err := http.DefaultClient.Do(invalid)
	if err != nil {
		t.Fatalf("POST /execute invalid timestamp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid timestamp, got %d", resp.StatusCode)
	}

	stale := signedTestRequest(t, server, "/execute", body, sess)
	staleTime := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	signTestRequestWith(t, stale, "/execute", body, sess, staleTime, "stale-nonce")
	resp, err = http.DefaultClient.Do(stale)
	if err != nil {
		t.Fatalf("POST /execute stale timestamp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for stale timestamp, got %d", resp.StatusCode)
	}
}

func TestPruneSeenNonces(t *testing.T) {
	now := time.Now()
	seen := map[string]time.Time{
		"old":    now.Add(-10 * time.Minute),
		"recent": now,
	}

	pruneSeenNonces(seen, now.Add(-5*time.Minute))

	if _, ok := seen["old"]; ok {
		t.Fatal("expected old nonce to be pruned")
	}
	if _, ok := seen["recent"]; !ok {
		t.Fatal("expected recent nonce to remain")
	}
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

	establishTestSession(t, server)

	resp := postRawWithSession(t, server, "/kek", []byte("bad"))
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

	establishTestSession(t, server)

	resp := postRawWithSession(t, server, "/oauth/exchange", []byte("bad"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestEscrowListEndpoint(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	// Empty list.
	resp := postRawWithSession(t, server, "/escrow/list", []byte("{}"))
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
	listReq := validEscrowStoreRequest(encrypted)
	listReq.GrantID = "g1"
	listReq.CredentialType = "oauth_token"
	storeResp := postJSON(t, server, "/escrow", listReq)
	storeResult := decodeResp[enclave.EscrowStoreResponse](t, storeResp)

	resp2 := postRawWithSession(t, server, "/escrow/list", []byte("{}"))
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

func TestResolveConnectorID(t *testing.T) {
	tests := []struct {
		input    string
		wantType string
		wantProv string
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

func TestSourceExecute_HappyPath(t *testing.T) {
	server, srv := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	// Store a credential in escrow.
	escrowReq := testEscrowRequest("g1", "oauth2", nil)
	escrowID := srv.escrow.Store(escrowReq, []byte(`{"access_token":"test"}`), time.Now().Add(time.Hour))

	resp := postJSON(t, server, "/source/execute", enclave.SourceExecuteRequest{
		EscrowID:  escrowID,
		UserID:    "user-1",
		VaultPath: "connected-accounts/user-1/gmail",
		Provider:  "gmail",
		Tool:      "test_search",
		Params:    map[string]any{"query": "hello"},
	})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	result := decodeResp[enclave.SourceExecuteResponse](t, resp)
	if result.Error != "" {
		t.Errorf("unexpected error: %s", result.Error)
	}
	if result.Result == nil {
		t.Fatal("expected result, got nil")
	}
}

func TestSourceExecute_EscrowNotFound(t *testing.T) {
	server, _ := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	resp := postJSON(t, server, "/source/execute", enclave.SourceExecuteRequest{
		EscrowID: "esc_nonexistent",
		Tool:     "test_search",
		Params:   map[string]any{"query": "hello"},
	})
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestSourceExecute_RejectsScopeMismatch(t *testing.T) {
	server, srv := setupTestEnclaveServer(t)
	defer server.Close()

	establishTestSession(t, server)

	escrowReq := testEscrowRequest("g1", "oauth2", nil)
	escrowID := srv.escrow.Store(escrowReq, []byte(`{"access_token":"test"}`), time.Now().Add(time.Hour))

	resp := postJSON(t, server, "/source/execute", enclave.SourceExecuteRequest{
		EscrowID:  escrowID,
		UserID:    "user-2",
		VaultPath: "connected-accounts/user-1/gmail",
		Provider:  "gmail",
		Tool:      "test_search",
		Params:    map[string]any{"query": "hello"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSourceExecute_ToolError(t *testing.T) {
	server, srv := setupTestEnclaveServerWithSource(t, &stubSourceConnector{
		err: fmt.Errorf("gmail API error: 401 Invalid Credentials"),
	})
	defer server.Close()

	establishTestSession(t, server)

	escrowReq := testEscrowRequest("g1", "oauth2", nil)
	escrowID := srv.escrow.Store(escrowReq, []byte(`{"access_token":"test"}`), time.Now().Add(time.Hour))

	resp := postJSON(t, server, "/source/execute", enclave.SourceExecuteRequest{
		EscrowID:  escrowID,
		UserID:    "user-1",
		VaultPath: "connected-accounts/user-1/gmail",
		Provider:  "gmail",
		Tool:      "test_search",
		Params:    map[string]any{"query": "hello"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 (error in body), got %d", resp.StatusCode)
	}

	result := decodeResp[enclave.SourceExecuteResponse](t, resp)
	if result.Error == "" {
		t.Error("expected error in response")
	}
}
