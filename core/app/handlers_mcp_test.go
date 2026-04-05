package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/registry"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func newMCPServer() *apiServer {
	return &apiServer{
		mcpServers:           mem.NewMCPServerStore(),
		enterpriseMCPServers: mem.NewEnterpriseMCPServerStore(),
		vault:                vault.NewMemVault(),
		users:                nil, // auth enabled check: nil means auth disabled
		newID:                func() string { return "test-id" },
	}
}

func newMCPServerWithAuth() *apiServer {
	srv := newMCPServer()
	// Setting users to non-nil signals auth is enabled, so requireAuth
	// will enforce authentication.
	srv.users = &stubUserStore{}
	return srv
}

// stubUserStore is a minimal non-nil UserStore to signal auth is enabled.
type stubUserStore struct{}

func (s *stubUserStore) Create(_ context.Context, _ model.User) error { return nil }
func (s *stubUserStore) Get(_ context.Context, _ string) (model.User, error) {
	return model.User{}, nil
}
func (s *stubUserStore) GetByEmail(_ context.Context, _ string) (model.User, error) {
	return model.User{}, nil
}
func (s *stubUserStore) GetByProviderSubject(_ context.Context, _, _ string) (model.User, error) {
	return model.User{}, nil
}
func (s *stubUserStore) List(_ context.Context, _ store.UserFilter) ([]model.User, error) {
	return nil, nil
}
func (s *stubUserStore) Update(_ context.Context, _ model.User) error { return nil }

func mcpRequest(method, path, body string, claims *auth.Claims) *http.Request {
	reader := strings.NewReader(body)
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if claims != nil {
		ctx := auth.ContextWithClaims(req.Context(), claims)
		req = req.WithContext(ctx)
	}
	return req
}

var userAClaims = &auth.Claims{
	EnterpriseID: "ent_1",
	Email:        "usera@example.com",
	Role:         "member",
}

var userBClaims = &auth.Claims{
	EnterpriseID: "ent_1",
	Email:        "userb@example.com",
	Role:         "member",
}

var adminClaims = &auth.Claims{
	EnterpriseID: "ent_1",
	Email:        "admin@example.com",
	Role:         "owner",
}

func init() {
	userAClaims.Subject = "usr_a"
	userBClaims.Subject = "usr_b"
	adminClaims.Subject = "usr_admin"
}

// --- User MCP Server tests ---

func TestListMCPServers_UserScoping(t *testing.T) {
	srv := newMCPServer()

	// User A creates a server.
	w := httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers",
		`{"name":"server-a","command":["cmd"]}`, userAClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateMCPServer status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// User B creates a server.
	srv.newID = func() string { return "test-id-2" }
	w = httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers",
		`{"name":"server-b","command":["cmd"]}`, userBClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateMCPServer status = %d, want %d", w.Code, http.StatusCreated)
	}

	// User A lists — should see only their server.
	w = httptest.NewRecorder()
	srv.ListMCPServers(w, mcpRequest("GET", "/v1/mcp-servers", "", userAClaims))
	if w.Code != http.StatusOK {
		t.Fatalf("ListMCPServers status = %d, want %d", w.Code, http.StatusOK)
	}

	var result struct {
		Items []api.MCPServerConfig `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&result)
	if len(result.Items) != 1 {
		t.Fatalf("user A list length = %d, want 1", len(result.Items))
	}
	if result.Items[0].Name != "server-a" {
		t.Errorf("server name = %q, want %q", result.Items[0].Name, "server-a")
	}
	if result.Items[0].Source == nil || *result.Items[0].Source != api.MCPServerConfigSourcePersonal {
		t.Errorf("source = %v, want personal", result.Items[0].Source)
	}
}

func TestListMCPServers_MergesEnterpriseAutoEnabled(t *testing.T) {
	srv := newMCPServer()

	// User A creates a personal server.
	w := httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers",
		`{"name":"personal-server","command":["cmd"]}`, userAClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateMCPServer status = %d", w.Code)
	}

	// Admin adds an enterprise auto-enabled server.
	srv.newID = func() string { return "ent-test-id" }
	w = httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
		`{"name":"enterprise-server","command":["cmd"],"auto_enabled":true}`, adminClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateEnterpriseMCPServer status = %d; body: %s", w.Code, w.Body.String())
	}

	// User A lists — should see personal + enterprise auto-enabled.
	srv.newID = func() string { return "unused" }
	w = httptest.NewRecorder()
	srv.ListMCPServers(w, mcpRequest("GET", "/v1/mcp-servers", "", userAClaims))
	if w.Code != http.StatusOK {
		t.Fatalf("ListMCPServers status = %d", w.Code)
	}

	var result struct {
		Items []api.MCPServerConfig `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&result)
	if len(result.Items) != 2 {
		t.Fatalf("merged list length = %d, want 2", len(result.Items))
	}

	// Check sources.
	sources := map[api.MCPServerConfigSource]bool{}
	for _, item := range result.Items {
		if item.Source != nil {
			sources[*item.Source] = true
		}
	}
	if !sources[api.MCPServerConfigSourcePersonal] {
		t.Error("expected a personal source server in list")
	}
	if !sources[api.MCPServerConfigSourceEnterprise] {
		t.Error("expected an enterprise source server in list")
	}
}

