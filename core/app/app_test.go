package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := NewHandler(log)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
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

	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "*")
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
