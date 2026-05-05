package notify

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestLogNotifier_NotifyEmitsStructuredFields verifies that the
// LogNotifier carries every field a downstream log scraper would
// want to filter on. Approval id and the review URL are the
// load-bearing ones — without them the log line tells the operator
// "something happened" but not which approval or where to act.
func TestLogNotifier_NotifyEmitsStructuredFields(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	n := NewLogNotifier(log)

	err := n.Notify(context.Background(), Notification{
		ApprovalID:  "act-1",
		IntentID:    "int-1",
		WorkspaceID: "ws-1",
		Summary:     "send-email on github://x/y",
		ReviewURL:   "http://127.0.0.1:5173/approvals",
		Approvers: []Approver{
			{PrincipalID: "user:alr", DisplayName: "Andrew", Contact: "alr@example.com"},
		},
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`"approval_id":"act-1"`,
		`"intent_id":"int-1"`,
		`"summary":"send-email on github://x/y"`,
		`"review_url":"http://127.0.0.1:5173/approvals"`,
		`"approvers":1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log line missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestNewLogNotifier_NonNilLogger covers the constructor's basic
// contract: pass a logger, get a notifier wired to it.
func TestNewLogNotifier_NonNilLogger(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	n := NewLogNotifier(log)
	if n == nil {
		t.Fatal("NewLogNotifier returned nil")
	}
	if n.log != log {
		t.Error("logger not stored on notifier")
	}
}

// TestMulti_NotifyDispatchesToAll asserts that NewMulti sends a single
// Notification to every wrapped notifier, in order. Used by the app
// wiring to compose log + desktop (and future Slack / email).
func TestMulti_NotifyDispatchesToAll(t *testing.T) {
	calls := 0
	notif := &recordingNotifier{onNotify: func(_ Notification) error {
		calls++
		return nil
	}}
	m := NewMulti(notif, notif, notif)
	if err := m.Notify(context.Background(), Notification{ApprovalID: "act-1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// TestMulti_NotifyCollectsErrors asserts that errors from individual
// notifiers don't stop dispatch to subsequent ones, but are returned
// to the caller as a MultiError. Important so an unreliable Slack
// webhook can't silently break the always-works log notifier in the
// same chain.
func TestMulti_NotifyCollectsErrors(t *testing.T) {
	good := &recordingNotifier{onNotify: func(_ Notification) error { return nil }}
	bad := &recordingNotifier{onNotify: func(_ Notification) error {
		return &MultiError{Errors: []error{simpleErr("boom")}}
	}}
	m := NewMulti(good, bad, good)
	err := m.Notify(context.Background(), Notification{ApprovalID: "act-1"})
	if err == nil {
		t.Fatal("Notify err = nil, want MultiError")
	}
	merr, ok := err.(*MultiError)
	if !ok {
		t.Fatalf("err type = %T, want *MultiError", err)
	}
	if len(merr.Errors) != 1 {
		t.Errorf("collected %d errors, want 1", len(merr.Errors))
	}
	if !strings.Contains(merr.Error(), "boom") {
		t.Errorf("MultiError.Error() = %q, missing root cause", merr.Error())
	}
	// All three notifiers should have been called despite the middle one erroring.
	if good.callCount+bad.callCount != 3 {
		t.Errorf("total notifier calls = %d (good=%d, bad=%d), want 3",
			good.callCount+bad.callCount, good.callCount, bad.callCount)
	}
}

// recordingNotifier is a minimal Notifier implementation for tests:
// counts calls and forwards to a hook.
type recordingNotifier struct {
	callCount int
	onNotify  func(Notification) error
}

func (r *recordingNotifier) Notify(_ context.Context, n Notification) error {
	r.callCount++
	if r.onNotify != nil {
		return r.onNotify(n)
	}
	return nil
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }
