package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	connectorpkg "github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/enclave"
)

// stubConnector implements connectorpkg.Connector for tests.
type stubConnector struct {
	result connectorpkg.ExecutionResult
	err    error
}

func (c *stubConnector) Type() string     { return "git" }
func (c *stubConnector) Provider() string { return "github" }
func (c *stubConnector) Execute(_ context.Context, _ connectorpkg.ExecutionRequest) (connectorpkg.ExecutionResult, error) {
	return c.result, c.err
}

// stubEnclaveClient implements enclave.Client for tests.
type stubEnclaveClient struct {
	resp enclave.ExecuteResponse
	err  error
}

func (e *stubEnclaveClient) Attest(_ context.Context, _ enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	return enclave.AttestationResponse{}, nil
}
func (e *stubEnclaveClient) EstablishSession(_ context.Context, _ enclave.SessionRequest) (enclave.SessionResponse, error) {
	return enclave.SessionResponse{}, nil
}
func (e *stubEnclaveClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{}, nil
}
func (e *stubEnclaveClient) OAuthExchange(_ context.Context, _ enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	return enclave.OAuthExchangeResponse{}, nil
}
func (e *stubEnclaveClient) Execute(_ context.Context, _ enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	return e.resp, e.err
}
func (e *stubEnclaveClient) EscrowStore(_ context.Context, _ enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	return enclave.EscrowStoreResponse{}, nil
}
func (e *stubEnclaveClient) EscrowList(_ context.Context) (enclave.EscrowListResponse, error) {
	return enclave.EscrowListResponse{}, nil
}
func (e *stubEnclaveClient) EscrowRevoke(_ context.Context, _ enclave.EscrowRevokeRequest) error {
	return nil
}
func (e *stubEnclaveClient) SourceExecute(_ context.Context, _ enclave.SourceExecuteRequest) (enclave.SourceExecuteResponse, error) {
	return enclave.SourceExecuteResponse{}, nil
}
func (e *stubEnclaveClient) Ready(_ context.Context) error { return nil }
func (e *stubEnclaveClient) Close() error                  { return nil }

func newExecutionServer() *apiServer {
	return &apiServer{
		log:        slog.Default(),
		registry:   connectorpkg.NewRegistry(),
		intents:    mem.NewIntentStore(),
		grants:     mem.NewGrantStore(),
		executions: mem.NewExecutionStore(),
		traces:     mem.NewTraceStore(),
		vault:      vault.NewMemVault(),
		newID:      func() string { return "test-id" },
	}
}

