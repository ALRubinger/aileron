package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/config"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// OTLP exporter contract:
//   - newExporter dispatch routes Exporter=otlp through to
//     otlptracehttp.New, which honors the standard OTel env vars.
//   - When OTEL_EXPORTER_OTLP_ENDPOINT points at an HTTP server,
//     ExportSpans POSTs an OTLP/HTTP request to /v1/traces with a
//     protobuf body.
//   - The exporter is a real sdktrace.SpanExporter — Shutdown is
//     wired and ExportSpans returns errors from the underlying client.

func TestNewExporter_DispatchesOTLPByConfig(t *testing.T) {
	// Sanity: cfg.Exporter=otlp returns a non-nil exporter via the
	// dispatch in tracing.go. We don't drive a real round-trip here —
	// that's the integration test below — but we verify the dispatch
	// itself doesn't fail at construction.
	cfg := &config.ObservabilityConfig{Exporter: config.ExporterOTLP}
	// otlptracehttp.New reads OTEL_EXPORTER_OTLP_ENDPOINT but does
	// not validate it at construction (the client connects lazily on
	// first ExportSpans). Constructor success is enough for this
	// dispatch test.
	exp, err := newExporter(cfg, nil /* stdoutW unused */)
	if err != nil {
		t.Fatalf("newExporter(otlp): %v", err)
	}
	if exp == nil {
		t.Fatal("expected non-nil OTLP exporter")
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })
}

func TestOTLPExporter_PostsToCollectorEndpoint(t *testing.T) {
	// Stand up a minimal HTTP server impersonating an OTLP/HTTP
	// collector. Drive a span through the SDK provider configured
	// with the OTLP exporter pointed at this server. Assert the
	// collector saw a POST to /v1/traces with a non-empty body.
	type capture struct {
		method      string
		path        string
		contentType string
		bodyLen     int
	}
	got := make(chan capture, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		select {
		case got <- capture{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			bodyLen:     len(body),
		}:
		default:
		}
		// Empty 200 is the OTLP success response.
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	// Point the OTLP exporter at our collector via the standard
	// OTel env var. The otlptracehttp client appends /v1/traces to
	// the endpoint automatically, so we pass just the host:port.
	endpointURL, err := url.Parse(collector.URL)
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true") // httptest is plain HTTP

	cfg := &config.ObservabilityConfig{Exporter: config.ExporterOTLP}
	exp, err := newExporter(cfg, nil)
	if err != nil {
		t.Fatalf("newExporter(otlp): %v", err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })

	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "otlp-test-span")
	span.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	select {
	case c := <-got:
		if c.method != http.MethodPost {
			t.Errorf("method = %q, want POST", c.method)
		}
		if !strings.HasSuffix(c.path, "/v1/traces") {
			t.Errorf("path = %q, want suffix /v1/traces", c.path)
		}
		if !strings.Contains(c.contentType, "application/x-protobuf") {
			t.Errorf("Content-Type = %q, want application/x-protobuf", c.contentType)
		}
		if c.bodyLen == 0 {
			t.Errorf("collector received empty body; expected an OTLP-encoded span")
		}
	default:
		t.Fatalf("collector received no request; endpoint was %s", endpointURL)
	}
}
