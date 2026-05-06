package observability

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/dailypath"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// File exporter contract:
//   - Writes spans to <stateDir>/traces/spans-YYYY-MM-DD.jsonl,
//     using the dailypath convention shared with the audit log.
//   - Empty batches are a no-op (don't create empty files).
//   - Each span emerges as one JSON line; the file is parseable by
//     standard JSONL tooling.
//   - Multiple ExportSpans calls append (don't overwrite).
//   - Empty stateDir is rejected at construction time.
//   - Shutdown is a no-op (no held file handle to flush).

func TestNewFileExporter_RejectsEmptyStateDir(t *testing.T) {
	if _, err := newFileExporter(""); err == nil {
		t.Fatal("expected error for empty stateDir")
	}
}

func TestNewFileExporter_CreatesTracesSubdir(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := newFileExporter(stateDir); err != nil {
		t.Fatalf("newFileExporter: %v", err)
	}
	tracesDir := dailypath.Dir(stateDir, tracesSubdir)
	if info, err := os.Stat(tracesDir); err != nil || !info.IsDir() {
		t.Fatalf("expected traces subdir at %s; err=%v", tracesDir, err)
	}
}

func TestFileExporter_WritesSpansToTodaysFile(t *testing.T) {
	stateDir := t.TempDir()
	exp, err := newFileExporter(stateDir)
	if err != nil {
		t.Fatalf("newFileExporter: %v", err)
	}

	// Drive a span through a synchronous SDK provider configured to
	// export to our file exporter.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "exported-span")
	span.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	wantPath := dailypath.PathAt(stateDir, tracesSubdir, tracesPrefix, time.Now())
	contents, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("reading traces file %s: %v", wantPath, err)
	}
	body := string(contents)
	if !strings.Contains(body, "exported-span") {
		t.Errorf("expected span name in file; got %q", body)
	}
	// Sanity-check the file is JSONL: at least one non-empty line
	// must parse as valid JSON.
	parsedAny := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Errorf("file contains non-JSON line: %q (%v)", line, err)
			continue
		}
		parsedAny = true
	}
	if !parsedAny {
		t.Errorf("expected at least one parseable JSON line in %q", body)
	}
}

func TestFileExporter_EmptyBatchIsNoOp(t *testing.T) {
	stateDir := t.TempDir()
	exp, err := newFileExporter(stateDir)
	if err != nil {
		t.Fatalf("newFileExporter: %v", err)
	}
	// Pass nil; ExportSpans must early-return without I/O.
	if err := exp.ExportSpans(context.Background(), nil); err != nil {
		t.Errorf("ExportSpans on empty batch returned error: %v", err)
	}
	// The file must not have been created — empty batches mustn't
	// produce empty files (which would make grep tooling miss
	// something / nothing in the absence of meaningful events).
	wantPath := dailypath.PathAt(stateDir, tracesSubdir, tracesPrefix, time.Now())
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Errorf("traces file should not exist for empty batch; stat err=%v", err)
	}
}

func TestFileExporter_SubsequentCallsAppend(t *testing.T) {
	stateDir := t.TempDir()
	exp, err := newFileExporter(stateDir)
	if err != nil {
		t.Fatalf("newFileExporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tracer := tp.Tracer("test")
	for _, name := range []string{"first-span", "second-span", "third-span"} {
		_, span := tracer.Start(context.Background(), name)
		span.End()
	}
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	contents, err := os.ReadFile(filepath.Join(stateDir, "traces", "spans-"+time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(contents)
	for _, name := range []string{"first-span", "second-span", "third-span"} {
		if !strings.Contains(body, name) {
			t.Errorf("expected %q in appended file; got %q", name, body)
		}
	}
}

func TestFileExporter_ShutdownIsNoOp(t *testing.T) {
	stateDir := t.TempDir()
	exp, err := newFileExporter(stateDir)
	if err != nil {
		t.Fatalf("newFileExporter: %v", err)
	}
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown returned error: %v", err)
	}
	// Shutdown is idempotent — multiple calls are fine.
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown returned error: %v", err)
	}
}

func TestNewExporter_DispatchesFileByConfig(t *testing.T) {
	// The newExporter dispatch in tracing.go must route Exporter=file
	// through to newFileExporter with the configured TracesDir. This
	// is what wires the config-driven choice into the global Init
	// code path.
	stateDir := t.TempDir()
	cfg := &config.ObservabilityConfig{
		Exporter:  config.ExporterFile,
		TracesDir: stateDir,
	}
	exp, err := newExporter(cfg, nil /* stdoutW unused for file */)
	if err != nil {
		t.Fatalf("newExporter(file): %v", err)
	}
	if exp == nil {
		t.Fatal("expected file exporter, got nil")
	}
	if info, err := os.Stat(filepath.Join(stateDir, "traces")); err != nil || !info.IsDir() {
		t.Errorf("traces subdir not created at construction; err=%v", err)
	}
}
