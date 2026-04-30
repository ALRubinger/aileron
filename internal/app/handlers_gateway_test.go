package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// Stage 2 contract (#368, ratified by ADR-0008):
//   - PostChatCompletions and PostMessages are transparent reverse
//     proxies to the configured upstream LLM provider.
//   - Path, method, body, and headers are forwarded unchanged.
//   - Streaming responses (SSE) round-trip without buffering beyond
//     what flushing requires.
//   - Upstream-emitted 4xx/5xx statuses propagate.
//   - Network/TLS failures map to a 502 with a structured Error body.
//   - When no upstream is configured the endpoint returns 503.
//
// Tests exercise the contract through the generated mux to guard
// against accidental decoupling between the spec, the wrapper, and
// the handler.

// --- helpers ---

func newGatewayTestServer(t *testing.T, openAIUpstream, anthropicUpstream *url.URL) *apiServer {
	t.Helper()
	s := &apiServer{log: slog.Default()}
	if openAIUpstream != nil {
		s.openAIProxy = newGatewayProxy(openAIUpstream, "openai", s.log)
	}
	if anthropicUpstream != nil {
		s.anthropicProxy = newGatewayProxy(anthropicUpstream, "anthropic", s.log)
	}
	return s
}

func muxFor(s *apiServer) *http.ServeMux {
	mux := http.NewServeMux()
	api.HandlerFromMux(s, mux)
	return mux
}

// --- not-configured edge ---

func TestPostChatCompletions_NotConfigured_Returns503(t *testing.T) {
	s := &apiServer{log: slog.Default()} // openAIProxy nil

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.PostChatCompletions(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	var resp api.Error
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "gateway_not_configured" {
		t.Errorf("error.code = %q, want gateway_not_configured", resp.Error.Code)
	}
}

func TestPostMessages_NotConfigured_Returns503(t *testing.T) {
	s := &apiServer{log: slog.Default()} // anthropicProxy nil

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.PostMessages(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- forwarding (path, method, body, headers) ---

func TestPostChatCompletions_ForwardsRequest(t *testing.T) {
	var captured struct {
		method string
		path   string
		auth   string
		ct     string
		body   []byte
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.ct = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		captured.body = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","model":"gpt-4","choices":[]}`))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	s := newGatewayTestServer(t, upstreamURL, nil)
	srv := httptest.NewServer(muxFor(s))
	t.Cleanup(srv.Close)

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-test-123")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if captured.method != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", captured.method)
	}
	if captured.path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", captured.path)
	}
	if captured.auth != "Bearer sk-test-123" {
		t.Errorf("upstream Authorization = %q, want forwarded bearer", captured.auth)
	}
	if captured.ct != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", captured.ct)
	}
	if string(captured.body) != reqBody {
		t.Errorf("upstream body = %q, want %q", captured.body, reqBody)
	}
}

func TestPostMessages_ForwardsAnthropicHeaders(t *testing.T) {
	var capturedAPIKey, capturedVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAPIKey = r.Header.Get("x-api-key")
		capturedVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[]}`))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	s := newGatewayTestServer(t, nil, upstreamURL)
	srv := httptest.NewServer(muxFor(s))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[]}`))
	req.Header.Set("x-api-key", "sk-ant-test-abc")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if capturedAPIKey != "sk-ant-test-abc" {
		t.Errorf("upstream x-api-key = %q, want forwarded", capturedAPIKey)
	}
	if capturedVersion != "2023-06-01" {
		t.Errorf("upstream anthropic-version = %q, want forwarded", capturedVersion)
	}
}

// --- non-streaming round-trip ---

func TestPostChatCompletions_NonStreaming_RoundTripsBody(t *testing.T) {
	upstreamPayload := `{"id":"chatcmpl-9","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(upstreamPayload))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	s := newGatewayTestServer(t, upstreamURL, nil)
	srv := httptest.NewServer(muxFor(s))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if string(got) != upstreamPayload {
		t.Errorf("body round-trip mismatch:\n got %q\nwant %q", got, upstreamPayload)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// --- streaming SSE ---

func TestPostChatCompletions_Streaming_PassesThroughChunks(t *testing.T) {
	chunks := []string{
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hel"}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"lo"}}]}`,
		`data: [DONE]`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream test response does not support flushing")
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	s := newGatewayTestServer(t, upstreamURL, nil)
	srv := httptest.NewServer(muxFor(s))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","messages":[],"stream":true}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	var got []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			got = append(got, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != len(chunks) {
		t.Fatalf("got %d data lines, want %d (got=%v)", len(got), len(chunks), got)
	}
	for i, want := range chunks {
		if got[i] != want {
			t.Errorf("chunk %d = %q, want %q", i, got[i], want)
		}
	}
}

// --- error propagation ---

func TestPostChatCompletions_UpstreamErrorStatusPropagates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	s := newGatewayTestServer(t, upstreamURL, nil)
	srv := httptest.NewServer(muxFor(s))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (upstream error must propagate)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("Invalid API key")) {
		t.Errorf("body = %q, want to contain upstream error message", body)
	}
}

func TestPostChatCompletions_UpstreamUnreachable_Returns502(t *testing.T) {
	// Bind a server, capture its URL, then close it so dial attempts fail.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL, _ := url.Parse(upstream.URL)
	upstream.Close()

	s := newGatewayTestServer(t, upstreamURL, nil)
	srv := httptest.NewServer(muxFor(s))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	var apiErr api.Error
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if apiErr.Error.Code != "upstream_error" {
		t.Errorf("error.code = %q, want upstream_error", apiErr.Error.Code)
	}
}
