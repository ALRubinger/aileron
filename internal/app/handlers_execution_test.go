package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/auth"
	connectorpkg "github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
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

func newExecutionServer() *apiServer {
	return &apiServer{
		log:                slog.Default(),
		registry:           connectorpkg.NewRegistry(),
		intents:            mem.NewIntentStore(),
		grants:             mem.NewGrantStore(),
		executions:         mem.NewExecutionStore(),
		traces:             mem.NewTraceStore(),
		vault:              vault.NewMemVault(),
		newID:              func() string { return "test-id" },
	}
}

func seedGrantAndIntent(ctx context.Context, s *apiServer, grantID, intentID string) {
	s.intents.Create(ctx, api.IntentEnvelope{
		IntentId:    intentID,
		WorkspaceId: "ws_1",
		Status:      api.IntentStatusApproved,
		Action: api.ActionIntent{
			Type:    "git.push",
			Summary: "push to main",
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	ciphertext, _ := crypto.Encrypt([]byte("ghp_token"), testKEK)
	s.vault.Put(ctx, "connectors/github/default", ciphertext, vault.Metadata{
		Type:   "api_key",
		Labels: map[string]string{vault.EncryptedLabel: "true"},
	})
	intent, err := s.intents.Get(ctx, intentID)
	if err != nil {
		panic(err)
	}
	grant, err := s.newExecutionGrant(ctx, intent, grantID, time.Now().UTC().Add(time.Hour), nil, "")
	if err != nil {
		panic(err)
	}
	if err := s.grants.Create(ctx, grant); err != nil {
		panic(err)
	}
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
	if resp.Status != api.ExecutionRunResponseStatusAccepted {
		t.Errorf("expected status 'accepted', got %s", resp.Status)
	}
	if resp.ExecutionId == "" {
		t.Error("expected non-empty execution ID")
	}
}

func TestApproveRequest_IssuesExecutionGrant(t *testing.T) {
	s := newExecutionServer()
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s.approvals = mem.NewApprovalStore()
	s.orchestrator = approval.NewInMemoryOrchestrator(s.approvals, func() string { return "approval-id" })
	s.vault.Put(ctx, "connectors/github/default", []byte("ciphertext"), vault.Metadata{Type: "api_key"})
	intent := api.IntentEnvelope{
		IntentId:    "int_approve",
		WorkspaceId: "ws_1",
		Status:      api.IntentStatusPendingApproval,
		Action:      api.ActionIntent{Type: "git.push", Summary: "push"},
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := s.intents.Create(ctx, intent); err != nil {
		t.Fatalf("Create intent: %v", err)
	}
	apr, err := s.orchestrator.Request(ctx, approval.ApprovalRequest{
		IntentID:    intent.IntentId,
		WorkspaceID: intent.WorkspaceId,
		Rationale:   "test",
	})
	if err != nil {
		t.Fatalf("Request approval: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/approve", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.ApproveRequest(w, req, apr.ApprovalID)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp api.ApprovalActionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ExecutionGrantId == nil {
		t.Fatal("expected execution grant ID")
	}
	grant, err := s.grants.Get(ctx, *resp.ExecutionGrantId)
	if err != nil {
		t.Fatalf("Get grant: %v", err)
	}
	if grant.IntentId != intent.IntentId {
		t.Fatalf("grant intent = %q, want %q", grant.IntentId, intent.IntentId)
	}
	if grant.Status != api.ExecutionGrantStatusActive {
		t.Fatalf("grant status = %q, want active", grant.Status)
	}
}

func TestApproveRequest_MissingIntentAfterApproval(t *testing.T) {
	s := newExecutionServer()
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s.approvals = mem.NewApprovalStore()
	s.orchestrator = approval.NewInMemoryOrchestrator(s.approvals, func() string { return "approval-id" })
	apr, err := s.orchestrator.Request(ctx, approval.ApprovalRequest{
		IntentID:    "missing_intent",
		WorkspaceID: "ws_1",
		Rationale:   "test",
	})
	if err != nil {
		t.Fatalf("Request approval: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/approve", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.ApproveRequest(w, req, apr.ApprovalID)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestModifyRequest_IssuesBoundedExecutionGrant(t *testing.T) {
	s := newExecutionServer()
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s.approvals = mem.NewApprovalStore()
	s.orchestrator = approval.NewInMemoryOrchestrator(s.approvals, func() string { return "approval-id" })
	s.vault.Put(ctx, "connectors/github/default", []byte("ciphertext"), vault.Metadata{Type: "api_key"})
	intent := api.IntentEnvelope{
		IntentId:    "int_modify",
		WorkspaceId: "ws_1",
		Status:      api.IntentStatusPendingApproval,
		Action:      api.ActionIntent{Type: "git.issue.create", Summary: "file issue"},
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := s.intents.Create(ctx, intent); err != nil {
		t.Fatalf("Create intent: %v", err)
	}
	apr, err := s.orchestrator.Request(ctx, approval.ApprovalRequest{
		IntentID:    intent.IntentId,
		WorkspaceID: intent.WorkspaceId,
		Rationale:   "test",
	})
	if err != nil {
		t.Fatalf("Request approval: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/modify", strings.NewReader(`{"modifications":{"issue_title":"approved"}}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.ModifyRequest(w, req, apr.ApprovalID)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp api.ApprovalActionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ExecutionGrantId == nil {
		t.Fatal("expected execution grant ID")
	}
	grant, err := s.grants.Get(ctx, *resp.ExecutionGrantId)
	if err != nil {
		t.Fatalf("Get grant: %v", err)
	}
	if grant.BoundedParameters == nil {
		t.Fatal("expected bounded parameters on grant")
	}
	if (*grant.BoundedParameters)["issue_title"] != "approved" {
		t.Fatalf("bounded issue_title = %v, want approved", (*grant.BoundedParameters)["issue_title"])
	}
}

func TestModifyRequest_MissingIntentAfterApproval(t *testing.T) {
	s := newExecutionServer()
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s.approvals = mem.NewApprovalStore()
	s.orchestrator = approval.NewInMemoryOrchestrator(s.approvals, func() string { return "approval-id" })
	apr, err := s.orchestrator.Request(ctx, approval.ApprovalRequest{
		IntentID:    "missing_intent",
		WorkspaceID: "ws_1",
		Rationale:   "test",
	})
	if err != nil {
		t.Fatalf("Request approval: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/modify", strings.NewReader(`{"modifications":{"issue_title":"approved"}}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.ModifyRequest(w, req, apr.ApprovalID)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
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
		Status:      api.IntentStatusApproved,
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
		Status:      api.IntentStatusApproved,
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
		Status:      api.IntentStatusApproved,
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

// failingIntentStore wraps a real IntentStore and injects configured errors on
// Get and/or Update, letting tests exercise the store-failure paths in the
// approval-action handlers without polluting the production store with hooks.
type failingIntentStore struct {
	inner      store.IntentStore
	failGet    error
	failUpdate error
}

func (f *failingIntentStore) Create(ctx context.Context, intent api.IntentEnvelope) error {
	return f.inner.Create(ctx, intent)
}

func (f *failingIntentStore) Get(ctx context.Context, intentID string) (api.IntentEnvelope, error) {
	if f.failGet != nil {
		return api.IntentEnvelope{}, f.failGet
	}
	return f.inner.Get(ctx, intentID)
}

func (f *failingIntentStore) List(ctx context.Context, filter store.IntentFilter) ([]api.IntentEnvelope, error) {
	return f.inner.List(ctx, filter)
}

func (f *failingIntentStore) Update(ctx context.Context, intent api.IntentEnvelope) error {
	if f.failUpdate != nil {
		return f.failUpdate
	}
	return f.inner.Update(ctx, intent)
}

// newApprovalActionServer builds a server with an in-memory approval
// orchestrator and a seeded pending-approval intent, then requests an approval
// and returns the server plus the requested approval record.
func newApprovalActionServer(t *testing.T, ctx context.Context, intentID, actionType string) (*apiServer, approval.Approval) {
	t.Helper()
	s := newExecutionServer()
	s.approvals = mem.NewApprovalStore()
	s.orchestrator = approval.NewInMemoryOrchestrator(s.approvals, func() string { return "approval-id" })
	s.vault.Put(ctx, "connectors/github/default", []byte("ciphertext"), vault.Metadata{Type: "api_key"})
	intent := api.IntentEnvelope{
		IntentId:    intentID,
		WorkspaceId: "ws_1",
		Status:      api.IntentStatusPendingApproval,
		Action:      api.ActionIntent{Type: actionType, Summary: "summary"},
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := s.intents.Create(ctx, intent); err != nil {
		t.Fatalf("Create intent: %v", err)
	}
	apr, err := s.orchestrator.Request(ctx, approval.ApprovalRequest{
		IntentID:    intent.IntentId,
		WorkspaceID: intent.WorkspaceId,
		Rationale:   "test",
	})
	if err != nil {
		t.Fatalf("Request approval: %v", err)
	}
	return s, apr
}

func assertStoreError(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	var resp api.Error
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if resp.Error.Code != "store_error" {
		t.Fatalf("error code = %q, want store_error", resp.Error.Code)
	}
}

// R1/R5 success counterpart: Deny with a healthy store returns 200 and the
// persisted intent reflects the denied status.
func TestDenyRequest_Success(t *testing.T) {
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s, apr := newApprovalActionServer(t, ctx, "int_deny_ok", "git.push")

	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/deny", strings.NewReader(`{"reason":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.DenyRequest(w, req, apr.ApprovalID)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp api.ApprovalActionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.IntentStatus == nil || *resp.IntentStatus != api.IntentStatusDenied {
		t.Fatalf("intent status = %v, want denied", resp.IntentStatus)
	}
	intent, err := s.intents.Get(ctx, "int_deny_ok")
	if err != nil {
		t.Fatalf("Get intent: %v", err)
	}
	if intent.Status != api.IntentStatusDenied {
		t.Fatalf("persisted intent status = %q, want denied", intent.Status)
	}
}

// R1: a Get failure in Deny returns 500 store_error and does not report the
// intent as denied. This is the core regression — it fails before U2's fix.
func TestDenyRequest_GetFailure(t *testing.T) {
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s, apr := newApprovalActionServer(t, ctx, "int_deny_getfail", "git.push")
	s.intents = &failingIntentStore{inner: s.intents, failGet: errors.New("boom")}

	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/deny", strings.NewReader(`{"reason":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.DenyRequest(w, req, apr.ApprovalID)

	assertStoreError(t, w)

	// R3: orchestrator-side approval mutation is not rolled back.
	stored, err := s.approvals.Get(ctx, apr.ApprovalID)
	if err != nil {
		t.Fatalf("Get approval: %v", err)
	}
	if stored.Status != api.ApprovalStatusDenied {
		t.Fatalf("approval status = %q, want denied (state must survive the 500)", stored.Status)
	}
}

// R2: an Update failure in Deny (Get succeeds) returns 500 store_error.
func TestDenyRequest_UpdateFailure(t *testing.T) {
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s, apr := newApprovalActionServer(t, ctx, "int_deny_updfail", "git.push")
	s.intents = &failingIntentStore{inner: s.intents, failUpdate: errors.New("boom")}

	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/deny", strings.NewReader(`{"reason":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.DenyRequest(w, req, apr.ApprovalID)

	assertStoreError(t, w)

	// R3: approval mutation is not rolled back.
	stored, err := s.approvals.Get(ctx, apr.ApprovalID)
	if err != nil {
		t.Fatalf("Get approval: %v", err)
	}
	if stored.Status != api.ApprovalStatusDenied {
		t.Fatalf("approval status = %q, want denied (state must survive the 500)", stored.Status)
	}
}

// R2: an Update failure in Approve (after grant creation) returns 500 store_error.
func TestApproveRequest_UpdateFailure(t *testing.T) {
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s, apr := newApprovalActionServer(t, ctx, "int_approve_updfail", "git.push")
	s.intents = &failingIntentStore{inner: s.intents, failUpdate: errors.New("boom")}

	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/approve", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.ApproveRequest(w, req, apr.ApprovalID)

	assertStoreError(t, w)
}

// R2: an Update failure in Modify (after grant creation) returns 500 store_error.
func TestModifyRequest_UpdateFailure(t *testing.T) {
	ctx := setTestUserClaims(context.Background(), "usr_a")
	s, apr := newApprovalActionServer(t, ctx, "int_modify_updfail", "git.issue.create")
	s.intents = &failingIntentStore{inner: s.intents, failUpdate: errors.New("boom")}

	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+apr.ApprovalID+"/modify", strings.NewReader(`{"modifications":{"issue_title":"approved"}}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	s.ModifyRequest(w, req, apr.ApprovalID)

	assertStoreError(t, w)
}