func seedGrantAndIntent(ctx context.Context, s *apiServer, grantID, intentID string) {
	s.intents.Create(ctx, api.IntentEnvelope{
		IntentId:    intentID,
		WorkspaceId: "ws_1",
		Status:      api.Approved,
		Action: api.ActionIntent{
			Type:    "git.push",
			Summary: "push to main",
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	s.grants.Create(ctx, api.ExecutionGrant{
		GrantId:   grantID,
		IntentId:  intentID,
		Status:    api.ExecutionGrantStatusActive,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	ciphertext, _ := crypto.Encrypt([]byte("ghp_token"), testKEK)
	s.vault.Put(ctx, "connectors/github/default", ciphertext, vault.Metadata{
		Type:   "api_key",
		Labels: map[string]string{vault.EncryptedLabel: "true"},
	})
}

func execRequest(grantID string) *http.Request {
	body := `{"grant_id":"` + grantID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/executions/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestRunExecution_DirectMode_Success(t *testing.T) {
	s := newExecutionServer()
	enableVaultEncryption(s, "usr_a")
	ctx := context.Background()

	conn := &stubConnector{
		result: connectorpkg.ExecutionResult{
			Status:     connectorpkg.ExecutionStatusSucceeded,
			Output:     map[string]any{"sha": "abc123"},
			ReceiptRef: "receipt_1",
		},
	}
	s.registry.Register(ctx, conn)

	seedGrantAndIntent(ctx, s, "grant_1", "int_1")

	w := httptest.NewRecorder()
	s.RunExecution(w, authedExecRequest("grant_1", userAClaims))

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp api.ExecutionRunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != api.Accepted {
		t.Errorf("expected status 'accepted', got %s", resp.Status)
	}
	if resp.ExecutionId == "" {
		t.Error("expected non-empty execution ID")
	}
}

func TestRunExecution_TEEMode_Success(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()

	conn := &stubConnector{}
	s.registry.Register(ctx, conn)
	s.enclaveClient = &stubEnclaveClient{
		resp: enclave.ExecuteResponse{
			Status:     "succeeded",
			Output:     map[string]any{"sha": "def456"},
			ReceiptRef: "receipt_2",
		},
	}

	seedGrantAndIntent(ctx, s, "grant_2", "int_2")

	w := httptest.NewRecorder()
	s.RunExecution(w, execRequest("grant_2"))

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp api.ExecutionRunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != api.Accepted {
		t.Errorf("expected status 'accepted', got %s", resp.Status)
	}
}

func TestRunExecution_InvalidBody(t *testing.T) {
	s := newExecutionServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/executions/run", strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.RunExecution(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestRunExecution_GrantNotFound(t *testing.T) {
	s := newExecutionServer()
	w := httptest.NewRecorder()
	s.RunExecution(w, execRequest("nonexistent"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestRunExecution_GrantInactive(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()
	s.grants.Create(ctx, api.ExecutionGrant{
		GrantId:   "grant_used",
		IntentId:  "int_1",
		Status:    api.ExecutionGrantStatusConsumed,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	w := httptest.NewRecorder()
	s.RunExecution(w, execRequest("grant_used"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestRunExecution_GrantExpired(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()
	s.intents.Create(ctx, api.IntentEnvelope{IntentId: "int_1", WorkspaceId: "ws"})
	s.grants.Create(ctx, api.ExecutionGrant{
		GrantId:   "grant_exp",
		IntentId:  "int_1",
		Status:    api.ExecutionGrantStatusActive,
		ExpiresAt: time.Now().UTC().Add(-time.Hour),
	})
	w := httptest.NewRecorder()
	s.RunExecution(w, execRequest("grant_exp"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestRunExecution_NoConnector(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()
	// Use "custom" action type which resolves to empty connector type/provider.
	s.intents.Create(ctx, api.IntentEnvelope{
		IntentId:    "int_nc",
		WorkspaceId: "ws",
		Status:      api.Approved,
		Action:      api.ActionIntent{Type: "custom", Summary: "do something"},
	})
	s.grants.Create(ctx, api.ExecutionGrant{
		GrantId:   "grant_nc",
		IntentId:  "int_nc",
		Status:    api.ExecutionGrantStatusActive,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	w := httptest.NewRecorder()
	s.RunExecution(w, execRequest("grant_nc"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRunExecution_VaultError(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()

	conn := &stubConnector{}
	s.registry.Register(ctx, conn)

	s.intents.Create(ctx, api.IntentEnvelope{
		IntentId:    "int_ve",
		WorkspaceId: "ws",
		Status:      api.Approved,
		Action:      api.ActionIntent{Type: "git.push", Summary: "push"},
	})
	s.grants.Create(ctx, api.ExecutionGrant{
		GrantId:   "grant_ve",
		IntentId:  "int_ve",
		Status:    api.ExecutionGrantStatusActive,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	// No vault entry → vault.Get will fail

	w := httptest.NewRecorder()
	s.RunExecution(w, execRequest("grant_ve"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRunExecution_TEEMode_EnclaveError(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()

	conn := &stubConnector{}
	s.registry.Register(ctx, conn)
	s.enclaveClient = &stubEnclaveClient{
		err: context.DeadlineExceeded,
	}

	seedGrantAndIntent(ctx, s, "grant_te", "int_te")

	w := httptest.NewRecorder()
	s.RunExecution(w, execRequest("grant_te"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRunExecution_DirectMode_ConnectorError(t *testing.T) {
	s := newExecutionServer()
	enableVaultEncryption(s, "usr_a")
	ctx := context.Background()

	conn := &stubConnector{
		err: context.DeadlineExceeded,
	}
	s.registry.Register(ctx, conn)

	seedGrantAndIntent(ctx, s, "grant_ce", "int_ce")

	w := httptest.NewRecorder()
	s.RunExecution(w, authedExecRequest("grant_ce", userAClaims))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRunExecution_DirectMode_ConnectorFailed(t *testing.T) {
	s := newExecutionServer()
	enableVaultEncryption(s, "usr_a")
	ctx := context.Background()

	conn := &stubConnector{
		result: connectorpkg.ExecutionResult{
			Status: connectorpkg.ExecutionStatusFailed,
			Error:  "push rejected",
		},
	}
	s.registry.Register(ctx, conn)

	seedGrantAndIntent(ctx, s, "grant_cf", "int_cf")

	w := httptest.NewRecorder()
	s.RunExecution(w, authedExecRequest("grant_cf", userAClaims))

	// Even on connector failure, the handler returns 202 with the execution ID.
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Encrypted credential (KEK) path tests ---

func seedEncryptedGrantAndIntent(ctx context.Context, s *apiServer, grantID, intentID string, kek []byte) {
	s.intents.Create(ctx, api.IntentEnvelope{
		IntentId:    intentID,
		WorkspaceId: "ws_1",
		Status:      api.Approved,
		Action: api.ActionIntent{
			Type:    "git.push",
			Summary: "push to main",
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	s.grants.Create(ctx, api.ExecutionGrant{
		GrantId:   grantID,
		IntentId:  intentID,
		Status:    api.ExecutionGrantStatusActive,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})

	// Store an encrypted credential in vault.
	ciphertext, _ := crypto.Encrypt([]byte("ghp_secret_token"), kek)
	s.vault.Put(ctx, "connectors/github/default", ciphertext, vault.Metadata{
		Type:   "api_key",
		Labels: map[string]string{vault.EncryptedLabel: "true"},
	})
}

func authedExecRequest(grantID string, claims *auth.Claims) *http.Request {
	req := execRequest(grantID)
	if claims != nil {
		ctx := auth.ContextWithClaims(req.Context(), claims)
		req = req.WithContext(ctx)
	}
	return req
}

func TestRunExecution_EncryptedCredential_VaultLocked(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()

	conn := &stubConnector{result: connectorpkg.ExecutionResult{Status: connectorpkg.ExecutionStatusSucceeded}}
	s.registry.Register(ctx, conn)

	kek := make([]byte, 32)
	seedEncryptedGrantAndIntent(ctx, s, "grant_enc1", "int_enc1", kek)

	w := httptest.NewRecorder()
	s.RunExecution(w, authedExecRequest("grant_enc1", userAClaims))
	// Encrypted credentials with no KEK session → 423 vault locked.
	if w.Code != 423 {
		t.Fatalf("expected 423, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRunExecution_TEEMode_WithEscrow(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()

	conn := &stubConnector{}
	s.registry.Register(ctx, conn)
	s.enclaveClient = &stubEnclaveClient{
		resp: enclave.ExecuteResponse{
			Status:     "succeeded",
			Output:     map[string]any{"result": "ok"},
			ReceiptRef: "receipt_esc",
		},
	}

	seedGrantAndIntent(ctx, s, "grant_esc", "int_esc")

	// Pre-populate escrow index so the handler uses the escrow ID path.
	s.escrowIndex.Store("connectors/github/default", "escrow_123")

	w := httptest.NewRecorder()
	s.RunExecution(w, authedExecRequest("grant_esc", userAClaims))
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp api.ExecutionRunResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != api.Accepted {
		t.Errorf("expected status 'accepted', got %s", resp.Status)
	}
}

func TestRunExecution_EncryptedCredential_DecryptsWithKEK(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()

	conn := &stubConnector{result: connectorpkg.ExecutionResult{Status: connectorpkg.ExecutionStatusSucceeded}}
	s.registry.Register(ctx, conn)

	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	seedEncryptedGrantAndIntent(ctx, s, "grant_enc5", "int_enc5", kek)

	// Set up KEK session so the handler can decrypt.
	cache := auth.NewKEKSessionCache(24 * time.Hour)
	cache.Set("usr_a", kek)
	s.kekSessionCache = cache

	w := httptest.NewRecorder()
	s.RunExecution(w, authedExecRequest("grant_enc5", userAClaims))
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (decrypted + executed), got %d: %s", w.Code, w.Body.String())
	}
}
