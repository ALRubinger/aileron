package intercept

import (
	"bytes"
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
