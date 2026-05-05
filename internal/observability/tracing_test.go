package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Init contract:
//   - Disabled or nil config → no-op provider, but the W3C
//     TraceContext propagator is still installed (so inbound
//     traceparent is parsed regardless of emission).
//   - Enabled + ExporterNoop → still no-op (SDK never constructed).
//   - Enabled + ExporterStdout → SDK provider whose spans reach the
//     supplied writer in JSON-per-line form.
//   - Shutdown is always safe to call.

func TestInit_DisabledReturnsNoOpProvider(t *testing.T) {
	cfg := &config.ObservabilityConfig{
		OTelEnabled: false,
		ServiceName: "aileron-test",
		Exporter:    config.ExporterStdout, // exporter set but disabled wins
	}

	p := Init(cfg, nil)
	defer func() {
		if err := p.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Error("expected no-op TracerProvider, got SDK provider")
	}

	// The propagator must be installed even when emission is off.
	if _, ok := otel.GetTextMapPropagator().(propagation.TraceContext); !ok {
		t.Errorf("propagator = %T, want propagation.TraceContext", otel.GetTextMapPropagator())
	}
}

func TestInit_NilConfigReturnsNoOpProvider(t *testing.T) {
	p := Init(nil, nil)
	defer p.Shutdown(context.Background())

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Error("expected no-op TracerProvider, got SDK provider")
	}
}

func TestInit_EnabledWithNoopExporterIsNoOp(t *testing.T) {
	cfg := &config.ObservabilityConfig{
		OTelEnabled: true,
		ServiceName: "aileron-test",
		Exporter:    config.ExporterNoop,
	}

	p := Init(cfg, nil)
	defer p.Shutdown(context.Background())

	// The SDK provider should not be installed when the exporter is noop:
	// recording spans with no exporter wastes work.
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Error("expected no-op TracerProvider when exporter=noop, got SDK provider")
	}
}

func TestInit_StdoutExporterEmitsSpan(t *testing.T) {
	cfg := &config.ObservabilityConfig{
		OTelEnabled: true,
		ServiceName: "aileron-test",
		Exporter:    config.ExporterStdout,
	}

	// Reach into newExporter directly so we can capture output without
	// races on os.Stderr. The Init code path is exercised separately;
	// this test asserts the exporter contract — given a span, write a
	// JSON line — which is the part that matters for downstream tooling.
	var buf bytes.Buffer
	exp, err := newExporter(cfg.Exporter, &buf)
	if err != nil {
		t.Fatalf("newExporter: %v", err)
	}
	if exp == nil {
		t.Fatal("newExporter returned nil for stdout exporter")
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer tp.Shutdown(context.Background())

	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "test-span")
	span.End()

	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "test-span") {
		t.Errorf("expected span name in output; got %q", out)
	}
	// The exporter writes valid JSON; sanity-check by decoding the
	// first non-empty line.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var any map[string]any
		if err := json.Unmarshal([]byte(line), &any); err != nil {
			t.Errorf("exporter wrote non-JSON line: %q (%v)", line, err)
		}
		break
	}
}

func TestInit_UnknownExporterDegradesToNoOp(t *testing.T) {
	// Bypass LoadObservabilityConfig — that function rejects unknown
	// exporter values. Init itself must still degrade to no-op when
	// somehow handed a bad config (defense-in-depth, since callers
	// can construct ObservabilityConfig literals).
	cfg := &config.ObservabilityConfig{
		OTelEnabled: true,
		ServiceName: "aileron-test",
		Exporter:    "datadog",
	}

	p := Init(cfg, nil)
	defer p.Shutdown(context.Background())

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Error("expected no-op TracerProvider for unknown exporter, got SDK provider")
	}
}

func TestProvider_ShutdownNilSafe(t *testing.T) {
	var p *Provider
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on nil Provider returned error: %v", err)
	}
}

func TestInit_EnabledWithStdoutInstallsSDKAndShutdownFlushes(t *testing.T) {
	// Snapshot global state — Init mutates the OTel globals.
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	cfg := &config.ObservabilityConfig{
		OTelEnabled: true,
		ServiceName: "aileron-test",
		Exporter:    config.ExporterStdout,
	}

	p := Init(cfg, nil)
	if p == nil {
		t.Fatal("Init returned nil Provider")
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Errorf("expected SDK TracerProvider, got %T", otel.GetTracerProvider())
	}

	// Emit a span so Shutdown has something to flush.
	tracer := otel.GetTracerProvider().Tracer("test")
	_, span := tracer.Start(context.Background(), "shutdown-flush")
	span.End()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown returned error: %v", err)
	}
}
