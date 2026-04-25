package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
)

// toolsEnclaveClient implements enclave.Client for tool handler tests,
// capturing SourceExecute calls.
type toolsEnclaveClient struct {
	sourceResp enclave.SourceExecuteResponse
	sourceErr  error
	sourceReq  *enclave.SourceExecuteRequest // last captured request
}

func (c *toolsEnclaveClient) Attest(_ context.Context, _ enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	return enclave.AttestationResponse{}, nil
}
func (c *toolsEnclaveClient) EstablishSession(_ context.Context, _ enclave.SessionRequest) (enclave.SessionResponse, error) {
	return enclave.SessionResponse{}, nil
}
func (c *toolsEnclaveClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{}, nil
}
func (c *toolsEnclaveClient) OAuthExchange(_ context.Context, _ enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	return enclave.OAuthExchangeResponse{}, nil
}
func (c *toolsEnclaveClient) Execute(_ context.Context, _ enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	return enclave.ExecuteResponse{}, nil
}
func (c *toolsEnclaveClient) EscrowStore(_ context.Context, _ enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	return enclave.EscrowStoreResponse{}, nil
}
func (c *toolsEnclaveClient) EscrowList(_ context.Context) (enclave.EscrowListResponse, error) {
	return enclave.EscrowListResponse{}, nil
}
func (c *toolsEnclaveClient) EscrowRevoke(_ context.Context, _ enclave.EscrowRevokeRequest) error {
	return nil
}
func (c *toolsEnclaveClient) SourceExecute(_ context.Context, req enclave.SourceExecuteRequest) (enclave.SourceExecuteResponse, error) {
	c.sourceReq = &req
	return c.sourceResp, c.sourceErr
}
func (c *toolsEnclaveClient) Ready(_ context.Context) error { return nil }
func (c *toolsEnclaveClient) Close() error                  { return nil }

// stubSourceConnector is a minimal SourceConnector for handler tests.
type stubSourceConnector struct{}

func (s *stubSourceConnector) Provider() string { return "slack" }
func (s *stubSourceConnector) Tools() []source.ToolDefinition {
	return []source.ToolDefinition{
		{Name: "slack_channel_history", Description: "Get channel messages"},
		{Name: "slack_search_messages", Description: "Search messages"},
	}
}
func (s *stubSourceConnector) Execute(_ context.Context, tool string, params map[string]any, _ []byte) (map[string]any, error) {
	return map[string]any{"tool": tool, "params": params}, nil
}

func newToolsTestServer() *apiServer {
	sourceReg := source.NewRegistry()
	sourceReg.Register(&stubSourceConnector{})

	return &apiServer{
		log:               slog.Default(),
		connectedAccounts: mem.NewConnectedAccountStore(),
		vault:             vault.NewMemVault(),
		sourceRegistry:    sourceReg,
		users:             &stubUserStore{},
		newID:             func() string { return "test-id" },
	}
}

func TestListTools_WithConnectedAccount(t *testing.T) {
	srv := newToolsTestServer()
	ctx := context.Background()

	// Seed a connected Slack account.
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/tools", "", userAClaims)
	srv.handleListTools(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Tools []source.ToolDefinition `json:"tools"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(resp.Tools))
	}
}

func TestListTools_NoConnectedAccounts(t *testing.T) {
	srv := newToolsTestServer()

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/tools", "", userAClaims)
	srv.handleListTools(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Tools []source.ToolDefinition `json:"tools"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Tools) != 0 {
		t.Fatalf("expected 0 tools without connected accounts, got %d", len(resp.Tools))
	}
}

