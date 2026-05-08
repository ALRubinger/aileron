package intercept

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/retry"
)

// gzipBytes returns the gzip-compressed encoding of s — used by tests
// that simulate an upstream that honored `Accept-Encoding: gzip`.
func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// --- decompressUpstreamBody ---

func TestDecompressUpstreamBody_GzipDecodesAndStripsHeader(t *testing.T) {
	plain := `{"type":"message","content":[]}`
	encoded := gzipBytes(t, plain)
	in := http.Header{}
	in.Set("Content-Encoding", "gzip")
	in.Set("Content-Length", "999")
	in.Set("X-Pass-Through", "keep")

	body, headers, err := decompressUpstreamBody(in, encoded)
	if err != nil {
		t.Fatalf("decompressUpstreamBody: %v", err)
	}
	if string(body) != plain {
		t.Errorf("body = %q, want %q", body, plain)
	}
	if got := headers.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding still present: %q", got)
	}
	if got := headers.Get("Content-Length"); got != "31" {
		t.Errorf("Content-Length = %q, want %d", got, len(plain))
	}
	if got := headers.Get("X-Pass-Through"); got != "keep" {
		t.Errorf("unrelated header dropped; X-Pass-Through = %q", got)
	}
	// Helper must not mutate the caller's input headers.
	if in.Get("Content-Encoding") != "gzip" {
		t.Error("input headers mutated")
	}
}

func TestDecompressUpstreamBody_IdentityPassThrough(t *testing.T) {
	plain := []byte(`{"type":"message"}`)
	in := http.Header{}
	in.Set("Content-Encoding", "identity")

	body, headers, err := decompressUpstreamBody(in, plain)
	if err != nil {
		t.Fatalf("decompressUpstreamBody: %v", err)
	}
	if !bytes.Equal(body, plain) {
		t.Errorf("body changed; got %q", body)
	}
	if headers.Get("Content-Encoding") != "identity" {
		t.Error("identity header should be preserved when unchanged")
	}
}

func TestDecompressUpstreamBody_NoEncodingHeader(t *testing.T) {
	plain := []byte(`{"type":"message"}`)
	body, _, err := decompressUpstreamBody(http.Header{}, plain)
	if err != nil {
		t.Fatalf("decompressUpstreamBody: %v", err)
	}
	if !bytes.Equal(body, plain) {
		t.Errorf("body changed; got %q", body)
	}
}

func TestDecompressUpstreamBody_UnknownEncodingPassThrough(t *testing.T) {
	// Brotli / zstd / deflate aren't supported. We pass through
	// unchanged so the existing parse-failure path logs and the
	// agent's SDK can still try to decode if it knows the encoding.
	in := http.Header{}
	in.Set("Content-Encoding", "br")
	original := []byte("opaque-brotli-bytes")

	body, headers, err := decompressUpstreamBody(in, original)
	if err != nil {
		t.Fatalf("decompressUpstreamBody: %v", err)
	}
	if !bytes.Equal(body, original) {
		t.Error("body changed for unknown encoding")
	}
	if headers.Get("Content-Encoding") != "br" {
		t.Error("Content-Encoding stripped for unknown encoding")
	}
}

func TestDecompressUpstreamBody_GzipMixedCaseAndWhitespace(t *testing.T) {
	plain := `{"ok":true}`
	in := http.Header{}
	in.Set("Content-Encoding", "  GZIP  ")

	body, _, err := decompressUpstreamBody(in, gzipBytes(t, plain))
	if err != nil {
		t.Fatalf("decompressUpstreamBody: %v", err)
	}
	if string(body) != plain {
		t.Errorf("body = %q, want %q", body, plain)
	}
}

