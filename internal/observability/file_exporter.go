package observability

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/ALRubinger/aileron/internal/dailypath"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// fileExporter writes OTel spans as JSON-per-line to a daily-rotated
// file under `<stateDir>/traces/spans-YYYY-MM-DD.jsonl`. The path
// convention is shared with the audit log (`<stateDir>/audit/...`)
// via the [dailypath] package, so a `~/.aileron` directory contains
// both surfaces side-by-side and rotates them together.
//
// The exporter does not hold a file handle across calls. Each
// ExportSpans opens today's path with O_APPEND|O_CREATE, writes the
// batch, and closes — same shape the audit FileStore uses for its
// writes (#468). Crossing midnight mid-batch is safe: the next call
// computes a fresh path against the new local date.
//
// Span serialization piggy-backs on stdouttrace's compact-JSON
// emitter — a fresh stdouttrace.Exporter is constructed per call,
// pointed at the open file, then discarded. This avoids hand-rolling
// JSON for every OTel span field; the tradeoff (allocating an
// exporter struct per batch) is trivial since stdouttrace.New does
// no I/O of its own.
type fileExporter struct {
	stateDir string

	mu sync.Mutex
}

// newFileExporter ensures the traces subdirectory exists and returns
// the exporter. Returns an error if the directory cannot be created
// (permissions, disk full); the caller in [Init] surfaces the error
// as a warn-log and degrades to no-op rather than failing daemon
// startup.
func newFileExporter(stateDir string) (*fileExporter, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("traces state dir is empty")
	}
	if err := os.MkdirAll(dailypath.Dir(stateDir, tracesSubdir), 0o755); err != nil {
		return nil, fmt.Errorf("creating traces directory: %w", err)
	}
	return &fileExporter{stateDir: stateDir}, nil
}

// tracesSubdir / tracesPrefix bind the trace exporter to its on-disk
// shape: `<stateDir>/traces/spans-YYYY-MM-DD.jsonl`. Symmetric to
// the auditSubdir / auditPrefix binding in internal/audit.
const (
	tracesSubdir = "traces"
	tracesPrefix = "spans-"
)

// ExportSpans appends each span as one JSON line to today's traces
// file. Concurrency-safe: the OTel SDK's batch processor serializes
// calls upstream, but the mutex defends against multiple processors
// or unusual configurations.
func (e *fileExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	path := dailypath.Path(e.stateDir, tracesSubdir, tracesPrefix)
	e.mu.Lock()
	defer e.mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening traces file %s: %w", path, err)
	}
	defer f.Close()

	// stdouttrace emits compact JSON-per-line by default (PrettyPrint
	// is opt-in), which is exactly the shape jq-friendly tooling
	// expects for the file at rest.
	inner, err := stdouttrace.New(stdouttrace.WithWriter(f))
	if err != nil {
		return fmt.Errorf("constructing inner serializer: %w", err)
	}
	return inner.ExportSpans(ctx, spans)
}

// Shutdown is a no-op: the exporter holds no file handle, no
// goroutine, no batched-but-unflushed state across calls. Each
// ExportSpans is self-contained.
func (e *fileExporter) Shutdown(_ context.Context) error {
	return nil
}

// Compile-time check that fileExporter satisfies the SDK's
// SpanExporter contract.
var _ sdktrace.SpanExporter = (*fileExporter)(nil)
