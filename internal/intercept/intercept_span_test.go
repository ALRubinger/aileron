package intercept

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Span emission contract for the intercept loop:
//
//   - One aileron.intercept.round span per iteration of the loop.
//     SpanKind=Internal. Round index attribute (0-based) tracks order.
//   - Protocol attribute distinguishes openai vs anthropic.
//   - Terminal round carries aileron.intercept.terminal=true.
//   - Tool-bearing rounds carry aileron.intercept.tool_calls_count.
//   - Failure paths set span status Error with a message.

func withInMemoryExporter(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exp
}

func roundSpans(spans []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "aileron.intercept.round" {
			out = append(out, s)
		}
	}
	return out
}

func attrInt(s sdktrace.ReadOnlySpan, key string) (int64, bool) {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsInt64(), true
		}
	}
	return 0, false
}

func attrString(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsString(), true
		}
	}
	return "", false
}

func attrBool(s sdktrace.ReadOnlySpan, key string) (bool, bool) {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsBool(), true
		}
	}
	return false, false
}

func TestHandleOpenAI_EmitsInterceptRoundSpans(t *testing.T) {
	// Two-round flow: round 0 returns a tool call, round 1 returns
	// the terminal text. We expect two intercept.round spans, the
	// first carrying tool_calls_count=1, the second terminal=true.
	exp := withInMemoryExporter(t)

	upstream, _ := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ship_update","arguments":"{\"channel\":\"#engineering\"}"}}]}}]}`,
		`{"id":"r2","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Posted."}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{result: action.Result{Content: `{"posted":true}`}}
	e := engineFor(t, upstream.URL, "", store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"tell team I shipped"}]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	spans := roundSpans(exp.GetSpans().Snapshots())
	if len(spans) != 2 {
		t.Fatalf("got %d round spans, want 2", len(spans))
	}

	// Find by round_index — span end order isn't strictly guaranteed
	// but in this synchronous flow it should be. Assert by attribute
	// to be robust regardless.
	var round0, round1 sdktrace.ReadOnlySpan
	for _, s := range spans {
		idx, _ := attrInt(s, "aileron.intercept.round_index")
		switch idx {
		case 0:
			round0 = s
		case 1:
			round1 = s
		}
	}
	if round0 == nil || round1 == nil {
		t.Fatalf("expected spans for round_index 0 and 1; got: %+v", spans)
	}

	// Both spans carry the openai protocol attribute.
	for i, s := range []sdktrace.ReadOnlySpan{round0, round1} {
		if v, _ := attrString(s, "aileron.intercept.protocol"); v != "openai" {
			t.Errorf("round %d protocol = %q, want openai", i, v)
		}
	}

	// Round 0 had a tool call; round 1 was terminal.
	if v, ok := attrInt(round0, "aileron.intercept.tool_calls_count"); !ok || v != 1 {
		t.Errorf("round 0 tool_calls_count = %v (ok=%v), want 1", v, ok)
	}
	if v, ok := attrBool(round1, "aileron.intercept.terminal"); !ok || !v {
		t.Errorf("round 1 terminal = %v (ok=%v), want true", v, ok)
	}
}

func TestHandleAnthropic_EmitsInterceptRoundSpansWithProtocolLabel(t *testing.T) {
	// Single terminal round on the Anthropic path: assert one
	// intercept.round span with protocol=anthropic.
	exp := withInMemoryExporter(t)

	upstream, _ := newScriptedUpstream(t,
		`{"id":"m1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{}
	e := engineFor(t, "", upstream.URL, store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`))
	w := httptest.NewRecorder()
	e.HandleAnthropic(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	spans := roundSpans(exp.GetSpans().Snapshots())
	if len(spans) != 1 {
		t.Fatalf("got %d round spans, want 1", len(spans))
	}
	if v, _ := attrString(spans[0], "aileron.intercept.protocol"); v != "anthropic" {
		t.Errorf("protocol = %q, want anthropic", v)
	}
	if v, ok := attrBool(spans[0], "aileron.intercept.terminal"); !ok || !v {
		t.Errorf("terminal = %v (ok=%v), want true", v, ok)
	}
}

func TestHandleOpenAI_NoOpProviderDoesNotPanic(t *testing.T) {
	// Default OTel global is no-op until daemon Init runs. The
	// intercept loop must still work — span ops are no-ops.
	upstream, _ := newScriptedUpstream(t,
		`{"id":"r1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`,
	)
	store := loadStore(t, map[string]string{"ship-update.md": shipUpdateAction})
	exec := &staticExecutor{}
	e := engineFor(t, upstream.URL, "", store, exec)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	e.HandleOpenAI(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with no-op tracer; body=%s", w.Code, w.Body.String())
	}
}
