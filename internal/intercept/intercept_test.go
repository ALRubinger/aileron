package intercept

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/retry"
)

// Stage 4 contract (#370, ratified by ADR-0008):
//
//   - When the upstream LLM emits a tool call targeting an
//     Aileron-augmented action, the engine intercepts it, executes the
//     action, injects the synthetic tool result, and continues with a
//     fresh upstream call until the LLM produces a final assistant
//     message.
//   - Tool calls for agent-declared tools pass through unchanged.
//   - Multi-turn (tool A → result → tool B → result → final) works.
//   - Action execution failures surface as tool-result errors per
//     ADR-0010, not as gateway 5xxs.
//   - Streaming preservation — tool-call deltas buffered until
//     complete; result injection doesn't break the agent's streaming
//     contract.
//
// The tests below drive the engine end-to-end against a stateful
// fake upstream so they exercise the same code path the live gateway
// will see.

// --- fixtures ---

const shipUpdateAction = `+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc"
capabilities = ["chat:write"]

[match]
intent = "tell team I shipped"

[[inputs]]
name = "channel"
type = "string"
description = "Slack channel."

[[execute]]
id = "post"
connector = "github://aileron/slack"
op = "post_message"

[execute.inputs]
channel = "${args.channel}"
+++

# Ship Update

Posts a 'shipped' announcement to a Slack channel.
`

const fileBugAction = `+++
name = "file-bug"
version = "1.0.0"
source = "hub://aileron/file-bug@1.0.0"

[[requires.connectors]]
name = "github://aileron/linear"
version = "1.0.0"
hash = "sha256:def"
capabilities = ["issues:write"]

[match]
intent = "file a bug"

[[inputs]]
name = "title"
type = "string"
description = "Issue title."

[[execute]]
id = "create"
connector = "github://aileron/linear"
op = "issue_create"

[execute.inputs]
title = "${args.title}"
+++

# File Bug
`

// loadStore writes the given manifests to a temp dir, loads them
// into a store, and returns the populated store.
func loadStore(t *testing.T, manifests map[string]string) *action.Store {
	t.Helper()
	dir := t.TempDir()
	for filename, body := range manifests {
		path := filepath.Join(dir, filename)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	s := action.NewStore(dir)
	res, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if res.HasErrors() {
		t.Fatalf("load errors: %+v", res.Errors)
	}
	return s
}

// staticExecutor returns a fixed Result for any call.
type staticExecutor struct {
	result action.Result
	calls  []struct {
		Name string
		Args map[string]any
	}
}

func (s *staticExecutor) Execute(_ context.Context, name string, args map[string]any) (action.Result, error) {
	s.calls = append(s.calls, struct {
		Name string
		Args map[string]any
	}{Name: name, Args: args})
	return s.result, nil
}

// scriptedUpstream serves a sequence of pre-canned JSON responses,
// one per request, capturing each request body as it goes. Tests use
// it to assert what the engine sent upstream and what it forwarded
// back to the agent.
type scriptedUpstream struct {
	t         *testing.T
	responses []string
	captured  []map[string]any
	calls     int
}

func newScriptedUpstream(t *testing.T, responses ...string) (*httptest.Server, *scriptedUpstream) {
	t.Helper()
	s := &scriptedUpstream{t: t, responses: responses}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s
}

func (s *scriptedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		s.t.Errorf("upstream received non-JSON body: %s", body)
	}
	s.captured = append(s.captured, decoded)
	idx := s.calls
	if idx >= len(s.responses) {
		s.t.Errorf("upstream call #%d but only %d scripted responses", idx+1, len(s.responses))
		http.Error(w, "no more responses", http.StatusInternalServerError)
		return
	}
	s.calls++
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(s.responses[idx]))
}

// engineFor builds an Engine wired to the given upstreams, action
// store, and executor. Defaults to StubExecutor when exec is nil.
func engineFor(t *testing.T, openAIURL, anthropicURL string, store *action.Store, exec action.Executor) *Engine {
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
	if exec == nil {
		exec = action.StubExecutor{}
	}
	// Tests use a tight retry policy so retried-network-error tests
	// don't pay real-time backoff; the in-memory recorder lets tests
	// query the audit log after the fact.
	e, err := New(Config{
		OpenAIUpstream:    oa,
		AnthropicUpstream: an,
		Actions:           store,
		Executor:          exec,
		Recorder:          audit.NewRecorder(audit.NewMemStore(), nil, nil),
		RetryPolicy:       retry.Policy{MaxRetries: 0, BaseDelay: time.Millisecond, Jitter: 0},
	})
	if err != nil {
		t.Fatalf("intercept.New: %v", err)
	}
	return e
}

// --- engine construction ---

func TestNew_RequiresActions(t *testing.T) {
	if _, err := New(Config{Executor: action.StubExecutor{}}); err == nil {
		t.Error("expected error when Actions is nil")
	}
}

func TestNew_RequiresExecutor(t *testing.T) {
	store := loadStore(t, nil)
	if _, err := New(Config{Actions: store}); err == nil {
		t.Error("expected error when Executor is nil")
	}
}

// --- OpenAI: gateway not configured ---

func TestHandleOpenAI_UpstreamNotConfigured_Returns503(t *testing.T) {
	store := loadStore(t, nil)
	e := engineFor(t, "", "", store, nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- OpenAI: passthrough when LLM picks no tool ---

func TestHandleOpenAI_NoToolCall_PassesResponseThrough(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello there!"}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, upstream.URL, "", store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello there!" {
		t.Errorf("content = %v, want passthrough", msg["content"])
	}
}

// --- OpenAI: tool-call interception (single round) ---

func TestHandleOpenAI_InterceptsAileronToolCall(t *testing.T) {
	// Round 1: LLM picks ship_update. Round 2: LLM produces final text.
	upstream, captured := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ship_update","arguments":"{\"channel\":\"#engineering\"}"}}]}}]}`,
		`{"id":"r2","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Posted to #engineering."}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{result: action.Result{Content: `{"posted":true,"ts":"123"}`}}
	e := engineFor(t, upstream.URL, "", store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"tell team I shipped"}]}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Agent should see the FINAL assistant message, never the tool call.
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Posted to #engineering." {
		t.Errorf("content = %v, want final text", msg["content"])
	}
	if _, hasToolCalls := msg["tool_calls"]; hasToolCalls {
		t.Error("agent saw tool_calls; should have been intercepted server-side")
	}

	// Executor was called exactly once with the LLM's args.
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].Name != "ship_update" {
		t.Errorf("called name = %q, want ship_update", exec.calls[0].Name)
	}
	if exec.calls[0].Args["channel"] != "#engineering" {
		t.Errorf("called args.channel = %v, want #engineering", exec.calls[0].Args["channel"])
	}

	// Continuation request must include the assistant tool-call message
	// AND the synthesized tool result.
	if len(captured.captured) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (initial + continuation)", len(captured.captured))
	}
	contMessages, _ := captured.captured[1]["messages"].([]any)
	if len(contMessages) < 3 {
		t.Fatalf("continuation messages = %d, want ≥3 (user + assistant + tool)", len(contMessages))
	}
	last := contMessages[len(contMessages)-1].(map[string]any)
	if last["role"] != "tool" {
		t.Errorf("last message role = %v, want tool", last["role"])
	}
	if !strings.Contains(fmt.Sprintf("%v", last["content"]), "posted") {
		t.Errorf("last message content = %v, expected executor's result", last["content"])
	}
}

// --- OpenAI: agent-declared tool passes through ---

func TestHandleOpenAI_AgentToolCallPassesThrough(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"agent_search","arguments":"{}"}}]}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{}
	e := engineFor(t, upstream.URL, "", store, exec)

	body := `{"model":"gpt-4","messages":[],"tools":[{"type":"function","function":{"name":"agent_search","description":"agent's"}}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// Agent sees the tool_call to handle locally.
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	choices, _ := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if _, hasToolCalls := msg["tool_calls"]; !hasToolCalls {
		t.Error("agent should see tool_calls for its own tool; got message without tool_calls")
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor called %d times for agent's tool; want 0", len(exec.calls))
	}
}

// --- OpenAI: multi-turn (action A → result → action B → result → final) ---

func TestHandleOpenAI_MultiTurnInterception(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		// Round 1: ship_update
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ship_update","arguments":"{\"channel\":\"#eng\"}"}}]}}]}`,
		// Round 2: file_bug
		`{"id":"r2","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c2","type":"function","function":{"name":"file_bug","arguments":"{\"title\":\"oops\"}"}}]}}]}`,
		// Round 3: final
		`{"id":"r3","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Done!"}}]}`,
	)
	store := loadStore(t, map[string]string{
		"ship-update.md": shipUpdateAction,
		"file-bug.md":    fileBugAction,
	})
	exec := &staticExecutor{result: action.Result{Content: `{"ok":true}`}}
	e := engineFor(t, upstream.URL, "", store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"go"}]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	choices, _ := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Done!" {
		t.Errorf("content = %v, want Done!", msg["content"])
	}
	if len(exec.calls) != 2 {
		t.Errorf("executor called %d times; want 2 (ship_update, file_bug)", len(exec.calls))
	}
}

// --- OpenAI: action error surfaces as tool-result, not gateway 5xx ---

func TestHandleOpenAI_ActionErrorBecomesToolResultError(t *testing.T) {
	// Round 1: tool call. Round 2: final after error result.
	upstream, captured := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ship_update","arguments":"{\"channel\":\"#x\"}"}}]}}]}`,
		`{"id":"r2","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Got an error from the action."}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{result: action.Result{Failure: failure.Upstream(429, "rate limited")}}
	e := engineFor(t, upstream.URL, "", store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"ship"}]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (action errors are tool-results, not gateway 5xxs)", w.Code)
	}
	contMessages, _ := captured.captured[1]["messages"].([]any)
	last := contMessages[len(contMessages)-1].(map[string]any)
	content, _ := last["content"].(string)
	if !strings.Contains(content, "error") || !strings.Contains(content, "rate limited") {
		t.Errorf("error content not surfaced to LLM: %v", content)
	}
}

// --- OpenAI: streaming response synthesized when agent asked for it ---

func TestHandleOpenAI_StreamingResponseEmittedAsSSE(t *testing.T) {
	upstream, captured := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hi!"}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, upstream.URL, "", store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"Hi!"`) {
		t.Errorf("SSE body missing assistant content; got %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("SSE body missing [DONE] terminator; got %s", body)
	}
	// Engine forces upstream stream=false during intercept rounds.
	streamFlag, _ := captured.captured[0]["stream"].(bool)
	if streamFlag {
		t.Error("upstream received stream=true; engine should force stream=false on intercept rounds")
	}
}

// --- OpenAI: upstream non-200 propagates ---

func TestHandleOpenAI_UpstreamErrorPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	t.Cleanup(srv.Close)

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, srv.URL, "", store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (upstream propagated)", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("bad key")) {
		t.Errorf("body lost upstream message: %s", w.Body.String())
	}
}

// --- OpenAI: mixed tool calls in one response → 502 ---

func TestHandleOpenAI_MixedToolCalls_Returns502(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"ship_update","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"agent_thing","arguments":"{}"}}]}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, upstream.URL, "", store, nil)

	body := `{"model":"gpt-4","messages":[],"tools":[{"type":"function","function":{"name":"agent_thing"}}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (mixed unsupported)", w.Code)
	}
}

// --- OpenAI: max rounds cap ---

func TestHandleOpenAI_MaxRoundsCap(t *testing.T) {
	// Always returns a tool_calls response, never finishes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"ship_update","arguments":"{}"}}]}}]}`))
	}))
	t.Cleanup(srv.Close)

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, srv.URL, "", store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (max rounds)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "max_intercept_rounds") {
		t.Errorf("body = %s, want max_intercept_rounds error code", w.Body.String())
	}
}

// --- Anthropic: passthrough when no tool_use ---

func TestHandleAnthropic_NoToolUse_PassesThrough(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"Hi!"}],"stop_reason":"end_turn"}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, "", upstream.URL, store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"Hi!"`) {
		t.Errorf("body lost upstream content: %s", w.Body.String())
	}
}

// --- Anthropic: tool_use interception ---

func TestHandleAnthropic_InterceptsAileronToolUse(t *testing.T) {
	upstream, captured := newScriptedUpstream(t,
		`{"id":"msg1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"tu_1","name":"ship_update","input":{"channel":"#eng"}}],"stop_reason":"tool_use"}`,
		`{"id":"msg2","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"Posted!"}],"stop_reason":"end_turn"}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{result: action.Result{Content: `{"posted":true}`}}
	e := engineFor(t, "", upstream.URL, store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1024,"messages":[{"role":"user","content":"ship"}]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"Posted!"`) {
		t.Errorf("agent didn't see final text: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"tool_use"`) {
		t.Error("agent saw tool_use; should have been intercepted")
	}
	if len(exec.calls) != 1 {
		t.Errorf("executor calls = %d, want 1", len(exec.calls))
	}
	// Continuation includes assistant + user(tool_result) messages.
	contMessages, _ := captured.captured[1]["messages"].([]any)
	if len(contMessages) < 3 {
		t.Fatalf("continuation messages = %d, want ≥3", len(contMessages))
	}
	user := contMessages[len(contMessages)-1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("expected synthesized user message; got role=%v", user["role"])
	}
	contentBlocks := user["content"].([]any)
	if contentBlocks[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("expected tool_result block; got %v", contentBlocks[0])
	}
}

// --- Anthropic: streaming SSE emission ---

func TestHandleAnthropic_StreamingEmittedAsSSE(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"msg_x","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"Hi!"}],"stop_reason":"end_turn"}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, "", upstream.URL, store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1024,"messages":[],"stream":true}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Errorf("SSE missing message_start: %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("SSE missing message_stop: %s", body)
	}
}

// --- OpenAI: invalid LLM JSON args become tool-result errors ---

func TestHandleOpenAI_InvalidArgsJSON_BecomesToolResultError(t *testing.T) {
	upstream, captured := newScriptedUpstream(t,
		// Round 1: tool call with malformed JSON arguments
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"ship_update","arguments":"{not-json"}}]}}]}`,
		`{"id":"r2","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Sorry."}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{}
	e := engineFor(t, upstream.URL, "", store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; bad-args should surface as tool-result error", w.Code)
	}
	// Executor should NOT have been called — invalid JSON skips it.
	if len(exec.calls) != 0 {
		t.Errorf("executor calls = %d, want 0 (bad args skipped)", len(exec.calls))
	}
	// The continuation request's last message should be a tool error.
	contMessages, _ := captured.captured[1]["messages"].([]any)
	last := contMessages[len(contMessages)-1].(map[string]any)
	content, _ := last["content"].(string)
	if !strings.Contains(content, "error") || !strings.Contains(content, "invalid JSON") {
		t.Errorf("error not surfaced; content=%v", content)
	}
}

// --- Helpers tests ---

