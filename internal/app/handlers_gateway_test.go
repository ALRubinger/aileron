package app

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// Stage 1 contract: both gateway endpoints are registered, accept JSON
// request bodies that conform to the OpenAPI schemas, and return
// 501 Not Implemented with a structured error response. Subsequent
// stages will replace these tests as real behavior lands.
//
// Per ADR-0008, the gateway must accept the OpenAI Chat Completions
// shape (`/v1/chat/completions`) and the Anthropic Messages shape
// (`/v1/messages`). The 501 placeholder is documented in the OpenAPI
// spec — see the responses block on each endpoint.

func TestPostChatCompletions_Stub_Returns501(t *testing.T) {
	s := &apiServer{log: slog.Default()}

	body := strings.NewReader(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "hello"}]
	}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.PostChatCompletions(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var resp api.Error
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error.Code != "not_implemented" {
		t.Errorf("error.code = %q, want not_implemented", resp.Error.Code)
	}
	if resp.Error.Message == "" {
		t.Error("error.message must not be empty")
	}
}

func TestPostMessages_Stub_Returns501(t *testing.T) {
	s := &apiServer{log: slog.Default()}

	body := strings.NewReader(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "hello"}]
	}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.PostMessages(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var resp api.Error
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error.Code != "not_implemented" {
		t.Errorf("error.code = %q, want not_implemented", resp.Error.Code)
	}
	if resp.Error.Message == "" {
		t.Error("error.message must not be empty")
	}
}

// Routes are registered through the generated HandlerFromMux. Verify
// the wrapper actually dispatches to the stubs — guards against the
// route being silently dropped from the spec or the codegen output.
func TestGatewayRoutes_RegisteredOnMux(t *testing.T) {
	mux := http.NewServeMux()
	api.HandlerFromMux(&apiServer{log: slog.Default()}, mux)

	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "messages",
			path: "/v1/messages",
			body: `{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			mux.ServeHTTP(w, r)

			if w.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501 (route may not be registered)", w.Code)
			}
		})
	}
}
