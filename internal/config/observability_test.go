package config

import (
	"testing"
)

// LoadObservabilityConfig contract:
//   - Defaults: disabled, service name "aileron", exporter "noop".
//   - AILERON_OTEL_ENABLED parses Go-style bool ("true"/"false"/"1"/"0"),
//     rejects garbage.
//   - AILERON_OTEL_SERVICE_NAME overrides the default; whitespace is trimmed.
//   - AILERON_OTEL_EXPORTER accepts only "noop" or "stdout"; anything else
//     is a load-time error.

func TestLoadObservabilityConfig_Defaults(t *testing.T) {
	t.Setenv("AILERON_OTEL_ENABLED", "")
	t.Setenv("AILERON_OTEL_SERVICE_NAME", "")
	t.Setenv("AILERON_OTEL_EXPORTER", "")

	cfg, err := LoadObservabilityConfig()
	if err != nil {
		t.Fatalf("LoadObservabilityConfig: %v", err)
	}
	if cfg.OTelEnabled {
		t.Error("OTelEnabled = true, want false")
	}
	if got, want := cfg.ServiceName, "aileron"; got != want {
		t.Errorf("ServiceName = %q, want %q", got, want)
	}
	if got, want := cfg.Exporter, ExporterNoop; got != want {
		t.Errorf("Exporter = %q, want %q", got, want)
	}
}

func TestLoadObservabilityConfig_EnabledTrue(t *testing.T) {
	t.Setenv("AILERON_OTEL_ENABLED", "true")
	t.Setenv("AILERON_OTEL_EXPORTER", "stdout")

	cfg, err := LoadObservabilityConfig()
	if err != nil {
		t.Fatalf("LoadObservabilityConfig: %v", err)
	}
	if !cfg.OTelEnabled {
		t.Error("OTelEnabled = false, want true")
	}
	if got, want := cfg.Exporter, ExporterStdout; got != want {
		t.Errorf("Exporter = %q, want %q", got, want)
	}
}

func TestLoadObservabilityConfig_RejectsBadEnabled(t *testing.T) {
	t.Setenv("AILERON_OTEL_ENABLED", "yes-please")
	if _, err := LoadObservabilityConfig(); err == nil {
		t.Fatal("expected error for malformed AILERON_OTEL_ENABLED, got nil")
	}
}

func TestLoadObservabilityConfig_RejectsUnknownExporter(t *testing.T) {
	t.Setenv("AILERON_OTEL_EXPORTER", "datadog")
	if _, err := LoadObservabilityConfig(); err == nil {
		t.Fatal("expected error for unknown AILERON_OTEL_EXPORTER, got nil")
	}
}

func TestLoadObservabilityConfig_TrimsServiceName(t *testing.T) {
	t.Setenv("AILERON_OTEL_SERVICE_NAME", "  aileron-staging  ")
	cfg, err := LoadObservabilityConfig()
	if err != nil {
		t.Fatalf("LoadObservabilityConfig: %v", err)
	}
	if got, want := cfg.ServiceName, "aileron-staging"; got != want {
		t.Errorf("ServiceName = %q, want %q", got, want)
	}
}