func TestSetStream_Toggles(t *testing.T) {
	in := []byte(`{"model":"x","stream":true}`)
	out, err := setStream(in, false)
	if err != nil {
		t.Fatalf("setStream: %v", err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	if got["stream"] != false {
		t.Errorf("stream = %v, want false", got["stream"])
	}
}

func TestAgentRequestedStreaming_Detects(t *testing.T) {
	if !agentRequestedStreaming([]byte(`{"stream":true}`)) {
		t.Error("expected true for stream=true")
	}
	if agentRequestedStreaming([]byte(`{"stream":false}`)) {
		t.Error("expected false for stream=false")
	}
	if agentRequestedStreaming([]byte(`{}`)) {
		t.Error("expected false when stream is absent")
	}
	if agentRequestedStreaming([]byte(`not-json`)) {
		t.Error("expected false on malformed body")
	}
}

func TestSetStream_RejectsMalformedBody(t *testing.T) {
	if _, err := setStream([]byte(`not-json`), false); err == nil {
		t.Error("expected error on malformed body")
	}
}

func TestCopyForwardableHeaders_DropsHopByHop(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer x")
	src.Set("X-Custom", "keep")
	src.Set("Connection", "close")        // hop-by-hop
	src.Set("Keep-Alive", "timeout=5")    // hop-by-hop
	src.Set("Transfer-Encoding", "chunked") // hop-by-hop
	src.Set("Content-Length", "100")      // explicitly stripped
	src.Set("Host", "x.example")          // explicitly stripped

	dst := http.Header{}
	copyForwardableHeaders(src, dst)

	if dst.Get("Authorization") != "Bearer x" {
		t.Error("Authorization not forwarded")
	}
	if dst.Get("X-Custom") != "keep" {
		t.Error("X-Custom not forwarded")
	}
	for _, k := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length", "Host"} {
		if dst.Get(k) != "" {
			t.Errorf("%s should not be forwarded", k)
		}
	}
}

func TestPassThrough_FiltersHopByHop(t *testing.T) {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Connection", "close")
	headers.Set("X-Upstream-ID", "abc")

	w := httptest.NewRecorder()
	passThrough(w, http.StatusTeapot, headers, []byte(`{"x":1}`))

	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Error("Content-Type not propagated")
	}
	if w.Header().Get("X-Upstream-ID") != "abc" {
		t.Error("X-Upstream-ID not propagated")
	}
	if w.Header().Get("Connection") != "" {
		t.Error("Connection should not be propagated")
	}
}

// --- OpenAI: invalid JSON request body fails augmentation ---

func TestHandleOpenAI_InvalidJSONBody_Returns400(t *testing.T) {
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be reached when augmentation fails")
	}))
	t.Cleanup(srv.Close)
	e := engineFor(t, srv.URL, "", store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{not-json`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid_request") {
		t.Errorf("body = %s, want invalid_request error", w.Body.String())
	}
}

// --- OpenAI: upstream connection failure → 502 ---

func TestHandleOpenAI_UpstreamUnreachable_Returns502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL := srv.URL
	srv.Close()

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, upstreamURL, "", store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"class":"network_error"`) {
		t.Errorf("body = %s, want network_error class", w.Body.String())
	}
}

// --- OpenAI: executor returning a fatal error → 500 ---

type erroringExecutor struct {
	err error
}

func (e erroringExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	return action.Result{}, e.err
}

func TestHandleOpenAI_ExecutorFatalError_Returns500(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"ship_update","arguments":"{}"}}]}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := erroringExecutor{err: fmt.Errorf("connector unavailable")}
	e := engineFor(t, upstream.URL, "", store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"class":"connector_runtime_error"`) {
		t.Errorf("body = %s, want connector_runtime_error class", w.Body.String())
	}
}

// =====================================================================
// Anthropic-side parity tests
// =====================================================================

// --- Anthropic: gateway not configured ---

func TestHandleAnthropic_UpstreamNotConfigured_Returns503(t *testing.T) {
	store := loadStore(t, nil)
	e := engineFor(t, "", "", store, nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1,"messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- Anthropic: invalid JSON body ---

func TestHandleAnthropic_InvalidJSONBody_Returns400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be reached")
	}))
	t.Cleanup(srv.Close)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, "", srv.URL, store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{not-json`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- Anthropic: upstream unreachable ---

func TestHandleAnthropic_UpstreamUnreachable_Returns502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL := srv.URL
	srv.Close()

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, "", upstreamURL, store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1,"messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// --- Anthropic: upstream non-200 propagates ---

func TestHandleAnthropic_UpstreamErrorPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	t.Cleanup(srv.Close)

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, "", srv.URL, store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1,"messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (passed through)", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("bad key")) {
		t.Errorf("body lost upstream message: %s", w.Body.String())
	}
}

