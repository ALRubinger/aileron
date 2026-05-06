package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ObservabilityConfig holds the knobs that control OpenTelemetry
// emission. Issue #390 Phase 7 ships this as foundation: the SDK
// bootstrap and traceparent extraction land here so external callers
// can correlate end-to-end. Span emission integrations and the file
// exporter ride on top.
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
	// writes JSON-per-line to stderr for local development. "file"
	// writes JSON-per-line to a daily-rotated file under TracesDir
	// (path: <TracesDir>/traces/spans-YYYY-MM-DD.jsonl).
	// Env: AILERON_OTEL_EXPORTER (default: "noop")
	Exporter string

	// TracesDir is the state directory under which the file exporter
	// writes its daily-rotated spans file. The file itself lives at
	// `<TracesDir>/traces/spans-YYYY-MM-DD.jsonl`, sibling to the
	// audit log's `<TracesDir>/audit/audit-YYYY-MM-DD.jsonl` when
	// the two share a state dir. Only consulted when Exporter ==
	// "file"; the field is populated regardless so wiring code can
	// inspect the resolved path without re-reading env vars.
	// Env: AILERON_TRACES_DIR (default: <home>/.aileron)
	TracesDir string
}

// Exporter values recognised by LoadObservabilityConfig.
const (
	ExporterNoop   = "noop"
	ExporterStdout = "stdout"
	ExporterFile   = "file"
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
	case ExporterNoop, ExporterStdout, ExporterFile:
	default:
		return nil, fmt.Errorf("AILERON_OTEL_EXPORTER: unknown exporter %q (want %q, %q, or %q)",
			exporter, ExporterNoop, ExporterStdout, ExporterFile)
	}

	return &ObservabilityConfig{
		OTelEnabled: enabled,
		ServiceName: envOrDefault("AILERON_OTEL_SERVICE_NAME", defaultOTelServiceName),
		Exporter:    exporter,
		TracesDir:   resolveTracesDir(),
	}, nil
}

// resolveTracesDir returns the state directory the file exporter
// writes under. Mirrors the audit store's resolveAuditStateDir
// behavior:
//
//   - env unset → default `<home>/.aileron`
//   - env explicitly empty → "" (caller falls back to no-op)
//   - env set → that exact path
//
// When UserHomeDir fails, fall back to ".aileron" (relative) so the
// daemon still starts; the file exporter's MkdirAll will create the
// dir relative to whatever directory the daemon was launched from.
func resolveTracesDir() string {
	if p, ok := os.LookupEnv("AILERON_TRACES_DIR"); ok {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aileron"
	}
	return filepath.Join(home, ".aileron")
}
