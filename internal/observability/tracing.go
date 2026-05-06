// Package observability bootstraps Aileron's OpenTelemetry tracer
// provider and propagator. Issue #390 Phase 7 ships this as
// foundation: external callers can include `traceparent` on requests
// to Aileron and have it propagated through; span emission at action,
// connector, capability, and approval boundaries lands in follow-up
// PRs once the audit-log file rotation work converges and the file
// exporter can share the rotating writer.
//
// The shape of [Init] is intentionally narrow: it returns a
// [Provider] whose [Provider.Shutdown] flushes pending spans. Code
// inside the request path uses [otel.Tracer] to acquire a tracer and
// [propagation.TraceContext] for W3C propagation — both come from
// the global registrations [Init] performs.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ALRubinger/aileron/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// resourceAttributeServiceName is the well-known OTel resource
// attribute key for service.name. We don't take a dependency on the
// versioned semconv package just for this single constant — the
// schema URL there changes per release and forces an upgrade
// treadmill that yields no behavioral benefit at this layer.
const resourceAttributeServiceName = "service.name"

// Provider wraps a [trace.TracerProvider] with a [Provider.Shutdown]
// hook the daemon calls at process end. Embedding rather than
// aliasing lets callers pass it anywhere a TracerProvider is
// expected.
type Provider struct {
	trace.TracerProvider

	shutdown func(context.Context) error
}

// Shutdown flushes any pending spans and releases exporter
// resources. Safe to call when the provider was created in no-op
// mode — the call short-circuits and returns nil.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Init constructs the tracer provider, installs it as the OTel
// global, and registers the W3C TraceContext propagator. Callers
// should defer Shutdown on the returned Provider to flush at process
// end.
//
// When cfg.OTelEnabled is false (the default) Init installs a
// no-op provider — there is no SDK overhead and exporters are
// never constructed. The propagator is installed unconditionally so
// `traceparent` on inbound requests is parsed regardless; this lets
// the gateway pass a coherent context downstream even before
// Aileron itself starts emitting spans, and it costs nothing when
// no spans are recorded.
//
// log may be nil; warnings about exporter construction failures
// are routed to slog.Default in that case. An exporter failure
// degrades gracefully to no-op rather than failing daemon startup —
// the Aileron HTTP server should keep serving requests when its
// telemetry sidecar is misconfigured.
func Init(cfg *config.ObservabilityConfig, log *slog.Logger) *Provider {
	if log == nil {
		log = slog.Default()
	}

	// Propagator first: cheap, useful even when emission is off.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	if cfg == nil || !cfg.OTelEnabled {
		p := noop.NewTracerProvider()
		otel.SetTracerProvider(p)
		return &Provider{TracerProvider: p}
	}

	exporter, err := newExporter(cfg, os.Stderr)
	if err != nil {
		log.Warn("observability: exporter construction failed; tracing disabled",
			"exporter", cfg.Exporter, "error", err.Error())
		p := noop.NewTracerProvider()
		otel.SetTracerProvider(p)
		return &Provider{TracerProvider: p}
	}
	if exporter == nil {
		// noop selection: no exporter, no SDK overhead.
		p := noop.NewTracerProvider()
		otel.SetTracerProvider(p)
		return &Provider{TracerProvider: p}
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			attribute.String(resourceAttributeServiceName, cfg.ServiceName),
		),
	)
	if err != nil {
		log.Warn("observability: resource construction failed; tracing disabled",
			"error", err.Error())
		p := noop.NewTracerProvider()
		otel.SetTracerProvider(p)
		return &Provider{TracerProvider: p}
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return &Provider{
		TracerProvider: tp,
		shutdown:       tp.Shutdown,
	}
}

// newExporter builds the SpanExporter selected by cfg.Exporter.
// Returns (nil, nil) for the no-op case so the caller can short-
// circuit to a no-op provider without constructing the SDK.
//
// stdoutW is the writer the stdout exporter targets — taken as a
// parameter so tests can capture span output without race
// conditions on os.Stderr. The file exporter uses cfg.TracesDir
// directly; the writer parameter does not apply.
func newExporter(cfg *config.ObservabilityConfig, stdoutW io.Writer) (sdktrace.SpanExporter, error) {
	switch cfg.Exporter {
	case config.ExporterNoop, "":
		return nil, nil
	case config.ExporterStdout:
		// PrettyPrint is intentionally off: jq-friendly JSON-per-line
		// is what humans want when piping through standard tooling.
		return stdouttrace.New(stdouttrace.WithWriter(stdoutW))
	case config.ExporterFile:
		return newFileExporter(cfg.TracesDir)
	case config.ExporterOTLP:
		// otlptracehttp.New honors the standard OTel env vars by
		// default — OTEL_EXPORTER_OTLP_ENDPOINT,
		// OTEL_EXPORTER_OTLP_HEADERS, OTEL_EXPORTER_OTLP_INSECURE,
		// etc. We deliberately don't add Aileron-prefixed
		// alternatives: anyone running an OTel collector already
		// has these vars in their environment, and forking the
		// names would force them to maintain two parallel sets.
		// HTTP/protobuf rather than gRPC keeps the dep tree light
		// and works through standard proxies.
		return otlptracehttp.New(context.Background())
	default:
		return nil, fmt.Errorf("unknown exporter %q", cfg.Exporter)
	}
}
