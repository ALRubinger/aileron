package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/vault"
)

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// Per #492 item 6a, NewHandler no longer falls back to an
	// implicit dev vault. Tests that don't care about credential
	// behavior pass an explicit MemVault to satisfy the new contract.
	handler, err := NewHandlerWithConfig(log, Config{Vault: vault.NewMemVault()})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	return handler
}

func TestNewHandler_ReturnsNonNil(t *testing.T) {
	handler := newTestHandler(t)
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestNewHandler_HealthEndpoint(t *testing.T) {
	handler := newTestHandler(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want %q", body["status"], "ok")
	}
	if body["service"] != "aileron" {
		t.Errorf("service = %v, want %q", body["service"], "aileron")
	}
}

func TestNewHandler_LocalDaemonTokenProtectsV1(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := NewHandlerWithConfig(log, Config{
		Vault:            vault.NewMemVault(),
		LocalDaemonToken: "tok_test",
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatalf("GET /v1/status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want 401", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok_test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/status with token: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status with token = %d, want 200", resp.StatusCode)
	}
}

func TestNewHandler_LocalDaemonHandshakeSetsCookie(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := NewHandlerWithConfig(log, Config{
		Vault:            vault.NewMemVault(),
		LocalDaemonToken: "tok_cookie",
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := &http.Client{}
	resp, err := client.Get(srv.URL + "/v1/auth/handshake")
	if err != nil {
		t.Fatalf("GET /v1/auth/handshake: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("handshake status = %d, want 200", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == localDaemonTokenCookie {
			cookie = c
			break
		}
	}
	if cookie == nil || cookie.Value != "tok_cookie" {
		t.Fatalf("daemon token cookie = %#v, want tok_cookie", cookie)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/status with cookie: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status with cookie = %d, want 200", resp.StatusCode)
	}
}

func TestNewHandler_LocalDaemonHandshakeDisabled(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := NewHandlerWithConfig(log, Config{Vault: vault.NewMemVault()})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/handshake", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("handshake status = %d, want 204", rr.Code)
	}
	if got := rr.Result().Cookies(); len(got) != 0 {
		t.Fatalf("cookies = %#v, want none", got)
	}
}

func TestNewHandler_LocalDaemonHandshakeRejectsAbsoluteHostMismatch(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := NewHandlerWithConfig(log, Config{
		Vault:            vault.NewMemVault(),
		LocalDaemonToken: "tok_cookie",
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://daemon.test/v1/auth/handshake", nil)
	req.Host = "other.test"
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("handshake status = %d, want 403", rr.Code)
	}
}

func TestNewHandler_CreateIntentAndPolicyEvaluation(t *testing.T) {
	handler := newTestHandler(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Submit an intent for a PR to a feature branch (should be allowed).
	intentBody := `{
		"workspace_id": "default",
		"agent_id": "test_agent",
		"idempotency_key": "test-1",
		"action": {
			"type": "git.pull_request.create",
			"summary": "Add tests",
			"domain": {
				"git": {
					"provider": "github",
					"repository": "acme/checkout",
					"branch": "add-tests",
					"base_branch": "develop"
				}
			}
		}
	}`

	resp, err := http.Post(srv.URL+"/v1/intents", "application/json", strings.NewReader(intentBody))
	if err != nil {
		t.Fatalf("POST /v1/intents: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, http.StatusCreated, body)
	}

	var intent map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&intent); err != nil {
		t.Fatalf("decode intent: %v", err)
	}

	// Feature branch PRs should be allowed by default seed policies.
	decision, ok := intent["decision"].(map[string]any)
	if !ok {
		t.Fatal("expected decision in response")
	}
	if decision["disposition"] != "allow" {
		t.Errorf("disposition = %v, want %q", decision["disposition"], "allow")
	}
	// Should have a grant ID since it was auto-approved.
	if decision["execution_grant_id"] == nil {
		t.Error("expected execution_grant_id for allowed intent")
	}
}

func TestNewHandler_IntentToMainRequiresApproval(t *testing.T) {
	handler := newTestHandler(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Submit an intent for a PR to main (should require approval).
	intentBody := `{
		"workspace_id": "default",
		"agent_id": "test_agent",
		"idempotency_key": "test-2",
		"action": {
			"type": "git.pull_request.create",
			"summary": "Deploy to production",
			"domain": {
				"git": {
					"provider": "github",
					"repository": "acme/checkout",
					"branch": "feat/deploy",
					"base_branch": "main"
				}
			}
		}
	}`

	resp, err := http.Post(srv.URL+"/v1/intents", "application/json", strings.NewReader(intentBody))
	if err != nil {
		t.Fatalf("POST /v1/intents: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, http.StatusCreated, body)
	}

	var intent map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&intent); err != nil {
		t.Fatalf("decode intent: %v", err)
	}

	decision, ok := intent["decision"].(map[string]any)
	if !ok {
		t.Fatal("expected decision in response")
	}
	if decision["disposition"] != "require_approval" {
		t.Errorf("disposition = %v, want %q", decision["disposition"], "require_approval")
	}
	if decision["approval_id"] == nil {
		t.Error("expected approval_id for intent requiring approval")
	}
}

func TestNewHandler_DocsEndpoint(t *testing.T) {
	handler := newTestHandler(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/docs")
	if err != nil {
		t.Fatalf("GET /docs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestNewHandler_OpenAPISpec(t *testing.T) {
	handler := newTestHandler(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/openapi.yaml")
	if err != nil {
		t.Fatalf("GET /openapi.yaml: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
}

func TestNewHandler_CORSHeaders(t *testing.T) {
	handler := newTestHandler(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// CORS headers are only set when an Origin header is present.
	req, err := http.NewRequest("GET", srv.URL+"/v1/health", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Origin", "https://app.withaileron.ai")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.withaileron.ai" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "https://app.withaileron.ai")
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want %q", got, "true")
	}
}

func TestNewHandler_RequestIDMiddleware(t *testing.T) {
	handler := newTestHandler(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Without client-provided ID, server should generate one.
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Request-ID"); got == "" {
		t.Error("expected X-Request-ID header to be set")
	}

	// With client-provided ID, server should echo it back.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/health", nil)
	req.Header.Set("X-Request-ID", "my-request-id")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/health with X-Request-ID: %v", err)
	}
	defer resp2.Body.Close()

	if got := resp2.Header.Get("X-Request-ID"); got != "my-request-id" {
		t.Errorf("X-Request-ID = %q, want %q", got, "my-request-id")
	}
}

func TestNewMailer_Resend(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config.AuthConfig{
		ResendAPIKey: "re_test_key",
		MailFrom:     "hello@example.com",
	}

	m := newMailer(log, cfg)
	if _, ok := m.(*auth.ResendMailer); !ok {
		t.Errorf("expected *auth.ResendMailer, got %T", m)
	}
}

func TestNewMailer_LogFallback(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config.AuthConfig{}

	m := newMailer(log, cfg)
	if _, ok := m.(*auth.LogMailer); !ok {
		t.Errorf("expected *auth.LogMailer, got %T", m)
	}
}
