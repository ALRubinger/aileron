package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Span emission contract for ActionApproval.Wait:
//
//   - Covers the blocked-on-user interval as aileron.approval.wait.
//   - Attribute keys mirror the audit-event keys emitted by Register
//     and Decide (PR #460) so consumers correlate the audit trail
//     with the span by `aileron.approval.id`.
//   - Approve / deny: span Ok (default), decision attribute set,
//     wait_ms reflects RequestedAt → DecidedAt.
//   - Timeout: span Error with the ErrActionApprovalTimeout message;
//     decision = "timeout".
//   - Caller-context cancel: span Error with ctx.Err() message;
//     decision = "cancelled".

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

func attrString(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsString(), true
		}
	}
	return "", false
}

func attrInt64(s sdktrace.ReadOnlySpan, key string) (int64, bool) {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsInt64(), true
		}
	}
	return 0, false
}

func TestWait_ApproveEmitsSpanOkWithDecisionAttrs(t *testing.T) {
	exp := withInMemoryExporter(t)

	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "github://x/y", "sess-1", nil)

	done := make(chan struct{})
	go func() {
		_, _ = a.Wait(context.Background(), time.Second)
		close(done)
	}()
	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	<-done

	spans := exp.GetSpans().Snapshots()
	got, ok := findSpan(spans, "aileron.approval.wait")
	if !ok {
		t.Fatalf("aileron.approval.wait span not emitted; got %d spans", len(spans))
	}
	if got.Status().Code != 0 /* codes.Unset */ {
		t.Errorf("span status = %v, want Unset on approve", got.Status())
	}
	if v, _ := attrString(got, "aileron.approval.id"); v != a.ID {
		t.Errorf("aileron.approval.id = %q, want %q", v, a.ID)
	}
	if v, _ := attrString(got, "aileron.approval.action"); v != "send-email" {
		t.Errorf("aileron.approval.action = %q, want send-email", v)
	}
	if v, _ := attrString(got, "aileron.approval.kind"); v != string(ApprovalKindAction) {
		t.Errorf("aileron.approval.kind = %q, want %q", v, ApprovalKindAction)
	}
	if v, _ := attrString(got, "aileron.connector.fqn"); v != "github://x/y" {
		t.Errorf("aileron.connector.fqn = %q", v)
	}
	if v, _ := attrString(got, "aileron.session.id"); v != "sess-1" {
		t.Errorf("aileron.session.id = %q", v)
	}
	if v, _ := attrString(got, "aileron.approval.decision"); v != "approved" {
		t.Errorf("aileron.approval.decision = %q, want approved", v)
	}
	if _, ok := attrInt64(got, "aileron.approval.wait_ms"); !ok {
		t.Errorf("aileron.approval.wait_ms missing from span")
	}
}

func TestWait_DenyEmitsSpanOkWithDeniedDecision(t *testing.T) {
	exp := withInMemoryExporter(t)

	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "", "", nil)

	done := make(chan struct{})
	go func() {
		_, _ = a.Wait(context.Background(), time.Second)
		close(done)
	}()
	if err := q.Decide(a.ID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	<-done

	spans := exp.GetSpans().Snapshots()
	got, ok := findSpan(spans, "aileron.approval.wait")
	if !ok {
		t.Fatal("aileron.approval.wait span not emitted")
	}
	// Deny is a deliberate user decision, not a system failure — span
	// status stays Ok. The decision attribute carries the verdict.
	if got.Status().Code != 0 /* codes.Unset */ {
		t.Errorf("span status = %v, want Unset on deny (deny is a valid decision, not a span error)", got.Status())
	}
	if v, _ := attrString(got, "aileron.approval.decision"); v != "denied" {
		t.Errorf("aileron.approval.decision = %q, want denied", v)
	}
	// Optional attrs absent when not set on Register.
	if v, present := attrString(got, "aileron.connector.fqn"); present {
		t.Errorf("aileron.connector.fqn should be absent when empty on Register; got %q", v)
	}
	if v, present := attrString(got, "aileron.session.id"); present {
		t.Errorf("aileron.session.id should be absent when empty on Register; got %q", v)
	}
}

func TestWait_TimeoutMarksSpanErrorWithTimeoutDecision(t *testing.T) {
	exp := withInMemoryExporter(t)

	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "", "", nil)

	_, err := a.Wait(context.Background(), 10*time.Millisecond)
	if !errors.Is(err, ErrActionApprovalTimeout) {
		t.Fatalf("Wait err = %v, want ErrActionApprovalTimeout", err)
	}

	spans := exp.GetSpans().Snapshots()
	got, ok := findSpan(spans, "aileron.approval.wait")
	if !ok {
		t.Fatal("aileron.approval.wait span not emitted")
	}
	if got.Status().Code != 1 /* codes.Error */ {
		t.Errorf("span status = %v, want Error on timeout", got.Status())
	}
	if v, _ := attrString(got, "aileron.approval.decision"); v != "timeout" {
		t.Errorf("aileron.approval.decision = %q, want timeout", v)
	}
}

func TestWait_ContextCancelledMarksSpanErrorWithCancelledDecision(t *testing.T) {
	exp := withInMemoryExporter(t)

	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Wait(ctx, time.Second)
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait err = %v, want context.Canceled", err)
	}

	spans := exp.GetSpans().Snapshots()
	got, ok := findSpan(spans, "aileron.approval.wait")
	if !ok {
		t.Fatal("aileron.approval.wait span not emitted")
	}
	if got.Status().Code != 1 /* codes.Error */ {
		t.Errorf("span status = %v, want Error on ctx cancel", got.Status())
	}
	if v, _ := attrString(got, "aileron.approval.decision"); v != "cancelled" {
		t.Errorf("aileron.approval.decision = %q, want cancelled", v)
	}
}

func TestWait_NoOpProviderDoesNotPanic(t *testing.T) {
	// Default OTel global is no-op until daemon Init runs. Wait must
	// still work — span ops are no-ops.
	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "", "", nil)

	done := make(chan struct{})
	go func() {
		_, _ = a.Wait(context.Background(), time.Second)
		close(done)
	}()
	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	<-done
}