// --- Anthropic: executor fatal error ---

func TestHandleAnthropic_ExecutorFatalError_Returns500(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"tu","name":"ship_update","input":{}}],"stop_reason":"tool_use"}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := erroringExecutor{err: fmt.Errorf("nope")}
	e := engineFor(t, "", upstream.URL, store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1,"messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// --- Anthropic: mixed tool uses → 502 ---

func TestHandleAnthropic_MixedToolUses_Returns502(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"a","name":"ship_update","input":{}},{"type":"tool_use","id":"b","name":"agent_thing","input":{}}],"stop_reason":"tool_use"}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, "", upstream.URL, store, nil)

	body := `{"model":"c","max_tokens":1,"messages":[],"tools":[{"name":"agent_thing","input_schema":{"type":"object"}}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (mixed unsupported)", w.Code)
	}
}

// --- Anthropic: agent's tool_use passes through ---

func TestHandleAnthropic_AgentToolUsePassesThrough(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"a","name":"agent_thing","input":{}}],"stop_reason":"tool_use"}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{}
	e := engineFor(t, "", upstream.URL, store, exec)

	body := `{"model":"c","max_tokens":1,"messages":[],"tools":[{"name":"agent_thing","input_schema":{"type":"object"}}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"name":"agent_thing"`) {
		t.Errorf("agent should see its own tool_use; got %s", w.Body.String())
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor called %d times for agent's tool; want 0", len(exec.calls))
	}
}

// --- Anthropic: max rounds cap ---

func TestHandleAnthropic_MaxRoundsCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"t","name":"ship_update","input":{}}],"stop_reason":"tool_use"}`))
	}))
	t.Cleanup(srv.Close)

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, "", srv.URL, store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1,"messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (max rounds)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "max_intercept_rounds") {
		t.Errorf("body = %s, want max_intercept_rounds error code", w.Body.String())
	}
}

// --- Anthropic: multi-turn ---

func TestHandleAnthropic_MultiTurnInterception(t *testing.T) {
	upstream, _ := newScriptedUpstream(t,
		`{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"t1","name":"ship_update","input":{"channel":"#x"}}],"stop_reason":"tool_use"}`,
		`{"id":"m2","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"t2","name":"file_bug","input":{"title":"bug"}}],"stop_reason":"tool_use"}`,
		`{"id":"m3","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn"}`,
	)
	store := loadStore(t, map[string]string{
		"ship-update.md": shipUpdateAction,
		"file-bug.md":    fileBugAction,
	})
	exec := &staticExecutor{result: action.Result{Content: `{"ok":true}`}}
	e := engineFor(t, "", upstream.URL, store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1,"messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"Done."`) {
		t.Errorf("agent didn't see final text: %s", w.Body.String())
	}
	if len(exec.calls) != 2 {
		t.Errorf("executor called %d times; want 2", len(exec.calls))
	}
}

// --- Anthropic: action error → tool_result with is_error ---

// --- Upstream returns 200 but unparseable shape → passThrough ---

func TestHandleOpenAI_UpstreamShapeUnparseable_PassesThrough(t *testing.T) {
	upstream, _ := newScriptedUpstream(t, `{}`)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, upstream.URL, "", store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (passThrough)", w.Code)
	}
	if w.Body.String() != `{}` {
		t.Errorf("body = %q, want passthrough of upstream %q", w.Body.String(), `{}`)
	}
}

func TestHandleAnthropic_UpstreamShapeUnparseable_PassesThrough(t *testing.T) {
	upstream, _ := newScriptedUpstream(t, `{"not":"a message"}`)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e := engineFor(t, "", upstream.URL, store, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1,"messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (passThrough)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"a message"`) {
		t.Errorf("body lost upstream payload: %s", w.Body.String())
	}
}

// --- Streaming fallback when terminal response can't be reshaped ---