func TestListTools_InactiveAccount(t *testing.T) {
	srv := newToolsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusRevoked, // not active
	})

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/tools", "", userAClaims)
	srv.handleListTools(w, r)

	var resp struct {
		Tools []source.ToolDefinition `json:"tools"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Tools) != 0 {
		t.Fatalf("expected 0 tools for inactive account, got %d", len(resp.Tools))
	}
}

func TestListTools_Unauthorized(t *testing.T) {
	srv := newToolsTestServer()

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/tools", "", nil)
	srv.handleListTools(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestListTools_NoSourceRegistry(t *testing.T) {
	srv := newToolsTestServer()
	srv.sourceRegistry = nil

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/tools", "", userAClaims)
	srv.handleListTools(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestExecuteTool_Success(t *testing.T) {
	srv := newToolsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{Type: "oauth_user_token"})

	body := `{"tool":"slack_channel_history","params":{"channel":"C0BACKEND"}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["tool"] != "slack_channel_history" {
		t.Errorf("expected tool=slack_channel_history, got %v", resp["tool"])
	}
}

func TestExecuteTool_UnknownTool(t *testing.T) {
	srv := newToolsTestServer()

	body := `{"tool":"nonexistent_tool","params":{}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestExecuteTool_MissingTool(t *testing.T) {
	srv := newToolsTestServer()

	body := `{"params":{}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestExecuteTool_InvalidJSON(t *testing.T) {
	srv := newToolsTestServer()

	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", "not-json", userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestExecuteTool_NotConnected(t *testing.T) {
	srv := newToolsTestServer()
	// No connected Slack account.

	body := `{"tool":"slack_channel_history","params":{"channel":"C0BACKEND"}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not_connected") {
		t.Errorf("expected not_connected error, got %s", w.Body.String())
	}
}

func TestExecuteTool_Unauthorized(t *testing.T) {
	srv := newToolsTestServer()

	body := `{"tool":"slack_channel_history","params":{}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, nil)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestExecuteTool_NoSourceRegistry(t *testing.T) {
	srv := newToolsTestServer()
	srv.sourceRegistry = nil

	body := `{"tool":"slack_channel_history","params":{}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestExecuteTool_InactiveAccount(t *testing.T) {
	srv := newToolsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusRevoked,
	})

	body := `{"tool":"slack_channel_history","params":{"channel":"C0BACKEND"}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExecuteTool_EnclaveRoute_Success(t *testing.T) {
	// When the enclave is active and credentials are escrowed, tool execution
	// must route through SourceExecute — plaintext never touches the host.
	srv := newToolsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})

	resultJSON, _ := json.Marshal(map[string]any{"messages": []string{"hello"}})
	mock := &toolsEnclaveClient{
		sourceResp: enclave.SourceExecuteResponse{Result: resultJSON},
	}
	srv.enclaveClient = mock
	srv.escrowIndex.Store("connected-accounts/usr_a/slack", "esc_123")

	body := `{"tool":"slack_channel_history","params":{"channel":"C0BACKEND"}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the request was routed through the enclave.
	if mock.sourceReq == nil {
		t.Fatal("expected SourceExecute to be called")
	}
	if mock.sourceReq.EscrowID != "esc_123" {
		t.Errorf("EscrowID = %q, want esc_123", mock.sourceReq.EscrowID)
	}
	if mock.sourceReq.Tool != "slack_channel_history" {
		t.Errorf("Tool = %q, want slack_channel_history", mock.sourceReq.Tool)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["messages"] == nil {
		t.Errorf("expected messages in response, got %v", resp)
	}
}

func TestExecuteTool_EnclaveRoute_Error(t *testing.T) {
	// When SourceExecute returns a tool-level error, it should be surfaced.
	srv := newToolsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})

	mock := &toolsEnclaveClient{
		sourceResp: enclave.SourceExecuteResponse{Error: "channel not found"},
	}
	srv.enclaveClient = mock
	srv.escrowIndex.Store("connected-accounts/usr_a/slack", "esc_123")

	body := `{"tool":"slack_channel_history","params":{"channel":"C_NONEXIST"}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "channel not found") {
		t.Errorf("expected error message in response, got %s", w.Body.String())
	}
}

func TestExecuteTool_EnclaveActive_NoEscrow_Forbidden(t *testing.T) {
	// When the enclave is active but the user has no escrowed credentials,
	// the endpoint must reject the request — not fall through to host-side
	// credential access.
	srv := newToolsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})

	srv.enclaveClient = &toolsEnclaveClient{}
	// No escrow entries — vault is locked.

	body := `{"tool":"slack_channel_history","params":{"channel":"C0BACKEND"}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "vault_locked") {
		t.Errorf("expected vault_locked error code, got %s", w.Body.String())
	}
}

func TestExecuteTool_EnclaveRoute_TransportError(t *testing.T) {
	// When the enclave is unreachable, the transport error is surfaced.
	srv := newToolsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})

	mock := &toolsEnclaveClient{
		sourceErr: context.DeadlineExceeded,
	}
	srv.enclaveClient = mock
	srv.escrowIndex.Store("connected-accounts/usr_a/slack", "esc_123")

	body := `{"tool":"slack_channel_history","params":{"channel":"C0BACKEND"}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExecuteTool_NoEnclave_HostExecution(t *testing.T) {
	// Without an enclave, tool execution uses the host vault — existing behavior.
	srv := newToolsTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	srv.vault.Put(ctx, "connected-accounts/usr_a/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{Type: "oauth_user_token"})

	body := `{"tool":"slack_channel_history","params":{"channel":"C0BACKEND"}}`
	w := httptest.NewRecorder()
	r := mcpRequest("POST", "/v1/tools/execute", body, userAClaims)
	srv.handleExecuteTool(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
