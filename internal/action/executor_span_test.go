package action

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// Span emission contract for SandboxExecutor.Execute:
//
//   - aileron.action.execute span wraps the full call. Carries
//     aileron.action.name and aileron.action.steps_count. On failure
//     (Result.Failure non-nil OR returned err non-nil) the span
//     status is Error and the OTel-shaped failure attributes are
//     attached when there's a structured failure.
//
//   - aileron.connector.call span wraps each step's conn.Invoke.
//     Child of action.execute. Carries aileron.connector.{fqn,op,hash}.
//     Invoke errors mark the span Error.
//
//   - Capability denial short-circuits before the connector.call —
//     the connector.call span is never started for denied steps.
//
// The audit-shape attribute keys here match PR #452's payload schema
// 1:1 — span attributes and audit payload share a single source of
// truth.

// withInMemoryExporter installs a sync-flushed tracer provider whose
// exported spans land in an in-memory collector. Returns the
// collector and restores the previous global on cleanup.
func withInMemoryExporter(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exp
}

func findSpan(spans []sdktrace.ReadOnlySpan, name string) (sdktrace.ReadOnlySpan, bool) {
	for _, s := range spans {
		if s.Name() == name {
			return s, true
		}
	}
	return nil, false
}

func attr(s sdktrace.ReadOnlySpan, key string) (any, bool) {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsInterface(), true
		}
	}
	return nil, false
}

func TestSandboxExecutor_Execute_EmitsActionAndConnectorSpans(t *testing.T) {
	exp := withInMemoryExporter(t)

	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"echo"}, []string{"echo"})

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{"value": "hi"}}}

	exec := NewSandboxExecutor(actions, store, rt, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", map[string]any{"value": "hi"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	spans := exp.GetSpans().Snapshots()
	actSpan, ok := findSpan(spans, "aileron.action.execute")
	if !ok {
		t.Fatalf("aileron.action.execute span not emitted; got %d spans", len(spans))
	}
	if v, _ := attr(actSpan, "aileron.action.name"); v != "test-action" {
		t.Errorf("aileron.action.name = %v, want test-action", v)
	}
	if v, _ := attr(actSpan, "aileron.action.steps_count"); v != int64(1) {
		t.Errorf("aileron.action.steps_count = %v, want 1", v)
	}

	connSpan, ok := findSpan(spans, "aileron.connector.call")
	if !ok {
		t.Fatal("aileron.connector.call span not emitted")
	}
	if v, _ := attr(connSpan, "aileron.connector.fqn"); v != "github://test/echo" {
		t.Errorf("aileron.connector.fqn = %v", v)
	}
	if v, _ := attr(connSpan, "aileron.connector.op"); v != "echo" {
		t.Errorf("aileron.connector.op = %v", v)
	}
	if v, _ := attr(connSpan, "aileron.connector.hash"); v != hash {
		t.Errorf("aileron.connector.hash = %v, want %q", v, hash)
	}

	// Connector span must be a child of the action span.
	if connSpan.Parent().SpanID() != actSpan.SpanContext().SpanID() {
		t.Errorf("connector.call parent span = %v, want action.execute span %v",
			connSpan.Parent().SpanID(), actSpan.SpanContext().SpanID())
	}
}

func TestSandboxExecutor_Execute_CapabilityDeniedMarksSpanError(t *testing.T) {
	// When the action-boundary check denies an op, no connector.call
	// span is started; the action span captures the failure via
	// Result.Failure, gets marked Error, and the OTel-shaped
	// aileron.failure.* attributes attach.
	exp := withInMemoryExporter(t)

	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	// Capability list omits "echo" — the only execute step's op.
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"other_op"}, []string{"echo"})

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{"ok": true}}}

	exec := NewSandboxExecutor(actions, store, rt, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", nil)
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if res.Failure == nil {
		t.Fatal("expected structured failure; got nil")
	}

	spans := exp.GetSpans().Snapshots()
	if _, ok := findSpan(spans, "aileron.connector.call"); ok {
		t.Error("aileron.connector.call span should not be emitted on capability denial")
	}
	actSpan, ok := findSpan(spans, "aileron.action.execute")
	if !ok {
		t.Fatal("aileron.action.execute span not emitted")
	}
	if actSpan.Status().Code != 1 /* codes.Error */ {
		t.Errorf("action.execute span status = %v, want Error", actSpan.Status())
	}
	if v, _ := attr(actSpan, "aileron.failure.class"); v == nil || v == "" {
		t.Errorf("aileron.failure.class missing from action.execute span")
	}
	if v, _ := attr(actSpan, "aileron.failure.boundary"); v == nil || v == "" {
		t.Errorf("aileron.failure.boundary missing from action.execute span")
	}
	if _, ok := attr(actSpan, "aileron.failure.retriable"); !ok {
		t.Errorf("aileron.failure.retriable missing from action.execute span")
	}
}

func TestSandboxExecutor_Execute_InvokeErrorMarksConnectorSpanError(t *testing.T) {
	// A sandbox.Error returned from Invoke maps to a structured
	// failure on the action span. The connector.call span itself is
	// also marked Error so consumers filtering on span status can
	// pinpoint the failing step.
	exp := withInMemoryExporter(t)

	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"echo"}, []string{"echo"})

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {
		err: &sandbox.Error{Class: sandbox.ClassCapabilityDenied, Message: "denied at sandbox boundary"},
	}}

	exec := NewSandboxExecutor(actions, store, rt, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}

	spans := exp.GetSpans().Snapshots()
	connSpan, ok := findSpan(spans, "aileron.connector.call")
	if !ok {
		t.Fatal("connector.call span not emitted")
	}
	if connSpan.Status().Code != 1 /* codes.Error */ {
		t.Errorf("connector.call status = %v, want Error", connSpan.Status())
	}
	if !strings.Contains(connSpan.Status().Description, "denied at sandbox boundary") {
		t.Errorf("connector.call status description = %q, want containing %q",
			connSpan.Status().Description, "denied at sandbox boundary")
	}
}

func TestSandboxExecutor_Execute_ActionNotFoundMarksActionSpanError(t *testing.T) {
	// Gateway-fatal Go error path: action lookup miss. The action
	// span is the only span emitted; its status is Error with the
	// Go error's message.
	exp := withInMemoryExporter(t)

	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"echo"}, []string{"echo"})

	exec := NewSandboxExecutor(actions, store, &fakeRuntime{}, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "no-such-action", nil); err == nil {
		t.Fatal("expected Go error for missing action")
	}

	spans := exp.GetSpans().Snapshots()
	actSpan, ok := findSpan(spans, "aileron.action.execute")
	if !ok {
		t.Fatal("aileron.action.execute span not emitted")
	}
	if actSpan.Status().Code != 1 /* codes.Error */ {
		t.Errorf("action.execute status = %v, want Error", actSpan.Status())
	}
	if _, ok := findSpan(spans, "aileron.connector.call"); ok {
		t.Error("aileron.connector.call span should not be emitted when no action found")
	}
}

func TestSandboxExecutor_Execute_NoOpProviderDoesNotCrash(t *testing.T) {
	// With the OTel global at the no-op provider (the daemon startup
	// default until AILERON_OTEL_ENABLED=true), Execute must still
	// work. Guards against accidental hard-dependencies on the SDK
	// being installed.
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"echo"}, []string{"echo"})

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{"v": 1}}}

	exec := NewSandboxExecutor(actions, store, rt, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
