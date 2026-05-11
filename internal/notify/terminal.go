package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// TerminalNotifier writes approval notifications as a human-readable
// multi-line block to an io.Writer. Production wiring points it at the
// daemon's stderr (which the daemon redirects into
// `~/.aileron/daemon.log`); the user can follow that file, or reach the
// queue via the webapp at the URL printed in the block.
//
// Output shape (with leading and trailing blank lines so the block
// stands out in a tail of mixed log output):
//
//	aileron: approval required — <Summary>
//	  open: <ReviewURL>
//	  or:   aileron open approval <ApprovalID>
//
// Both lines under the header are designed to be useful from a
// different terminal than the one running the agent: the URL is
// directly openable (most terminals turn it into a clickable link) and
// the `aileron open approval <id>` form lets the user trigger the same
// open from any shell on the host.
//
// Replaces the previous DesktopNotifier (osascript / notify-send),
// which surfaced notifications that had no working click action on
// macOS — clicking the notification opened Finder rather than the
// review URL, which was strictly worse than no notification at all.
type TerminalNotifier struct {
	mu  sync.Mutex
	w   io.Writer
	log *slog.Logger
}

// NewTerminalNotifier returns a TerminalNotifier writing to w. Passing
// nil for w substitutes io.Discard so production callers that mis-wire
// the notifier degrade to silent-but-correct rather than panicking.
// Passing nil for log uses slog.Default.
func NewTerminalNotifier(w io.Writer, log *slog.Logger) *TerminalNotifier {
	if w == nil {
		w = io.Discard
	}
	if log == nil {
		log = slog.Default()
	}
	return &TerminalNotifier{w: w, log: log}
}

// Notify writes the formatted block to the configured writer. Fails
// soft: a writer error is logged and swallowed so a transient stderr
// problem can't break the approval flow itself (the queue's invariants
// don't depend on notifications firing).
func (n *TerminalNotifier) Notify(_ context.Context, notif Notification) error {
	header := "aileron: approval required"
	if notif.Summary != "" {
		header = header + " — " + notif.Summary
	}
	url := notif.ReviewURL
	if url == "" {
		url = "(set AILERON_WEBAPP_URL to enable a review link)"
	}
	cmd := "aileron open approval <id>"
	if notif.ApprovalID != "" {
		cmd = "aileron open approval " + notif.ApprovalID
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := fmt.Fprintf(n.w, "\n%s\n  open: %s\n  or:   %s\n\n", header, url, cmd); err != nil {
		n.log.Warn("terminal notification write failed",
			"error", err,
			"approval_id", notif.ApprovalID,
		)
	}
	return nil
}