func TestGetMCPServer_OwnershipCheck(t *testing.T) {
	srv := newMCPServer()

	// User A creates a server.
	w := httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers",
		`{"name":"server-a","command":["cmd"]}`, userAClaims))

	var created api.MCPServerConfig
	json.NewDecoder(w.Body).Decode(&created)
	id := *created.Id

	// User A can get it.
	w = httptest.NewRecorder()
	srv.GetMCPServer(w, mcpRequest("GET", "/v1/mcp-servers/"+id, "", userAClaims), id)
	if w.Code != http.StatusOK {
		t.Fatalf("user A Get status = %d, want %d", w.Code, http.StatusOK)
	}

	// User B gets 404.
	w = httptest.NewRecorder()
	srv.GetMCPServer(w, mcpRequest("GET", "/v1/mcp-servers/"+id, "", userBClaims), id)
	if w.Code != http.StatusNotFound {
		t.Fatalf("user B Get status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDeleteMCPServer_OwnershipCheck(t *testing.T) {
	srv := newMCPServer()

	// User A creates a server.
	w := httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers",
		`{"name":"server-a","command":["cmd"]}`, userAClaims))

	var created api.MCPServerConfig
	json.NewDecoder(w.Body).Decode(&created)
	id := *created.Id

	// User B cannot delete it.
	w = httptest.NewRecorder()
	srv.DeleteMCPServer(w, mcpRequest("DELETE", "/v1/mcp-servers/"+id, "", userBClaims), id)
	if w.Code != http.StatusNotFound {
		t.Fatalf("user B Delete status = %d, want %d", w.Code, http.StatusNotFound)
	}

	// User A can delete it.
	w = httptest.NewRecorder()
	srv.DeleteMCPServer(w, mcpRequest("DELETE", "/v1/mcp-servers/"+id, "", userAClaims), id)
	if w.Code != http.StatusNoContent {
		t.Fatalf("user A Delete status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

// --- Enterprise MCP Server tests ---

func TestEnterpriseMCPServer_AdminOnly(t *testing.T) {
	srv := newMCPServer()

	// Member cannot create enterprise servers.
	w := httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
		`{"name":"ent-server","command":["cmd"]}`, userAClaims))
	if w.Code != http.StatusForbidden {
		t.Fatalf("member Create status = %d, want %d", w.Code, http.StatusForbidden)
	}

	// Admin can create.
	srv.newID = func() string { return "ent-test-id" }
	w = httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
		`{"name":"ent-server","command":["cmd"]}`, adminClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("admin Create status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var created api.EnterpriseMCPServer
	json.NewDecoder(w.Body).Decode(&created)
	id := *created.Id

	// Member can read.
	w = httptest.NewRecorder()
	srv.GetEnterpriseMCPServer(w, mcpRequest("GET", "/v1/enterprise/mcp-servers/"+id, "", userAClaims), id)
	if w.Code != http.StatusOK {
		t.Fatalf("member Get status = %d, want %d", w.Code, http.StatusOK)
	}

	// Member cannot update.
	w = httptest.NewRecorder()
	srv.UpdateEnterpriseMCPServer(w, mcpRequest("PUT", "/v1/enterprise/mcp-servers/"+id,
		`{"name":"updated","command":["cmd"]}`, userAClaims), id)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member Update status = %d, want %d", w.Code, http.StatusForbidden)
	}

	// Member cannot delete.
	w = httptest.NewRecorder()
	srv.DeleteEnterpriseMCPServer(w, mcpRequest("DELETE", "/v1/enterprise/mcp-servers/"+id, "", userAClaims), id)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member Delete status = %d, want %d", w.Code, http.StatusForbidden)
	}

	// Admin can delete.
	w = httptest.NewRecorder()
	srv.DeleteEnterpriseMCPServer(w, mcpRequest("DELETE", "/v1/enterprise/mcp-servers/"+id, "", adminClaims), id)
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin Delete status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestEnterpriseMCPServer_EnterpriseIsolation(t *testing.T) {
	srv := newMCPServer()

	// Admin in ent_1 creates a server.
	srv.newID = func() string { return "ent1-srv" }
	w := httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
		`{"name":"ent1-server","command":["cmd"]}`, adminClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("Create status = %d", w.Code)
	}

	// Admin in ent_2 cannot see it.
	ent2Admin := &auth.Claims{
		EnterpriseID: "ent_2",
		Email:        "admin2@example.com",
		Role:         "owner",
	}
	ent2Admin.Subject = "usr_admin2"

	w = httptest.NewRecorder()
	srv.GetEnterpriseMCPServer(w, mcpRequest("GET", "/v1/enterprise/mcp-servers/emcp_ent1-srv", "", ent2Admin), "emcp_ent1-srv")
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-enterprise Get status = %d, want %d", w.Code, http.StatusNotFound)
	}

	// List for ent_2 is empty.
	w = httptest.NewRecorder()
	srv.ListEnterpriseMCPServers(w, mcpRequest("GET", "/v1/enterprise/mcp-servers", "", ent2Admin))
	if w.Code != http.StatusOK {
		t.Fatalf("List status = %d", w.Code)
	}
	var result struct {
		Items []api.EnterpriseMCPServer `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&result)
	if len(result.Items) != 0 {
		t.Fatalf("ent_2 list length = %d, want 0", len(result.Items))
	}
}

func TestUpdateMCPServer_OwnershipAndSuccess(t *testing.T) {
	srv := newMCPServer()

	// User A creates a server.
	w := httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers",
		`{"name":"original","command":["cmd"]}`, userAClaims))
	var created api.MCPServerConfig
	json.NewDecoder(w.Body).Decode(&created)
	id := *created.Id

	// User B cannot update it.
	w = httptest.NewRecorder()
	srv.UpdateMCPServer(w, mcpRequest("PUT", "/v1/mcp-servers/"+id,
		`{"name":"hacked","command":["evil"]}`, userBClaims), id)
	if w.Code != http.StatusNotFound {
		t.Fatalf("user B Update status = %d, want %d", w.Code, http.StatusNotFound)
	}

	// User A can update it.
	w = httptest.NewRecorder()
	srv.UpdateMCPServer(w, mcpRequest("PUT", "/v1/mcp-servers/"+id,
		`{"name":"updated","command":["new-cmd"]}`, userAClaims), id)
	if w.Code != http.StatusOK {
		t.Fatalf("user A Update status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var updated api.MCPServerConfig
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.Name != "updated" {
		t.Errorf("Name = %q, want %q", updated.Name, "updated")
	}
	// Read-only fields preserved.
	if updated.UserId == nil || *updated.UserId != "usr_a" {
		t.Errorf("UserId should be preserved as usr_a, got %v", updated.UserId)
	}
	if updated.UpdatedAt == nil {
		t.Error("UpdatedAt should be set after update")
	}

	// Update nonexistent returns 404.
	w = httptest.NewRecorder()
	srv.UpdateMCPServer(w, mcpRequest("PUT", "/v1/mcp-servers/nonexistent",
		`{"name":"x","command":["x"]}`, userAClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("Update nonexistent status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestSetMCPServerCredential_OwnershipAndSuccess(t *testing.T) {
	srv := newMCPServer()

	// User A creates a server.
	w := httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers",
		`{"name":"cred-test","command":["cmd"]}`, userAClaims))
	var created api.MCPServerConfig
	json.NewDecoder(w.Body).Decode(&created)
	id := *created.Id

	// User B cannot set credentials.
	w = httptest.NewRecorder()
	srv.SetMCPServerCredential(w, mcpRequest("POST", "/v1/mcp-servers/"+id+"/credentials",
		`{"env_var_name":"API_KEY","secret_value":"secret123"}`, userBClaims), id)
	if w.Code != http.StatusNotFound {
		t.Fatalf("user B SetCredential status = %d, want %d", w.Code, http.StatusNotFound)
	}

	// User A can set credentials.
	w = httptest.NewRecorder()
	srv.SetMCPServerCredential(w, mcpRequest("POST", "/v1/mcp-servers/"+id+"/credentials",
		`{"env_var_name":"API_KEY","secret_value":"secret123"}`, userAClaims), id)
	if w.Code != http.StatusOK {
		t.Fatalf("user A SetCredential status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var credResp api.SetCredentialResponse
	json.NewDecoder(w.Body).Decode(&credResp)
	if credResp.EnvVarName != "API_KEY" {
		t.Errorf("EnvVarName = %q, want %q", credResp.EnvVarName, "API_KEY")
	}
	if !credResp.Stored {
		t.Error("Stored should be true")
	}

	// Verify server env was updated with vault reference.
	w = httptest.NewRecorder()
	srv.GetMCPServer(w, mcpRequest("GET", "/v1/mcp-servers/"+id, "", userAClaims), id)
	var fetched api.MCPServerConfig
	json.NewDecoder(w.Body).Decode(&fetched)
	if fetched.Env == nil {
		t.Fatal("env should not be nil after setting credential")
	}
	envVal := (*fetched.Env)["API_KEY"]
	if !strings.HasPrefix(envVal, "vault://") {
		t.Errorf("API_KEY env value = %q, want vault:// prefix", envVal)
	}

	// SetCredential on nonexistent server returns 404.
	w = httptest.NewRecorder()
	srv.SetMCPServerCredential(w, mcpRequest("POST", "/v1/mcp-servers/nonexistent/credentials",
		`{"env_var_name":"X","secret_value":"y"}`, userAClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("SetCredential nonexistent status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestUpdateEnterpriseMCPServer_AdminSuccess(t *testing.T) {
	srv := newMCPServer()

	// Admin creates an enterprise server.
	srv.newID = func() string { return "ent-upd" }
	w := httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
		`{"name":"original","command":["cmd"],"auto_enabled":false}`, adminClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("Create status = %d", w.Code)
	}
	var created api.EnterpriseMCPServer
	json.NewDecoder(w.Body).Decode(&created)
	id := *created.Id

	// Admin can update.
	w = httptest.NewRecorder()
	srv.UpdateEnterpriseMCPServer(w, mcpRequest("PUT", "/v1/enterprise/mcp-servers/"+id,
		`{"name":"updated-ent","command":["new-cmd"],"auto_enabled":true}`, adminClaims), id)
	if w.Code != http.StatusOK {
		t.Fatalf("admin Update status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var updated api.EnterpriseMCPServer
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.Name != "updated-ent" {
		t.Errorf("Name = %q, want %q", updated.Name, "updated-ent")
	}
	if updated.AutoEnabled == nil || !*updated.AutoEnabled {
		t.Error("AutoEnabled should be true after update")
	}
	// Read-only fields preserved.
	if updated.EnterpriseId == nil || *updated.EnterpriseId != "ent_1" {
		t.Errorf("EnterpriseId should be preserved")
	}

	// Update nonexistent returns 404.
	w = httptest.NewRecorder()
	srv.UpdateEnterpriseMCPServer(w, mcpRequest("PUT", "/v1/enterprise/mcp-servers/nonexistent",
		`{"name":"x","command":["x"]}`, adminClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("Update nonexistent status = %d, want %d", w.Code, http.StatusNotFound)
	}

	// Cross-enterprise update returns 404.
	ent2Admin := &auth.Claims{EnterpriseID: "ent_2", Email: "a2@x.com", Role: "owner"}
	ent2Admin.Subject = "usr_a2"
	w = httptest.NewRecorder()
	srv.UpdateEnterpriseMCPServer(w, mcpRequest("PUT", "/v1/enterprise/mcp-servers/"+id,
		`{"name":"hijack","command":["x"]}`, ent2Admin), id)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-enterprise Update status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestSetEnterpriseMCPServerCredential_AdminOnly(t *testing.T) {
	srv := newMCPServer()

	// Admin creates an enterprise server.
	srv.newID = func() string { return "ent-cred" }
	w := httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
		`{"name":"cred-ent","command":["cmd"]}`, adminClaims))
	var created api.EnterpriseMCPServer
	json.NewDecoder(w.Body).Decode(&created)
	id := *created.Id

	// Member cannot set credentials.
	w = httptest.NewRecorder()
	srv.SetEnterpriseMCPServerCredential(w, mcpRequest("POST", "/v1/enterprise/mcp-servers/"+id+"/credentials",
		`{"env_var_name":"KEY","secret_value":"val"}`, userAClaims), id)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member SetCredential status = %d, want %d", w.Code, http.StatusForbidden)
	}

	// Admin can set credentials.
	w = httptest.NewRecorder()
	srv.SetEnterpriseMCPServerCredential(w, mcpRequest("POST", "/v1/enterprise/mcp-servers/"+id+"/credentials",
		`{"env_var_name":"KEY","secret_value":"val"}`, adminClaims), id)
	if w.Code != http.StatusOK {
		t.Fatalf("admin SetCredential status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var credResp api.SetCredentialResponse
	json.NewDecoder(w.Body).Decode(&credResp)
	if credResp.EnvVarName != "KEY" || !credResp.Stored {
		t.Errorf("unexpected response: %+v", credResp)
	}

	// Cross-enterprise credential set returns 404.
	ent2Admin := &auth.Claims{EnterpriseID: "ent_2", Email: "a2@x.com", Role: "owner"}
	ent2Admin.Subject = "usr_a2"
	w = httptest.NewRecorder()
	srv.SetEnterpriseMCPServerCredential(w, mcpRequest("POST", "/v1/enterprise/mcp-servers/"+id+"/credentials",
		`{"env_var_name":"KEY","secret_value":"val"}`, ent2Admin), id)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-enterprise SetCredential status = %d, want %d", w.Code, http.StatusNotFound)
	}

	// Nonexistent server returns 404.
	w = httptest.NewRecorder()
	srv.SetEnterpriseMCPServerCredential(w, mcpRequest("POST", "/v1/enterprise/mcp-servers/nonexistent/credentials",
		`{"env_var_name":"KEY","secret_value":"val"}`, adminClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("nonexistent SetCredential status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDeleteMCPServer_Nonexistent(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.DeleteMCPServer(w, mcpRequest("DELETE", "/v1/mcp-servers/nonexistent", "", userAClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("Delete nonexistent status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDeleteEnterpriseMCPServer_CrossEnterprise(t *testing.T) {
	srv := newMCPServer()

	// Admin in ent_1 creates.
	srv.newID = func() string { return "cross-del" }
	w := httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
		`{"name":"ent1-srv","command":["cmd"]}`, adminClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("Create status = %d", w.Code)
	}

	// Admin in ent_2 cannot delete it.
	ent2Admin := &auth.Claims{EnterpriseID: "ent_2", Email: "a2@x.com", Role: "owner"}
	ent2Admin.Subject = "usr_a2"
	w = httptest.NewRecorder()
	srv.DeleteEnterpriseMCPServer(w, mcpRequest("DELETE", "/v1/enterprise/mcp-servers/emcp_cross-del", "", ent2Admin), "emcp_cross-del")
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-enterprise Delete status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHelpers_UserIDAndEnterpriseID(t *testing.T) {
	// With claims.
	req := mcpRequest("GET", "/test", "", adminClaims)
	if got := userIDFromRequest(req); got != "usr_admin" {
		t.Errorf("userIDFromRequest = %q, want %q", got, "usr_admin")
	}
	if got := enterpriseIDFromRequest(req); got != "ent_1" {
		t.Errorf("enterpriseIDFromRequest = %q, want %q", got, "ent_1")
	}

	// Without claims.
	req = mcpRequest("GET", "/test", "", nil)
	if got := userIDFromRequest(req); got != "" {
		t.Errorf("userIDFromRequest with no claims = %q, want empty", got)
	}
	if got := enterpriseIDFromRequest(req); got != "" {
		t.Errorf("enterpriseIDFromRequest with no claims = %q, want empty", got)
	}
}

func TestIsAdmin(t *testing.T) {
	// Owner is admin.
	if !isAdmin(mcpRequest("GET", "/", "", adminClaims)) {
		t.Error("owner should be admin")
	}
	// Member is not admin.
	if isAdmin(mcpRequest("GET", "/", "", userAClaims)) {
		t.Error("member should not be admin")
	}
	// No claims is not admin.
	if isAdmin(mcpRequest("GET", "/", "", nil)) {
		t.Error("nil claims should not be admin")
	}
	// Admin role is admin.
	adminRoleClaims := &auth.Claims{EnterpriseID: "ent_1", Email: "x@x.com", Role: "admin"}
	adminRoleClaims.Subject = "usr_x"
	if !isAdmin(mcpRequest("GET", "/", "", adminRoleClaims)) {
		t.Error("admin role should be admin")
	}
}

func TestMCPServer_UnauthenticatedWithAuthEnabled(t *testing.T) {
	srv := newMCPServerWithAuth()

	// Unauthenticated request when auth is enabled → 401.
	w := httptest.NewRecorder()
	srv.ListMCPServers(w, mcpRequest("GET", "/v1/mcp-servers", "", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated List status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	// Also test unauthenticated for enterprise endpoints.
	w = httptest.NewRecorder()
	srv.ListEnterpriseMCPServers(w, mcpRequest("GET", "/v1/enterprise/mcp-servers", "", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Enterprise List status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	w = httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers",
		`{"name":"x","command":["x"]}`, nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Create status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	w = httptest.NewRecorder()
	srv.SetMCPServerCredential(w, mcpRequest("POST", "/v1/mcp-servers/x/credentials",
		`{"env_var_name":"X","secret_value":"y"}`, nil), "x")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated SetCredential status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestCreateMCPServer_BadRequest(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers", `{invalid`, userAClaims))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCreateEnterpriseMCPServer_BadRequest(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers", `{invalid`, adminClaims))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestUpdateMCPServer_BadRequest(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.UpdateMCPServer(w, mcpRequest("PUT", "/v1/mcp-servers/x", `{invalid`, userAClaims), "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestUpdateEnterpriseMCPServer_BadRequest(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.UpdateEnterpriseMCPServer(w, mcpRequest("PUT", "/v1/enterprise/mcp-servers/x", `{invalid`, adminClaims), "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSetMCPServerCredential_BadRequest(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.SetMCPServerCredential(w, mcpRequest("POST", "/v1/mcp-servers/x/credentials", `{invalid`, userAClaims), "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSetEnterpriseMCPServerCredential_BadRequest(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.SetEnterpriseMCPServerCredential(w, mcpRequest("POST", "/v1/enterprise/mcp-servers/x/credentials", `{invalid`, adminClaims), "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Registry test helper ---

func newRegistryServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/servers" {
			http.NotFound(w, r)
			return
		}
		resp := registry.RegistryResponse{
			Servers: []registry.RegistryEntry{
				{Server: registry.RegistryServer{
					Name:        "io.example/filesystem",
					Description: "Filesystem access",
					VersionDetail: registry.VersionDetail{
						Version: "1.0.0",
						Packages: []registry.Package{{
							RegistryType: "npm",
							Name:         "@example/mcp-fs",
							Runtime:      registry.RuntimeConfig{Type: "node", Command: "npx"},
							EnvVars: []registry.EnvVar{
								{Name: "FS_ROOT", Description: "Root dir", Required: true},
							},
						}},
					},
				}},
				{Server: registry.RegistryServer{
					Name:        "io.example/github",
					Description: "GitHub integration",
					VersionDetail: registry.VersionDetail{
						Version: "2.0.0",
						Packages: []registry.Package{{
							RegistryType: "npm",
							Name:         "@example/mcp-github",
							Runtime:      registry.RuntimeConfig{Type: "node", Command: "npx"},
							EnvVars: []registry.EnvVar{
								{Name: "GITHUB_TOKEN", Description: "PAT", Required: true},
							},
						}},
					},
				}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func newMCPServerWithRegistry(t *testing.T) (*apiServer, *httptest.Server) {
	t.Helper()
	ts := newRegistryServer(t)
	client := registry.NewClient(ts.Client(), slog.Default()).WithBaseURL(ts.URL)
	// Prefetch so FetchAll doesn't block.
	client.Start(t.Context())

	srv := &apiServer{
		mcpServers:           mem.NewMCPServerStore(),
		enterpriseMCPServers: mem.NewEnterpriseMCPServerStore(),
		vault:                vault.NewMemVault(),
		registryClient:       client,
		newID:                func() string { return "mkt-test-id" },
	}
	return srv, ts
}

// --- Marketplace tests ---

func TestListMarketplaceServers_Success(t *testing.T) {
	srv, ts := newMCPServerWithRegistry(t)
	defer ts.Close()

	w := httptest.NewRecorder()
	srv.ListMarketplaceServers(w, mcpRequest("GET", "/v1/marketplace/servers", "", userAClaims), api.ListMarketplaceServersParams{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var result struct {
		Items []api.MarketplaceServer `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&result)
	if len(result.Items) != 2 {
		t.Fatalf("items length = %d, want 2", len(result.Items))
	}
	// Nothing installed yet.
	for _, item := range result.Items {
		if item.Installed != nil && *item.Installed {
			t.Errorf("server %q should not be installed", item.Name)
		}
	}
}

func TestListMarketplaceServers_WithSearch(t *testing.T) {
	srv, ts := newMCPServerWithRegistry(t)
	defer ts.Close()

	q := "filesystem"
	w := httptest.NewRecorder()
	srv.ListMarketplaceServers(w, mcpRequest("GET", "/v1/marketplace/servers?q=filesystem", "", userAClaims), api.ListMarketplaceServersParams{Q: &q})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result struct {
		Items []api.MarketplaceServer `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&result)
	if len(result.Items) != 1 {
		t.Fatalf("filtered items length = %d, want 1", len(result.Items))
	}
	if result.Items[0].Name != "io.example/filesystem" {
		t.Errorf("Name = %q, want %q", result.Items[0].Name, "io.example/filesystem")
	}
}

func TestListMarketplaceServers_InstalledFlagReflectsUserAndEnterprise(t *testing.T) {
	srv, ts := newMCPServerWithRegistry(t)
	defer ts.Close()

	// User A installs filesystem from marketplace.
	w := httptest.NewRecorder()
	srv.InstallMarketplaceServer(w, mcpRequest("POST", "/v1/marketplace/servers/io.example%2Ffilesystem/install", "", userAClaims), "io.example/filesystem")
	if w.Code != http.StatusCreated {
		t.Fatalf("Install status = %d; body: %s", w.Code, w.Body.String())
	}

	// Enterprise auto-enables github.
	srv.newID = func() string { return "ent-gh" }
	w = httptest.NewRecorder()
	srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
		`{"name":"github","command":["cmd"],"auto_enabled":true,"registry_id":"io.example/github"}`, adminClaims))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateEnterprise status = %d", w.Code)
	}

	// User A lists marketplace — both should show installed.
	srv.newID = func() string { return "unused" }
	w = httptest.NewRecorder()
	srv.ListMarketplaceServers(w, mcpRequest("GET", "/v1/marketplace/servers", "", userAClaims), api.ListMarketplaceServersParams{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var result struct {
		Items []api.MarketplaceServer `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&result)
	installedCount := 0
	for _, item := range result.Items {
		if item.Installed != nil && *item.Installed {
			installedCount++
		}
	}
	if installedCount != 2 {
		t.Errorf("installed count = %d, want 2", installedCount)
	}
}

func TestListMarketplaceServers_UnauthWithAuth(t *testing.T) {
	srv, ts := newMCPServerWithRegistry(t)
	defer ts.Close()
	srv.users = &stubUserStore{}

	w := httptest.NewRecorder()
	srv.ListMarketplaceServers(w, mcpRequest("GET", "/v1/marketplace/servers", "", nil), api.ListMarketplaceServersParams{})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestInstallMarketplaceServer_Success(t *testing.T) {
	srv, ts := newMCPServerWithRegistry(t)
	defer ts.Close()

	w := httptest.NewRecorder()
	srv.InstallMarketplaceServer(w, mcpRequest("POST", "/v1/marketplace/servers/io.example%2Ffilesystem/install", "", userAClaims), "io.example/filesystem")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var result api.InstallResult
	json.NewDecoder(w.Body).Decode(&result)

	if result.Server.Name != "io.example/filesystem" {
		t.Errorf("Name = %q, want %q", result.Server.Name, "io.example/filesystem")
	}
	if result.Server.UserId == nil || *result.Server.UserId != "usr_a" {
		t.Errorf("UserId = %v, want usr_a", result.Server.UserId)
	}
	if result.Server.Source == nil || *result.Server.Source != api.MCPServerConfigSourcePersonal {
		t.Error("Source should be personal")
	}
	if result.Server.RegistryId == nil || *result.Server.RegistryId != "io.example/filesystem" {
		t.Errorf("RegistryId = %v", result.Server.RegistryId)
	}
	if result.RequiredCredentials == nil || len(*result.RequiredCredentials) != 1 {
		t.Fatalf("RequiredCredentials length = %v, want 1", result.RequiredCredentials)
	}
	if (*result.RequiredCredentials)[0].Name != "FS_ROOT" {
		t.Errorf("first required cred = %q, want FS_ROOT", (*result.RequiredCredentials)[0].Name)
	}
}

func TestInstallMarketplaceServer_NotFound(t *testing.T) {
	srv, ts := newMCPServerWithRegistry(t)
	defer ts.Close()

	w := httptest.NewRecorder()
	srv.InstallMarketplaceServer(w, mcpRequest("POST", "/v1/marketplace/servers/nonexistent/install", "", userAClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestInstallMarketplaceServer_UnauthWithAuth(t *testing.T) {
	srv, ts := newMCPServerWithRegistry(t)
	defer ts.Close()
	srv.users = &stubUserStore{}

	w := httptest.NewRecorder()
	srv.InstallMarketplaceServer(w, mcpRequest("POST", "/v1/marketplace/servers/io.example%2Ffilesystem/install", "", nil), "io.example/filesystem")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// --- deriveCommand tests ---

func TestDeriveCommand_NpmPackage(t *testing.T) {
	srv := &registry.RegistryServer{
		Name: "test-server",
		VersionDetail: registry.VersionDetail{
			Packages: []registry.Package{{
				RegistryType: "npm",
				Name:         "@example/mcp-test",
				Runtime:      registry.RuntimeConfig{Args: []string{"--stdio"}},
				EnvVars:      []registry.EnvVar{{Name: "KEY", Required: true}},
			}},
		},
	}
	cmd, envVars := deriveCommand(srv)
	if len(cmd) != 4 || cmd[0] != "npx" || cmd[1] != "-y" || cmd[2] != "@example/mcp-test" || cmd[3] != "--stdio" {
		t.Errorf("npm command = %v", cmd)
	}
	if len(envVars) != 1 || envVars[0].Name != "KEY" {
		t.Errorf("envVars = %v", envVars)
	}
}

func TestDeriveCommand_RuntimeCommand(t *testing.T) {
	srv := &registry.RegistryServer{
		Name: "test-server",
		VersionDetail: registry.VersionDetail{
			Packages: []registry.Package{{
				RegistryType: "docker",
				Name:         "example/mcp",
				Runtime:      registry.RuntimeConfig{Command: "docker", Args: []string{"run", "example/mcp"}},
			}},
		},
	}
	cmd, _ := deriveCommand(srv)
	if len(cmd) != 3 || cmd[0] != "docker" {
		t.Errorf("runtime command = %v", cmd)
	}
}

func TestDeriveCommand_FallbackToName(t *testing.T) {
	srv := &registry.RegistryServer{
		Name:          "my-server",
		VersionDetail: registry.VersionDetail{},
	}
	cmd, envVars := deriveCommand(srv)
	if len(cmd) != 1 || cmd[0] != "my-server" {
		t.Errorf("fallback command = %v", cmd)
	}
	if envVars != nil {
		t.Errorf("fallback envVars should be nil, got %v", envVars)
	}
}

func TestDeriveCommand_NpmWithoutArgs(t *testing.T) {
	srv := &registry.RegistryServer{
		Name: "test-server",
		VersionDetail: registry.VersionDetail{
			Packages: []registry.Package{{
				RegistryType: "npm",
				Name:         "@example/simple",
				Runtime:      registry.RuntimeConfig{},
			}},
		},
	}
	cmd, _ := deriveCommand(srv)
	if len(cmd) != 3 || cmd[0] != "npx" || cmd[1] != "-y" || cmd[2] != "@example/simple" {
		t.Errorf("npm command without args = %v", cmd)
	}
}

// --- Additional error path coverage ---

func TestGetMCPServer_Nonexistent(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.GetMCPServer(w, mcpRequest("GET", "/v1/mcp-servers/nonexistent", "", userAClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestGetEnterpriseMCPServer_Nonexistent(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.GetEnterpriseMCPServer(w, mcpRequest("GET", "/v1/enterprise/mcp-servers/nonexistent", "", adminClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDeleteEnterpriseMCPServer_Nonexistent(t *testing.T) {
	srv := newMCPServer()

	w := httptest.NewRecorder()
	srv.DeleteEnterpriseMCPServer(w, mcpRequest("DELETE", "/v1/enterprise/mcp-servers/nonexistent", "", adminClaims), "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestEnterpriseMCPToUserView_WithPolicyMapping(t *testing.T) {
	prefix := "git"
	es := api.EnterpriseMCPServer{
		Id:      ptrS("emcp_1"),
		Name:    "test",
		Command: []string{"cmd"},
		Mode:    ptrMode(api.EnterpriseMCPServerModeRemote),
		PolicyMapping: &struct {
			ToolPrefix *string `json:"tool_prefix,omitempty"`
		}{ToolPrefix: &prefix},
	}
	source := api.MCPServerConfigSourceEnterprise
	cfg := enterpriseMCPToUserView(es, source)

	if cfg.PolicyMapping == nil || cfg.PolicyMapping.ToolPrefix == nil || *cfg.PolicyMapping.ToolPrefix != "git" {
		t.Errorf("PolicyMapping.ToolPrefix = %v, want git", cfg.PolicyMapping)
	}
	if cfg.Mode == nil || *cfg.Mode != api.MCPServerConfigModeRemote {
		t.Errorf("Mode = %v, want remote", cfg.Mode)
	}
}

func ptrS(s string) *string { return &s }
func ptrMode(m api.EnterpriseMCPServerMode) *api.EnterpriseMCPServerMode { return &m }

// --- Error-injecting store for internal error paths ---

type errorMCPServerStore struct {
	*mem.MCPServerStore
	listErr   error
	getErr    error
	createErr error
	updateErr error
	deleteErr error
}

func (s *errorMCPServerStore) List(_ context.Context, _ store.MCPServerFilter) ([]api.MCPServerConfig, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MCPServerStore.List(context.Background(), store.MCPServerFilter{})
}

func (s *errorMCPServerStore) Get(_ context.Context, id string) (api.MCPServerConfig, error) {
	if s.getErr != nil {
		return api.MCPServerConfig{}, s.getErr
	}
	return s.MCPServerStore.Get(context.Background(), id)
}

func (s *errorMCPServerStore) Create(_ context.Context, srv api.MCPServerConfig) error {
	if s.createErr != nil {
		return s.createErr
	}
	return s.MCPServerStore.Create(context.Background(), srv)
}

func (s *errorMCPServerStore) Update(_ context.Context, srv api.MCPServerConfig) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.MCPServerStore.Update(context.Background(), srv)
}

func (s *errorMCPServerStore) Delete(_ context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.MCPServerStore.Delete(context.Background(), id)
}

type errorEntMCPServerStore struct {
	*mem.EnterpriseMCPServerStore
	listErr   error
	getErr    error
	createErr error
	updateErr error
	deleteErr error
}

func (s *errorEntMCPServerStore) List(_ context.Context, _ store.EnterpriseMCPServerFilter) ([]api.EnterpriseMCPServer, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.EnterpriseMCPServerStore.List(context.Background(), store.EnterpriseMCPServerFilter{})
}

func (s *errorEntMCPServerStore) Get(_ context.Context, id string) (api.EnterpriseMCPServer, error) {
	if s.getErr != nil {
		return api.EnterpriseMCPServer{}, s.getErr
	}
	return s.EnterpriseMCPServerStore.Get(context.Background(), id)
}

func (s *errorEntMCPServerStore) Create(_ context.Context, srv api.EnterpriseMCPServer) error {
	if s.createErr != nil {
		return s.createErr
	}
	return s.EnterpriseMCPServerStore.Create(context.Background(), srv)
}

func (s *errorEntMCPServerStore) Update(_ context.Context, srv api.EnterpriseMCPServer) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.EnterpriseMCPServerStore.Update(context.Background(), srv)
}

func (s *errorEntMCPServerStore) Delete(_ context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.EnterpriseMCPServerStore.Delete(context.Background(), id)
}

func TestMCPServer_InternalErrors(t *testing.T) {
	internalErr := fmt.Errorf("database connection failed")

	t.Run("ListMCPServers store error", func(t *testing.T) {
		srv := newMCPServer()
		srv.mcpServers = &errorMCPServerStore{MCPServerStore: mem.NewMCPServerStore(), listErr: internalErr}
		w := httptest.NewRecorder()
		srv.ListMCPServers(w, mcpRequest("GET", "/v1/mcp-servers", "", userAClaims))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("CreateMCPServer store error", func(t *testing.T) {
		srv := newMCPServer()
		srv.mcpServers = &errorMCPServerStore{MCPServerStore: mem.NewMCPServerStore(), createErr: internalErr}
		w := httptest.NewRecorder()
		srv.CreateMCPServer(w, mcpRequest("POST", "/v1/mcp-servers", `{"name":"x","command":["x"]}`, userAClaims))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("GetMCPServer store error", func(t *testing.T) {
		srv := newMCPServer()
		srv.mcpServers = &errorMCPServerStore{MCPServerStore: mem.NewMCPServerStore(), getErr: internalErr}
		w := httptest.NewRecorder()
		srv.GetMCPServer(w, mcpRequest("GET", "/v1/mcp-servers/x", "", userAClaims), "x")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("UpdateMCPServer store get error", func(t *testing.T) {
		srv := newMCPServer()
		srv.mcpServers = &errorMCPServerStore{MCPServerStore: mem.NewMCPServerStore(), getErr: internalErr}
		w := httptest.NewRecorder()
		srv.UpdateMCPServer(w, mcpRequest("PUT", "/v1/mcp-servers/x", `{"name":"x","command":["x"]}`, userAClaims), "x")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("DeleteMCPServer store get error", func(t *testing.T) {
		srv := newMCPServer()
		srv.mcpServers = &errorMCPServerStore{MCPServerStore: mem.NewMCPServerStore(), getErr: internalErr}
		w := httptest.NewRecorder()
		srv.DeleteMCPServer(w, mcpRequest("DELETE", "/v1/mcp-servers/x", "", userAClaims), "x")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("SetMCPServerCredential store get error", func(t *testing.T) {
		srv := newMCPServer()
		srv.mcpServers = &errorMCPServerStore{MCPServerStore: mem.NewMCPServerStore(), getErr: internalErr}
		w := httptest.NewRecorder()
		srv.SetMCPServerCredential(w, mcpRequest("POST", "/v1/mcp-servers/x/credentials",
			`{"env_var_name":"K","secret_value":"V"}`, userAClaims), "x")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}

func TestEnterpriseMCPServer_InternalErrors(t *testing.T) {
	internalErr := fmt.Errorf("database connection failed")

	t.Run("ListEnterpriseMCPServers store error", func(t *testing.T) {
		srv := newMCPServer()
		srv.enterpriseMCPServers = &errorEntMCPServerStore{EnterpriseMCPServerStore: mem.NewEnterpriseMCPServerStore(), listErr: internalErr}
		w := httptest.NewRecorder()
		srv.ListEnterpriseMCPServers(w, mcpRequest("GET", "/v1/enterprise/mcp-servers", "", adminClaims))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("CreateEnterpriseMCPServer store error", func(t *testing.T) {
		srv := newMCPServer()
		srv.enterpriseMCPServers = &errorEntMCPServerStore{EnterpriseMCPServerStore: mem.NewEnterpriseMCPServerStore(), createErr: internalErr}
		w := httptest.NewRecorder()
		srv.CreateEnterpriseMCPServer(w, mcpRequest("POST", "/v1/enterprise/mcp-servers",
			`{"name":"x","command":["x"]}`, adminClaims))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("GetEnterpriseMCPServer store error", func(t *testing.T) {
		srv := newMCPServer()
		srv.enterpriseMCPServers = &errorEntMCPServerStore{EnterpriseMCPServerStore: mem.NewEnterpriseMCPServerStore(), getErr: internalErr}
		w := httptest.NewRecorder()
		srv.GetEnterpriseMCPServer(w, mcpRequest("GET", "/v1/enterprise/mcp-servers/x", "", adminClaims), "x")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("UpdateEnterpriseMCPServer store get error", func(t *testing.T) {
		srv := newMCPServer()
		srv.enterpriseMCPServers = &errorEntMCPServerStore{EnterpriseMCPServerStore: mem.NewEnterpriseMCPServerStore(), getErr: internalErr}
		w := httptest.NewRecorder()
		srv.UpdateEnterpriseMCPServer(w, mcpRequest("PUT", "/v1/enterprise/mcp-servers/x",
			`{"name":"x","command":["x"]}`, adminClaims), "x")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("DeleteEnterpriseMCPServer store get error", func(t *testing.T) {
		srv := newMCPServer()
		srv.enterpriseMCPServers = &errorEntMCPServerStore{EnterpriseMCPServerStore: mem.NewEnterpriseMCPServerStore(), getErr: internalErr}
		w := httptest.NewRecorder()
		srv.DeleteEnterpriseMCPServer(w, mcpRequest("DELETE", "/v1/enterprise/mcp-servers/x", "", adminClaims), "x")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("SetEnterpriseMCPServerCredential store get error", func(t *testing.T) {
		srv := newMCPServer()
		srv.enterpriseMCPServers = &errorEntMCPServerStore{EnterpriseMCPServerStore: mem.NewEnterpriseMCPServerStore(), getErr: internalErr}
		w := httptest.NewRecorder()
		srv.SetEnterpriseMCPServerCredential(w, mcpRequest("POST", "/v1/enterprise/mcp-servers/x/credentials",
			`{"env_var_name":"K","secret_value":"V"}`, adminClaims), "x")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}
