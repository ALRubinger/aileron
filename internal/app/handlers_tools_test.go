package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
)

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

func TestExecuteTool_HostExecution(t *testing.T) {
	// Tool execution uses the host vault.
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