func TestDecompressUpstreamBody_GzipEmptyBodyIsNoOp(t *testing.T) {
	in := http.Header{}
	in.Set("Content-Encoding", "gzip")
	body, headers, err := decompressUpstreamBody(in, nil)
	if err != nil {
		t.Fatalf("expected no error on empty gzip body; got %v", err)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
	if headers.Get("Content-Encoding") != "gzip" {
		t.Error("empty-body short-circuit should not strip headers")
	}
}

func TestDecompressUpstreamBody_GzipMalformedReturnsError(t *testing.T) {
	in := http.Header{}
	in.Set("Content-Encoding", "gzip")
	if _, _, err := decompressUpstreamBody(in, []byte("not-actually-gzip")); err == nil {
		t.Fatal("expected error on malformed gzip body")
	}
}

// --- truncateForLog ---

func TestTruncateForLog_RespectsLimit(t *testing.T) {
	got := truncateForLog("abcdefghij", 4)
	if got != "abcd…" {
		t.Errorf("got %q, want abcd…", got)
	}
}

func TestTruncateForLog_PassesThroughShort(t *testing.T) {
	if got := truncateForLog("abc", 10); got != "abc" {
		t.Errorf("short string got truncated: %q", got)
	}
}

// --- Upstream-visibility logging (#532 finding 7b) ---

// engineWithCapturedLogger builds an Engine wired to capture slog
// output to buf, used for asserting on Warn-level events the
// production handlers emit on upstream non-success and unparseable-
// 200 bodies.
func engineWithCapturedLogger(t *testing.T, openAIURL, anthropicURL string, store *action.Store, buf *bytes.Buffer) *Engine {
	t.Helper()
	var oa, an *url.URL
	if openAIURL != "" {
		u, _ := url.Parse(openAIURL)
		oa = u
	}
	if anthropicURL != "" {
		u, _ := url.Parse(anthropicURL)
		an = u
	}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e, err := New(Config{
		OpenAIUpstream:    oa,
		AnthropicUpstream: an,
		Actions:           store,
		Executor:          action.StubExecutor{},
		Recorder:          audit.NewRecorder(audit.NewMemStore(), nil, nil),
		Log:               logger,
		RetryPolicy:       retry.Policy{MaxRetries: 0, BaseDelay: time.Millisecond, Jitter: 0},
	})
	if err != nil {
		t.Fatalf("intercept.New: %v", err)
	}
	return e
}

// TestHandleAnthropic_LogsUnparseable200Body pins finding #7b: when
// upstream returns an HTTP 200 carrying a body that isn't a usable
// message response — Anthropic's `{"type":"error","error":{...}}`
// envelope is the motivating case (overloaded_error /
// rate_limit_error inside an otherwise-successful stream) — the
// gateway must log a WARN with the parse error and a body preview so
// the operator sees the upstream cause in daemon.log. Without this
// signal a 200 that took 72s looks identical to a slow-but-success-
// ful round.
//
// The contract is provider-agnostic: any unparseable success body
// (error envelope or genuinely malformed JSON) triggers the log.
// This test pins the motivating shape; the handler does not depend
// on it.
func TestHandleAnthropic_LogsUnparseable200Body(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_test_42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"API overloaded"}}`))
	}))
	t.Cleanup(srv.Close)

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})

	var logs bytes.Buffer
	e := engineWithCapturedLogger(t, "", srv.URL, store, &logs)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	out := logs.String()
	for _, want := range []string{
		"upstream returned unparseable success body",
		"protocol=anthropic",
		"request_id=req_test_42",
		// body preview must surface the upstream error type for grep
		// without requiring the gateway to know Anthropic's shape:
		"overloaded_error",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in log, got:\n%s", want, out)
		}
	}
}

// TestHandleAnthropic_LogsUpstreamNonSuccess pins the second half of
// finding #7b: a non-200 upstream response must also surface as a
// structured WARN event with status + request_id, not just an OTel
// span attribute (operators without OTel enabled were getting no
// in-band signal at all).
//
// Uses 401 because 429/5xx are retryable failures wrapped via
// failure.Upstream and surfaced via writeFailure — the
// "non-200 passthrough" branch exercised here is for 4xx non-429.
func TestHandleAnthropic_LogsUpstreamNonSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_test_401")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	t.Cleanup(srv.Close)

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	var logs bytes.Buffer
	e := engineWithCapturedLogger(t, "", srv.URL, store, &logs)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	out := logs.String()
	for _, want := range []string{
		"upstream non-success",
		"protocol=anthropic",
		"upstream_status=401",
		"request_id=req_test_401",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in log, got:\n%s", want, out)
		}
	}
}

// TestHandleOpenAI_LogsUpstreamNonSuccess mirrors the Anthropic
// non-success-logging contract for the OpenAI path. Same operator-
// visibility argument applies.
func TestHandleOpenAI_LogsUpstreamNonSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req_oa_401")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	t.Cleanup(srv.Close)

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	var logs bytes.Buffer
	e := engineWithCapturedLogger(t, srv.URL, "", store, &logs)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	out := logs.String()
	for _, want := range []string{
		"upstream non-success",
		"protocol=openai",
		"upstream_status=401",
		"request_id=req_oa_401",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in log, got:\n%s", want, out)
		}
	}
}
