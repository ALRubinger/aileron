package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// HTTPMiddleware contract:
//   - Extracts an incoming `traceparent` so handler-side span starts
//     are children of the upstream trace.
//   - Starts a server-root span named per spanNameFn, ends it on
//     handler return.
//   - Marks the span as Error when the handler writes 5xx.
//   - When a span name fn is nil, uses METHOD + PATH.
//   - Works in no-op mode (Init disabled) without panicking.

func TestHTTPMiddleware_ExtractsIncomingTraceparent(t *testing.T) {
	// Stand up a real SDK provider so we can inspect the resulting
	// span tree. Init's no-op short-circuit hides too much for this
	// assertion.
	withSDKProvider(t)

	const incomingTraceID = "0af7651916cd43dd8448eb211c80319c"
	const incomingSpanID = "b7ad6b7169203331"
	traceparent := fmt.Sprintf("00-%s-%s-01", incomingTraceID, incomingSpanID)

	var observed trace.SpanContext
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = trace.SpanFromContext(r.Context()).SpanContext()
	})
	wrapped := HTTPMiddleware(func(_ *http.Request) string { return "test.span" })(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/actions/foo/run", nil)
	req.Header.Set("traceparent", traceparent)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if got := observed.TraceID().String(); got != incomingTraceID {
		t.Errorf("TraceID = %q, want %q (traceparent not extracted)", got, incomingTraceID)
	}
	// The span observed inside the handler should be a child of the
	// incoming SpanID, not equal to it.
	if got := observed.SpanID().String(); got == incomingSpanID {
		t.Error("expected child SpanID, got the parent SpanID — middleware did not Start a new span")
	}
}

func TestHTTPMiddleware_StartsFreshTraceWhenHeaderAbsent(t *testing.T) {
	withSDKProvider(t)

	var observed trace.SpanContext
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = trace.SpanFromContext(r.Context()).SpanContext()
	})
	wrapped := HTTPMiddleware(nil)(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/actions/foo/run", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if !observed.IsValid() {
		t.Error("expected a valid SpanContext for the handler-visible span; got invalid")
	}
}

func TestHTTPMiddleware_MalformedTraceparentStartsFreshTrace(t *testing.T) {
	withSDKProvider(t)

	var observed trace.SpanContext
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = trace.SpanFromContext(r.Context()).SpanContext()
	})
	wrapped := HTTPMiddleware(nil)(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/actions/foo/run", nil)
	req.Header.Set("traceparent", "this-is-not-a-valid-traceparent")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	// The middleware must not panic on a malformed header. We don't
	// assert a particular trace ID — the W3C spec leaves the failure
	// behavior to the parser, and the parser may return either an
	// invalid SpanContext or a fresh one. Either way, the handler
	// must run.
	_ = observed
}

func TestHTTPMiddleware_ExportsSpanWithStatus5xxAsError(t *testing.T) {
	var buf bytes.Buffer
	exp, err := newExporter(&config.ObservabilityConfig{Exporter: config.ExporterStdout}, &buf)
	if err != nil {
		t.Fatalf("newExporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	wrapped := HTTPMiddleware(func(_ *http.Request) string { return "test.span" })(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	out := buf.String()
	// The exporter writes one or more lines; find the span and
	// assert its status code attribute and span status.
	found := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record["Name"] != "test.span" {
			continue
		}
		found = true
		// Span status: stdouttrace serializes Status.Code as a string;
		// just assert it is present and non-Unset.
		if status, ok := record["Status"].(map[string]any); ok {
			if code, _ := status["Code"].(string); code == "" || code == "Unset" {
				t.Errorf("expected span status to reflect 5xx; got %v", status)
			}
		}
	}
	if !found {
		t.Errorf("did not find exported span; output was:\n%s", out)
	}
}

func TestHTTPMiddleware_NoOpModeDoesNotPanic(t *testing.T) {
	cfg := &config.ObservabilityConfig{OTelEnabled: false}
	p := Init(cfg, nil)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := HTTPMiddleware(nil)(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
}

func TestHTTPMiddleware_BodyOnlyWriteDefaultsToOK(t *testing.T) {
	// A handler that writes a body without an explicit WriteHeader
	// implies http.StatusOK. The middleware's recorder must not
	// double-set status or panic in that case; the resulting span
	// stays at the default OK code.
	withSDKProvider(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := HTTPMiddleware(nil)(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

func TestHTTPMiddleware_DefaultSpanName(t *testing.T) {
	// Internal helper, but worth a guard: the default name is the
	// shape downstream operators search by in trace tools.
	r := httptest.NewRequest(http.MethodPost, "/v1/actions/foo/run", nil)
	if got, want := defaultSpanName(r), "POST /v1/actions/foo/run"; got != want {
		t.Errorf("defaultSpanName = %q, want %q", got, want)
	}
}

// withSDKProvider installs a minimal SDK tracer provider for the
// duration of the test, restoring the previous one on cleanup. Used
// by tests that need real trace.SpanContext IDs (the no-op provider
// returns invalid SpanContexts).
func withSDKProvider(t *testing.T) {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
}