func TestEmitOpenAITerminal_StreamingFallbackOnUnparseableBody(t *testing.T) {
	w := httptest.NewRecorder()
	emitOpenAITerminal(w, []byte(`not-json`), true /*wantStream*/)
	// Falls back to JSON when chunk synthesis fails — at least the
	// raw upstream body reaches the agent.
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (fallback)", w.Header().Get("Content-Type"))
	}
	if w.Body.String() != "not-json" {
		t.Errorf("body = %q, want fallback raw passthrough", w.Body.String())
	}
}

func TestEmitAnthropicTerminal_StreamingFallbackOnUnparseableBody(t *testing.T) {
	w := httptest.NewRecorder()
	emitAnthropicTerminal(w, []byte(`not-json`), true /*wantStream*/)
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (fallback)", w.Header().Get("Content-Type"))
	}
}

func TestEmitAnthropicTerminal_StreamingFallbackOnMissingContent(t *testing.T) {
	// Valid JSON but no `content` field — anthropicEventsFromMessage
	// returns an error and the helper falls back to JSON.
	w := httptest.NewRecorder()
	emitAnthropicTerminal(w, []byte(`{"id":"x","type":"message"}`), true /*wantStream*/)
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json fallback", w.Header().Get("Content-Type"))
	}
}

func TestOpenAIChunksFromCompletion_ErrorOnNoChoices(t *testing.T) {
	if _, err := openAIChunksFromCompletion([]byte(`{"id":"x"}`)); err == nil {
		t.Error("expected error when response has no choices")
	}
}

func TestAnthropicEventsFromMessage_ErrorOnInvalidJSON(t *testing.T) {
	if _, err := anthropicEventsFromMessage([]byte(`not-json`)); err == nil {
		t.Error("expected error decoding malformed JSON")
	}
}

func TestParseAnthropicResponse_ErrorOnInvalidJSON(t *testing.T) {
	_, _, err := parseAnthropicResponse([]byte(`not-json`))
	if err == nil {
		t.Error("expected error decoding malformed JSON")
	}
}

func TestParseAnthropicResponse_ErrorOnWrongType(t *testing.T) {
	_, _, err := parseAnthropicResponse([]byte(`{"type":"error"}`))
	if err == nil {
		t.Error("expected error when type != message")
	}
}

