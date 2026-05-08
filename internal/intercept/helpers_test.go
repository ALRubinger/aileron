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

// --- peekAnthropicError contract (#532 finding 7b) ---

func TestPeekAnthropicError_DetectsErrorEnvelope(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"API overloaded"}}`)
	gotType, gotMsg, ok := peekAnthropicError(body)
	if !ok {
		t.Fatal("expected ok=true for valid Anthropic error envelope")
	}
	if gotType != "overloaded_error" {
		t.Errorf("type = %q, want overloaded_error", gotType)
	}
	if gotMsg != "API overloaded" {
		t.Errorf("message = %q, want %q", gotMsg, "API overloaded")
	}
}

func TestPeekAnthropicError_RejectsValidMessage(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[]}`)
	if _, _, ok := peekAnthropicError(body); ok {
		t.Error("peekAnthropicError accepted a valid message body — must only match type=error")
	}
}

func TestPeekAnthropicError_RejectsUnparseable(t *testing.T) {
	if _, _, ok := peekAnthropicError([]byte("not json")); ok {
		t.Error("peekAnthropicError accepted unparseable body")
	}
}

// --- peekOpenAIError contract ---

func TestPeekOpenAIError_DetectsErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"type":"rate_limit_exceeded","code":"rate_limit","message":"Slow down"}}`)
	gotType, gotMsg, ok := peekOpenAIError(body)
	if !ok {
		t.Fatal("expected ok=true for OpenAI error envelope")
	}
	if gotType != "rate_limit_exceeded" {
		t.Errorf("type = %q, want rate_limit_exceeded", gotType)
	}
	if gotMsg != "Slow down" {
		t.Errorf("message = %q", gotMsg)
	}
}

func TestPeekOpenAIError_FallsBackToCode(t *testing.T) {
	body := []byte(`{"error":{"code":"insufficient_quota","message":"out of credits"}}`)
	gotType, _, ok := peekOpenAIError(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if gotType != "insufficient_quota" {
		t.Errorf("type = %q, want insufficient_quota (fell back to code)", gotType)
	}
}

func TestPeekOpenAIError_RejectsValidChoices(t *testing.T) {
	// A successful chat completion may legitimately mention "error" in
	// content text or finish_reason metadata; the helper must not
	// classify it as an error envelope.
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"no error here"}}]}`)
	if _, _, ok := peekOpenAIError(body); ok {
		t.Error("peekOpenAIError accepted a successful completion as an error")
	}
}

func TestPeekOpenAIError_RejectsUnparseable(t *testing.T) {
	if _, _, ok := peekOpenAIError([]byte("garbage")); ok {
		t.Error("peekOpenAIError accepted unparseable body")
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

// --- HandleAnthropic logs upstream-error envelope inside HTTP 200 ---

// engineWithCapturedLogger builds an Engine wired to capture slog
// output to buf, used for asserting on Warn-level events the
// production handlers emit on upstream non-success and HTTP 200
// error envelopes.
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

// TestHandleAnthropic_LogsUpstreamErrorEnvelopeOn200 pins finding #7b:
// Anthropic's `{"type":"error","error":{...}}` body inside HTTP 200
// must produce a WARN-level log event so the operator sees the
// upstream cause in daemon.log instead of guessing from a 200-
// looking request that took 72s.
func TestHandleAnthropic_LogsUpstreamErrorEnvelopeOn200(t *testing.T) {
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
		"upstream error in HTTP 200 envelope",
		"protocol=anthropic",
		"upstream_error_type=overloaded_error",
		"request_id=req_test_42",
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
