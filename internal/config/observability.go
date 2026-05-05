package config

import (
	"fmt"
	"strconv"
)

// ObservabilityConfig holds the knobs that control OpenTelemetry
// emission. Issue #390 Phase 7 ships this as foundation: the SDK
// bootstrap and traceparent extraction land here so external callers
// can correlate end-to-end. Span emission at action / connector /
// capability / approval boundaries lands in follow-up PRs once the
// audit-log file rotation work converges and we can share the
// rotating writer between audit and traces.
type ObservabilityConfig struct {
	// OTelEnabled is the master switch. When false, the bootstrap
	// installs a no-op tracer provider and tracing is effectively
	// off — there is no overhead beyond the no-op call sites.
	// Env: AILERON_OTEL_ENABLED (default: false)
	OTelEnabled bool

	// ServiceName is the OTel resource attribute `service.name`
	// reported on every span. Distinguishes Aileron from the agent
	// and the upstream LLM in trace tooling.
	// Env: AILERON_OTEL_SERVICE_NAME (default: "aileron")
	ServiceName string

	// Exporter selects how spans leave the process. "noop" drops
	// every span (the default; matches OTelEnabled=false). "stdout"
	// writes JSON-per-line to stderr for local development. The
	// file/OTLP exporters land in follow-up PRs.
	// Env: AILERON_OTEL_EXPORTER (default: "noop")
	Exporter string
}

// Exporter values recognised by LoadObservabilityConfig.
const (
	ExporterNoop   = "noop"
	ExporterStdout = "stdout"
)

const (
	defaultOTelServiceName = "aileron"
	defaultOTelExporter    = ExporterNoop
)

// LoadObservabilityConfig reads tracing configuration from
// environment variables. Returns an error only when an explicit
// value fails to parse; absent env vars fall back to disabled
// tracing with a no-op exporter.
func LoadObservabilityConfig() (*ObservabilityConfig, error) {
	enabledRaw := envTrimmed("AILERON_OTEL_ENABLED")
	enabled := false
	if enabledRaw != "" {
		v, err := strconv.ParseBool(enabledRaw)
		if err != nil {
			return nil, fmt.Errorf("AILERON_OTEL_ENABLED: %w", err)
		}
		enabled = v
	}

	exporter := envOrDefault("AILERON_OTEL_EXPORTER", defaultOTelExporter)
	switch exporter {
	case ExporterNoop, ExporterStdout:
	default:
		return nil, fmt.Errorf("AILERON_OTEL_EXPORTER: unknown exporter %q (want %q or %q)",
			exporter, ExporterNoop, ExporterStdout)
	}

	return &ObservabilityConfig{
		OTelEnabled: enabled,
		ServiceName: envOrDefault("AILERON_OTEL_SERVICE_NAME", defaultOTelServiceName),
		Exporter:    exporter,
	}, nil
}
