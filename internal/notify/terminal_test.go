package notify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// TestTerminalNotifier_WritesFormattedBlock pins down the contract:
// header line names the action, an `open:` line carries the review
// URL, and an `or:` line carries the `aileron open approval <id>`
// hint. Leading and trailing newlines bracket the block so it stays
// readable when interleaved with other log lines.
func TestTerminalNotifier_WritesFormattedBlock(t *testing.T) {
	var buf bytes.Buffer
	n := NewTerminalNotifier(&buf, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-42",
		Summary:    "send-email on github://x/y",
		ReviewURL:  "http://127.0.0.1:50419/approvals?focus=act-42",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"aileron: approval required — send-email on github://x/y",
		"open: http://127.0.0.1:50419/approvals?focus=act-42",
		"or:   aileron open approval act-42",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "\n") || !strings.HasSuffix(out, "\n\n") {
		t.Errorf("expected leading newline and trailing blank line; got %q", out)
	}
}

// TestTerminalNotifier_FallbackURLPromptWhenEmpty: when ReviewURL is
// empty (no WebappURL / AILERON_WEBAPP_URL configured), the block still
// fires and points the user at how to fix it rather than silently
// dropping the URL line.
func TestTerminalNotifier_FallbackURLPromptWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	n := NewTerminalNotifier(&buf, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	_ = n.Notify(context.Background(), Notification{
		ApprovalID: "act-1",
		Summary:    "send-email",
		ReviewURL:  "",
	})
	if !strings.Contains(buf.String(), "AILERON_WEBAPP_URL") {
		t.Errorf("expected fallback prompt when ReviewURL is empty; got %q", buf.String())
	}
}

// TestTerminalNotifier_FallbackCommandPlaceholderWhenIDEmpty: an
// approval registered without an ID (defensive — shouldn't happen
// today) still emits a usable `aileron open approval <id>` template
// rather than `aileron open approval ` with a dangling trailing space.
func TestTerminalNotifier_FallbackCommandPlaceholderWhenIDEmpty(t *testing.T) {
	var buf bytes.Buffer
	n := NewTerminalNotifier(&buf, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	_ = n.Notify(context.Background(), Notification{
		ApprovalID: "",
		Summary:    "send-email",
		ReviewURL:  "http://example.com/approvals",
	})
	if !strings.Contains(buf.String(), "aileron open approval <id>") {
		t.Errorf("expected <id> placeholder when ApprovalID empty; got %q", buf.String())
	}
}

// TestTerminalNotifier_EmptySummaryDropsDash: if Summary is empty the
// header collapses to `aileron: approval required` with no trailing
// em-dash. Avoids a dangling separator in the log.
func TestTerminalNotifier_EmptySummaryDropsDash(t *testing.T) {
	var buf bytes.Buffer
	n := NewTerminalNotifier(&buf, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	_ = n.Notify(context.Background(), Notification{
		ApprovalID: "act-1",
		Summary:    "",
		ReviewURL:  "http://example.com/approvals",
	})
	out := buf.String()
	if !strings.Contains(out, "aileron: approval required\n") {
		t.Errorf("expected bare header line; got %q", out)
	}
	if strings.Contains(out, "approval required —") {
		t.Errorf("expected no trailing separator on empty summary; got %q", out)
	}
}

// errorWriter is an io.Writer that always fails. Used to verify the
// fail-soft contract — a stderr that can't be written to shouldn't
// break the approval flow.
type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("simulated stderr failure") }

// TestTerminalNotifier_WriterErrorReturnsNil: when the underlying
// writer errors (broken pipe, full disk), Notify returns nil. The
// queue's invariants don't depend on notifications firing.
func TestTerminalNotifier_WriterErrorReturnsNil(t *testing.T) {
	n := NewTerminalNotifier(errorWriter{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-1", Summary: "send-email", ReviewURL: "http://example.com",
	})
	if err != nil {
		t.Errorf("Notify err = %v, want nil (fail-soft)", err)
	}
}

// TestNewTerminalNotifier_NilWriterDoesNotPanic asserts the constructor
// degrades to io.Discard when callers pass nil — production wiring may
// not always have a writer handy at construction time, and a panic
// here would be a strictly worse failure mode than silent-correct.
func TestNewTerminalNotifier_NilWriterDoesNotPanic(t *testing.T) {
	n := NewTerminalNotifier(nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	err := n.Notify(context.Background(), Notification{ApprovalID: "act-1"})
	if err != nil {
		t.Errorf("Notify err = %v, want nil", err)
	}
}

// TestNewTerminalNotifier_NilLoggerUsesDefault: the constructor should
// gracefully accept a nil logger (production may build the notifier
// before the daemon's logger is configured).
func TestNewTerminalNotifier_NilLoggerUsesDefault(t *testing.T) {
	n := NewTerminalNotifier(io.Discard, nil)
	if n.log == nil {
		t.Error("log is nil after NewTerminalNotifier(_, nil); want slog.Default")
	}
}

// TestTerminalNotifier_ConcurrentNotifyDoesNotInterleave guards the
// internal mutex: two notifications fired in parallel must not
// interleave their bytes. Without the lock, the formatted blocks would
// scramble when slog handlers / SSE listeners notify concurrently.
func TestTerminalNotifier_ConcurrentNotifyDoesNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	n := NewTerminalNotifier(&buf, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = n.Notify(context.Background(), Notification{
				ApprovalID: "act-" + string(rune('0'+i)),
				Summary:    "act-" + string(rune('0'+i)),
				ReviewURL:  "http://example.com/approvals",
			})
		}(i)
	}
	wg.Wait()

	// The output should be a concatenation of 10 complete blocks. The
	// header line appears once per block; if writes interleaved we'd
	// see fewer (or partial) header lines.
	headers := strings.Count(buf.String(), "aileron: approval required")
	if headers != 10 {
		t.Errorf("got %d header lines, want 10 (writes likely interleaved)", headers)
	}
}
