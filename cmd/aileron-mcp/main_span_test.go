package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Span emission contract for runAction:
//
//   - Wraps the outbound POST in an aileron.mcp.tool.call span
//     carrying aileron.action.name. SpanKind=Client.
//   - Injects W3C TraceContext on the outbound request so the daemon's
//     middleware (#457) can extract it and parent its action.execute /
//     connector.call spans to this one.
//   - Error path marks the span Error with the daemon's error text as
//     the status description.
//   - With the default no-op tracer provider (AILERON_OTEL_ENABLED
//     unset), runAction still works — span operations are no-ops.

// withSDKProvider installs a sync-flushed SDK tracer provider for the
// test, restoring the previous global on cleanup. The W3C
// TraceContext propagator is also installed so traceparent injection
// actually emits a header.
func withSDKProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return exp
}

func TestRunAction_EmitsToolCallSpanWithActionName(t *testing.T) {
	exp := withSDKProvider(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content := "ok"
		_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "audit_x", Result: &content})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.runAction(context.Background(), "ship-update", nil)
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}

	spans := exp.GetSpans().Snapshots()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name() != "aileron.mcp.tool.call" {
		t.Errorf("span name = %q, want aileron.mcp.tool.call", spans[0].Name())
	}
	var actionName string
	for _, a := range spans[0].Attributes() {
		if string(a.Key) == "aileron.action.name" {
			actionName = a.Value.AsString()
		}
	}
	if actionName != "ship-update" {
		t.Errorf("aileron.action.name = %q, want ship-update", actionName)
	}
}

func TestRunAction_InjectsTraceparentOnOutboundRequest(t *testing.T) {
	withSDKProvider(t)

	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("traceparent")
		content := "ok"
		_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "audit_x", Result: &content})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	if got := s.runAction(context.Background(), "ship-update", nil); got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}

	if captured == "" {
		t.Fatal("traceparent header was not injected on outbound request")
	}
	// W3C TraceContext format: version-traceid-spanid-flags
	// e.g. 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01
	parts := strings.Split(captured, "-")
	if len(parts) != 4 {
		t.Errorf("traceparent malformed: %q (want 4 hyphen-separated parts)", captured)
	}
	if parts[0] != "00" {
		t.Errorf("traceparent version = %q, want 00", parts[0])
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 {
		t.Errorf("traceparent ID lengths off: traceid=%d (want 32), spanid=%d (want 16)",
			len(parts[1]), len(parts[2]))
	}
}

func TestRunAction_ErrorPathMarksSpanError(t *testing.T) {
	exp := withSDKProvider(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error":{"class":"binding_required","message":"vault is locked"}}`))
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.runAction(context.Background(), "ship-update", nil)
	if !got.IsError {
		t.Fatal("expected error for 412")
	}

	spans := exp.GetSpans().Snapshots()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status().Code != 1 /* codes.Error */ {
		t.Errorf("span status = %v, want Error", spans[0].Status())
	}
	if !strings.Contains(spans[0].Status().Description, "binding_required") {
		t.Errorf("span status description = %q, want containing 'binding_required'",
			spans[0].Status().Description)
	}
}

func TestRunAction_PropagatesParentTraceWhenContextHasOne(t *testing.T) {
	// When the caller already started a span (hypothetical future:
	// an MCP-level traceparent or a wrapping operation in this
	// process), runAction's span must be a child of it, and the
	// outbound traceparent must reflect that trace.
	exp := withSDKProvider(t)

	tracer := otel.GetTracerProvider().Tracer("test")
	parentCtx, parentSpan := tracer.Start(context.Background(), "parent")
	defer parentSpan.End()
	parentTraceID := parentSpan.SpanContext().TraceID().String()

	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("traceparent")
		content := "ok"
		_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "audit_x", Result: &content})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	if got := s.runAction(parentCtx, "ship-update", nil); got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}

	if !strings.Contains(captured, parentTraceID) {
		t.Errorf("outbound traceparent = %q, want it to contain parent trace id %q",
			captured, parentTraceID)
	}

	// The mcp.tool.call span itself should also be a child of the
	// parent — same trace id, parent span id pointing at the parent.
	spans := exp.GetSpans().Snapshots()
	for _, s := range spans {
		if s.Name() == "aileron.mcp.tool.call" {
			if s.SpanContext().TraceID().String() != parentTraceID {
				t.Errorf("mcp.tool.call trace id = %q, want %q",
					s.SpanContext().TraceID().String(), parentTraceID)
			}
			return
		}
	}
	t.Fatal("aileron.mcp.tool.call span not found")
}

func TestErrorText_FallsBackWhenNoTextContent(t *testing.T) {
	// errorText is the helper that pulls a human-readable message
	// out of a tool-error result for the span status description.
	// Robust against malformed results: empty content slice or
	// content with non-text or empty-text entries.
	cases := []struct {
		name string
		in   toolResult
		want string
	}{
		{"empty content", toolResult{}, "tool error"},
		{"non-text content", toolResult{Content: []toolContent{{Type: "image", Text: "ignored"}}}, "tool error"},
		{"empty text", toolResult{Content: []toolContent{{Type: "text", Text: ""}}}, "tool error"},
		{"first non-empty text wins", toolResult{Content: []toolContent{
			{Type: "text", Text: ""},
			{Type: "text", Text: "real error"},
		}}, "real error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorText(tc.in); got != tc.want {
				t.Errorf("errorText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunAction_NoOpTracerDoesNotPanic(t *testing.T) {
	// Default OTel global is no-op until Init runs. runAction must
	// still work — span ops are no-ops, propagator inject writes
	// nothing to the header (the no-op propagator is the default
	// before Init runs in main).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content := "ok"
		_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "audit_x", Result: &content})
	}))
	defer srv.Close()

	// Reset to fresh defaults: no-op tracer + no-op propagator.
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(otel.GetTracerProvider()) // already noop by default
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	if got := s.runAction(context.Background(), "ship-update", nil); got.IsError {
		t.Fatalf("unexpected error with no-op tracer: %s", got.Content[0].Text)
	}
}
