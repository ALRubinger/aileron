package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/auth"
	"github.com/golang-jwt/jwt/v5"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func newTestLLMConfigServer() *apiServer {
	return &apiServer{
		llmConfigs: mem.NewLLMConfigStore(),
		vault:      vault.NewMemVault(),
		users:      nil, // nil = auth disabled (dev mode), requireAuth allows all
		newID:      func() string { return "test-id" },
	}
}

func TestHandleUpsertUserLLMConfig(t *testing.T) {
	s := newTestLLMConfigServer()

	body := `{"provider":"anthropic","model_research":"haiku","model_synthesis":"sonnet","api_key":"sk-test"}`
	req := httptest.NewRequest("PUT", "/v1/llm-config", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	s.handleUpsertUserLLMConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["provider"] != "anthropic" {
		t.Errorf("expected anthropic, got %v", resp["provider"])
	}
	if resp["api_key_configured"] != true {
		t.Error("expected api_key_configured=true")
	}
	// API key must NOT be in response.
	if _, ok := resp["api_key"]; ok {
		t.Error("api_key should not be in response")
	}
}

func TestHandleUpsertUserLLMConfig_MissingProvider(t *testing.T) {
	s := newTestLLMConfigServer()

	body := `{"api_key":"sk-test"}`
	req := httptest.NewRequest("PUT", "/v1/llm-config", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	s.handleUpsertUserLLMConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleUpsertUserLLMConfig_MissingAPIKey(t *testing.T) {
	s := newTestLLMConfigServer()

	body := `{"provider":"anthropic"}`
	req := httptest.NewRequest("PUT", "/v1/llm-config", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	s.handleUpsertUserLLMConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleGetUserLLMConfig(t *testing.T) {
	s := newTestLLMConfigServer()

	// First create a config.
	body := `{"provider":"anthropic","model_research":"haiku","model_synthesis":"sonnet","api_key":"sk-test"}`
	createReq := httptest.NewRequest("PUT", "/v1/llm-config", bytes.NewBufferString(body))
	createW := httptest.NewRecorder()
	s.handleUpsertUserLLMConfig(createW, createReq)

	// Then get it.
	req := httptest.NewRequest("GET", "/v1/llm-config", nil)
	w := httptest.NewRecorder()
	s.handleGetUserLLMConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["provider"] != "anthropic" {
		t.Errorf("expected anthropic, got %v", resp["provider"])
	}
}

func TestHandleGetUserLLMConfig_NotFound(t *testing.T) {
	s := newTestLLMConfigServer()

	req := httptest.NewRequest("GET", "/v1/llm-config", nil)
	w := httptest.NewRecorder()
	s.handleGetUserLLMConfig(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleDeleteUserLLMConfig(t *testing.T) {
	s := newTestLLMConfigServer()

	// Create a config.
	body := `{"provider":"anthropic","model_research":"haiku","model_synthesis":"sonnet","api_key":"sk-test"}`
	createReq := httptest.NewRequest("PUT", "/v1/llm-config", bytes.NewBufferString(body))
	createW := httptest.NewRecorder()
	s.handleUpsertUserLLMConfig(createW, createReq)

	// Delete it.
	req := httptest.NewRequest("DELETE", "/v1/llm-config", nil)
	w := httptest.NewRecorder()
	s.handleDeleteUserLLMConfig(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify it's gone.
	getReq := httptest.NewRequest("GET", "/v1/llm-config", nil)
	getW := httptest.NewRecorder()
	s.handleGetUserLLMConfig(getW, getReq)

	if getW.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", getW.Code)
	}
}

func TestHandleDeleteUserLLMConfig_NotFound(t *testing.T) {
	s := newTestLLMConfigServer()

	req := httptest.NewRequest("DELETE", "/v1/llm-config", nil)
	w := httptest.NewRecorder()
	s.handleDeleteUserLLMConfig(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleUpsertUserLLMConfig_Upsert(t *testing.T) {
	s := newTestLLMConfigServer()

	// Create initial config.
	body := `{"provider":"anthropic","model_research":"haiku","model_synthesis":"sonnet","api_key":"sk-first"}`
	req := httptest.NewRequest("PUT", "/v1/llm-config", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	s.handleUpsertUserLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Update with different model.
	body2 := `{"provider":"anthropic","model_research":"haiku","model_synthesis":"opus","api_key":"sk-second"}`
	req2 := httptest.NewRequest("PUT", "/v1/llm-config", bytes.NewBufferString(body2))
	w2 := httptest.NewRecorder()
	s.handleUpsertUserLLMConfig(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}

	var resp map[string]any
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["model_synthesis"] != "opus" {
		t.Errorf("expected opus, got %v", resp["model_synthesis"])
	}
}

func TestExtractEnterpriseLLMConfigID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/enterprises/ent_123/llm-config", "ent_123"},
		{"/v1/enterprises//llm-config", ""},
		{"/v1/enterprises/ent_123", ""},
		{"/v1/llm-config", ""},
		{"/v1/enterprises/ent_123/nested/llm-config", ""},
	}

	for _, tt := range tests {
		got := extractEnterpriseLLMConfigID(tt.path)
		if got != tt.want {
			t.Errorf("extractEnterpriseLLMConfigID(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestRequireEnterpriseAdmin_DevMode(t *testing.T) {
	s := &apiServer{
		users: nil, // auth disabled
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	if !s.requireEnterpriseAdmin(w, req, "ent_1") {
		t.Error("expected true in dev mode (no users store)")
	}
}

func TestRequireEnterpriseAdmin_NonMember(t *testing.T) {
	users := mem.NewUserStore()
	users.Create(context.Background(), model.User{
		ID:           "usr_1",
		EnterpriseID: "ent_other",
		Email:        "test@example.com",
		Role:         model.UserRoleAdmin,
	})

	s := &apiServer{users: users}

	// Auth context with a different enterprise.
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_1"},
		EnterpriseID:     "ent_other",
	})
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	if s.requireEnterpriseAdmin(w, req, "ent_1") {
		t.Error("expected false for non-member")
	}
}

func TestRequireEnterpriseAdmin_MemberNotAdmin(t *testing.T) {
	users := mem.NewUserStore()
	users.Create(context.Background(), model.User{
		ID:           "usr_1",
		EnterpriseID: "ent_1",
		Email:        "test@example.com",
		Role:         model.UserRoleMember,
	})

	s := &apiServer{users: users}

	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_1"},
		EnterpriseID:     "ent_1",
	})
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	if s.requireEnterpriseAdmin(w, req, "ent_1") {
		t.Error("expected false for non-admin member")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRequireEnterpriseAdmin_Admin(t *testing.T) {
	users := mem.NewUserStore()
	users.Create(context.Background(), model.User{
		ID:           "usr_1",
		EnterpriseID: "ent_1",
		Email:        "test@example.com",
		Role:         model.UserRoleAdmin,
	})

	s := &apiServer{users: users}

	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_1"},
		EnterpriseID:     "ent_1",
	})
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	if !s.requireEnterpriseAdmin(w, req, "ent_1") {
		t.Error("expected true for admin")
	}
}

// --- Enterprise handler tests (with auth context) ---

func newTestEnterpriseLLMConfigServer() *apiServer {
	users := mem.NewUserStore()
	users.Create(context.Background(), model.User{
		ID:           "usr_admin",
		EnterpriseID: "ent_1",
		Email:        "admin@example.com",
		Role:         model.UserRoleAdmin,
	})
	return &apiServer{
		llmConfigs: mem.NewLLMConfigStore(),
		vault:      vault.NewMemVault(),
		users:      users,
		newID:      func() string { return "test-id" },
	}
}

func withAdminAuth(req *http.Request) *http.Request {
	ctx := auth.ContextWithClaims(req.Context(), &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_admin"},
		EnterpriseID:     "ent_1",
	})
	return req.WithContext(ctx)
}

func TestHandleUpsertEnterpriseLLMConfig(t *testing.T) {
	s := newTestEnterpriseLLMConfigServer()

	body := `{"provider":"anthropic","model_research":"haiku","model_synthesis":"sonnet","api_key":"sk-org"}`
	req := withAdminAuth(httptest.NewRequest("PUT", "/v1/enterprises/ent_1/llm-config", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()

	s.handleUpsertEnterpriseLLMConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["owner_type"] != "enterprise" {
		t.Errorf("expected enterprise, got %v", resp["owner_type"])
	}
	if resp["api_key_configured"] != true {
		t.Error("expected api_key_configured=true")
	}
}

func TestHandleUpsertEnterpriseLLMConfig_MissingProvider(t *testing.T) {
	s := newTestEnterpriseLLMConfigServer()

	body := `{"api_key":"sk-org"}`
	req := withAdminAuth(httptest.NewRequest("PUT", "/v1/enterprises/ent_1/llm-config", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()

	s.handleUpsertEnterpriseLLMConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleGetEnterpriseLLMConfig(t *testing.T) {
	s := newTestEnterpriseLLMConfigServer()

	// Create first.
	body := `{"provider":"anthropic","model_research":"haiku","model_synthesis":"sonnet","api_key":"sk-org"}`
	createReq := withAdminAuth(httptest.NewRequest("PUT", "/v1/enterprises/ent_1/llm-config", bytes.NewBufferString(body)))
	createW := httptest.NewRecorder()
	s.handleUpsertEnterpriseLLMConfig(createW, createReq)

	// Get.
	req := withAdminAuth(httptest.NewRequest("GET", "/v1/enterprises/ent_1/llm-config", nil))
	w := httptest.NewRecorder()
	s.handleGetEnterpriseLLMConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetEnterpriseLLMConfig_NotFound(t *testing.T) {
	s := newTestEnterpriseLLMConfigServer()

	req := withAdminAuth(httptest.NewRequest("GET", "/v1/enterprises/ent_1/llm-config", nil))
	w := httptest.NewRecorder()
	s.handleGetEnterpriseLLMConfig(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleDeleteEnterpriseLLMConfig(t *testing.T) {
	s := newTestEnterpriseLLMConfigServer()

	// Create.
	body := `{"provider":"anthropic","model_research":"haiku","model_synthesis":"sonnet","api_key":"sk-org"}`
	createReq := withAdminAuth(httptest.NewRequest("PUT", "/v1/enterprises/ent_1/llm-config", bytes.NewBufferString(body)))
	createW := httptest.NewRecorder()
	s.handleUpsertEnterpriseLLMConfig(createW, createReq)

	// Delete.
	req := withAdminAuth(httptest.NewRequest("DELETE", "/v1/enterprises/ent_1/llm-config", nil))
	w := httptest.NewRecorder()
	s.handleDeleteEnterpriseLLMConfig(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteEnterpriseLLMConfig_NotFound(t *testing.T) {
	s := newTestEnterpriseLLMConfigServer()

	req := withAdminAuth(httptest.NewRequest("DELETE", "/v1/enterprises/ent_1/llm-config", nil))
	w := httptest.NewRecorder()
	s.handleDeleteEnterpriseLLMConfig(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleUpsertEnterpriseLLMConfig_Forbidden(t *testing.T) {
	users := mem.NewUserStore()
	users.Create(context.Background(), model.User{
		ID:           "usr_member",
		EnterpriseID: "ent_1",
		Email:        "member@example.com",
		Role:         model.UserRoleMember,
	})
	s := &apiServer{
		llmConfigs: mem.NewLLMConfigStore(),
		vault:      vault.NewMemVault(),
		users:      users,
		newID:      func() string { return "test-id" },
	}

	body := `{"provider":"anthropic","api_key":"sk-org"}`
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_member"},
		EnterpriseID:     "ent_1",
	})
	req := httptest.NewRequest("PUT", "/v1/enterprises/ent_1/llm-config", bytes.NewBufferString(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleUpsertEnterpriseLLMConfig(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpsertEnterpriseLLMConfig_BadPath(t *testing.T) {
	s := newTestEnterpriseLLMConfigServer()

	body := `{"provider":"anthropic","api_key":"sk-org"}`
	req := withAdminAuth(httptest.NewRequest("PUT", "/v1/enterprises//llm-config", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	s.handleUpsertEnterpriseLLMConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleUpsertUserLLMConfig_InvalidJSON(t *testing.T) {
	s := newTestLLMConfigServer()

	req := httptest.NewRequest("PUT", "/v1/llm-config", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	s.handleUpsertUserLLMConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
