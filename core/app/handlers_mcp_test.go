package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
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