func TestHandleAnthropic_ActionErrorBecomesIsError(t *testing.T) {
	upstream, captured := newScriptedUpstream(t,
		`{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"tool_use","id":"t","name":"ship_update","input":{}}],"stop_reason":"tool_use"}`,
		`{"id":"m2","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"Got error."}],"stop_reason":"end_turn"}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{result: action.Result{Failure: failure.Upstream(429, "rate limited")}}
	e := engineFor(t, "", upstream.URL, store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"c","max_tokens":1,"messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	contMessages, _ := captured.captured[1]["messages"].([]any)
	user := contMessages[len(contMessages)-1].(map[string]any)
	blocks := user["content"].([]any)
	first := blocks[0].(map[string]any)
	if first["is_error"] != true {
		t.Errorf("tool_result.is_error = %v, want true", first["is_error"])
	}
}

// =====================================================================
// Stage 4 (#365, ratified by ADR-0010): retry + audit integration.
//
// These tests exercise the engine end-to-end with retry policy and a
// recorder so we can assert that:
//   - Network failures and upstream 5xx/429 retry per ADR-0010.
//   - Final failures carry the retried count in details.
//   - Failure responses to the agent embed an audit_id that resolves
//     to a real recorded event.
//   - Action-side failures (Result.Failure) flow back as tool-results
//     with is_error / structured envelope content.
// =====================================================================

// engineWithRecorder builds an engine pointed at the given upstream
// using a small retry policy (so retries don't pay real-time backoff)
// and returns the engine + the audit store so tests can introspect
// recorded events.
func engineWithRecorder(t *testing.T, openAIURL, anthropicURL string, store *action.Store, exec action.Executor, policy retry.Policy) (*Engine, *audit.MemStore) {
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
	if exec == nil {
		exec = action.StubExecutor{}
	}
	auditStore := audit.NewMemStore()
	e, err := New(Config{
		OpenAIUpstream:    oa,
		AnthropicUpstream: an,
		Actions:           store,
		Executor:          exec,
		Recorder:          audit.NewRecorder(auditStore, nil, nil),
		RetryPolicy:       policy,
	})
	if err != nil {
		t.Fatalf("intercept.New: %v", err)
	}
	return e, auditStore
}

// flakyUpstream returns 503 the first n times, then a real OpenAI
// response. Counts requests for assertion.
type flakyUpstream struct {
	mu       sync.Mutex
	failsLeft int
	final    string
	calls    int
}

func newFlakyUpstream(t *testing.T, failsLeft int, final string) (*httptest.Server, *flakyUpstream) {
	t.Helper()
	f := &flakyUpstream{failsLeft: failsLeft, final: final}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		left := f.failsLeft
		if left > 0 {
			f.failsLeft--
		}
		f.mu.Unlock()
		if left > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"upstream busy"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(f.final))
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func TestHandleOpenAI_RetriesUpstream5xxThenSucceeds(t *testing.T) {
	upstream, flaky := newFlakyUpstream(t, 2,
		`{"id":"ok","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"final"}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e, _ := engineWithRecorder(t, upstream.URL, "", store, nil,
		retry.Policy{MaxRetries: 3, BaseDelay: time.Millisecond, Jitter: 0})

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if flaky.calls != 3 {
		t.Errorf("upstream calls = %d, want 3 (2 retries + success)", flaky.calls)
	}
	if !strings.Contains(w.Body.String(), `"final"`) {
		t.Errorf("body missing final content: %s", w.Body.String())
	}
}

func TestHandleOpenAI_RetriesUpstream5xxThenFails_AuditIDResolvable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"persistent"}`))
	}))
	t.Cleanup(srv.Close)

	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	e, auditStore := engineWithRecorder(t, srv.URL, "", store, nil,
		retry.Policy{MaxRetries: 2, BaseDelay: time.Millisecond, Jitter: 0})

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	inner, _ := resp["error"].(map[string]any)
	if inner["class"] != "external_api_error" {
		t.Errorf("class = %v, want external_api_error", inner["class"])
	}
	auditID, _ := inner["audit_id"].(string)
	if auditID == "" {
		t.Fatal("audit_id missing in envelope")
	}
	if _, ok := auditStore.GetByEventID(auditID); !ok {
		t.Errorf("audit_id %q does not resolve to a recorded event", auditID)
	}
	details, _ := inner["details"].(map[string]any)
	if details["retried"] == nil {
		t.Errorf("details.retried missing on exhausted retry; got details=%v", details)
	}
}

func TestHandleOpenAI_ActionFailure_FlowsAsToolResult_AuditRecorded(t *testing.T) {
	upstream, captured := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"ship_update","arguments":"{}"}}]}}]}`,
		`{"id":"r2","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"sorry"}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{result: action.Result{Failure: failure.CapabilityDeniedAt(failure.Action, "channel scope missing")}}
	e, auditStore := engineWithRecorder(t, upstream.URL, "", store, exec, retry.DefaultPolicy())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (action errors are tool-results)", w.Code)
	}
	// Continuation request's tool message carries the failure shape.
	contMessages, _ := captured.captured[1]["messages"].([]any)
	last := contMessages[len(contMessages)-1].(map[string]any)
	content, _ := last["content"].(string)
	if !strings.Contains(content, "capability_denied") {
		t.Errorf("tool content missing class: %s", content)
	}
	// Audit log records the action-side failure too.
	traces, _ := auditStore.ListTraces(context.Background(), audit.Filter{})
	hasFailure := false
	for _, tr := range traces {
		for _, ev := range tr.Events {
			if class, _ := ev.Payload["aileron.failure.class"].(string); class == "capability_denied" {
				hasFailure = true
			}
		}
	}
	if !hasFailure {
		t.Error("audit log did not record the action-side capability_denied failure")
	}
}
