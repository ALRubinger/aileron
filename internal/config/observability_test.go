package config

import (
	"os"
	"strings"
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

func TestLoadObservabilityConfig_AcceptsFileExporter(t *testing.T) {
	t.Setenv("AILERON_OTEL_ENABLED", "true")
	t.Setenv("AILERON_OTEL_EXPORTER", "file")
	cfg, err := LoadObservabilityConfig()
	if err != nil {
		t.Fatalf("LoadObservabilityConfig: %v", err)
	}
	if got, want := cfg.Exporter, ExporterFile; got != want {
		t.Errorf("Exporter = %q, want %q", got, want)
	}
}

func TestLoadObservabilityConfig_AcceptsOTLPExporter(t *testing.T) {
	t.Setenv("AILERON_OTEL_ENABLED", "true")
	t.Setenv("AILERON_OTEL_EXPORTER", "otlp")
	cfg, err := LoadObservabilityConfig()
	if err != nil {
		t.Fatalf("LoadObservabilityConfig: %v", err)
	}
	if got, want := cfg.Exporter, ExporterOTLP; got != want {
		t.Errorf("Exporter = %q, want %q", got, want)
	}
}

func TestLoadObservabilityConfig_TracesDirHonorsEnv(t *testing.T) {
	t.Setenv("AILERON_TRACES_DIR", "/var/lib/aileron/x")
	cfg, err := LoadObservabilityConfig()
	if err != nil {
		t.Fatalf("LoadObservabilityConfig: %v", err)
	}
	if got, want := cfg.TracesDir, "/var/lib/aileron/x"; got != want {
		t.Errorf("TracesDir = %q, want %q", got, want)
	}
}

func TestLoadObservabilityConfig_TracesDirEmptyEnvIsExplicitlyEmpty(t *testing.T) {
	// Symmetric to the audit store's resolveAuditStateDir: an
	// explicit empty env var means "don't write to disk", as
	// distinct from "use the default location."
	t.Setenv("AILERON_TRACES_DIR", "")
	cfg, err := LoadObservabilityConfig()
	if err != nil {
		t.Fatalf("LoadObservabilityConfig: %v", err)
	}
	if cfg.TracesDir != "" {
		t.Errorf("TracesDir = %q, want empty", cfg.TracesDir)
	}
}

func TestLoadObservabilityConfig_TracesDirDefaultsUnderHome(t *testing.T) {
	// When no env var is set, TracesDir resolves to <home>/.aileron.
	// We don't pin the exact value since the home dir varies per
	// machine; assert it ends with the expected suffix.
	if err := os.Unsetenv("AILERON_TRACES_DIR"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	cfg, err := LoadObservabilityConfig()
	if err != nil {
		t.Fatalf("LoadObservabilityConfig: %v", err)
	}
	if cfg.TracesDir == "" {
		t.Fatal("expected non-empty TracesDir default; got empty")
	}
	if !strings.HasSuffix(cfg.TracesDir, ".aileron") {
		t.Errorf("TracesDir = %q, want suffix .aileron", cfg.TracesDir)
	}
}
