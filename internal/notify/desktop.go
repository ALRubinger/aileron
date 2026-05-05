package notify

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
)

// DesktopNotifier delivers approval requests as native operating-system
// notifications (macOS Notification Center, Linux notify-send). The
// surface is best-effort and fails open: when the platform isn't
// supported, the underlying binary is missing, or the shell-out
// errors, the notifier logs a warning and returns nil. Notifications
// are a nice-to-have to nudge the user toward the webapp — if they
// don't fire, the queue still works, the user can still discover
// pending entries by visiting /approvals directly.
//
// Per-platform behavior:
//
//   - darwin: shells out to `osascript -e 'display notification ...'`.
//     Always available on macOS; no install required. Title appears
//     in bold; subtitle below; body in the click-through area.
//   - linux: shells out to `notify-send`. Requires the libnotify
//     bin/devel package (e.g. `apt install libnotify-bin`); we don't
//     install it for the user.
//   - other (windows, freebsd, etc.): no-op. Logged once per Notify
//     call so the operator can see why notifications aren't appearing.
//
// Defense in depth: we never accept arbitrary shell input — every
// argument crosses the boundary as exec.Command argv, never via
// `sh -c`, so an attacker-controlled action name in the Summary can't
// shell-inject. The osascript path passes a single AppleScript
// literal that we sanitize for embedded double quotes, the only
// character that can break the literal.
type DesktopNotifier struct {
	log *slog.Logger
	// runner is the exec hook. Tests inject a fake; production uses
	// runReal which calls exec.CommandContext.
	runner func(ctx context.Context, name string, args ...string) error
	// goos is the platform string used for dispatch. Defaults to
	// runtime.GOOS in NewDesktopNotifier; tests override it directly
	// so every branch is reachable on any host without skip-on-GOOS.
	goos string
}

// NewDesktopNotifier returns a DesktopNotifier wired to real `os/exec`
// and the host's runtime.GOOS for platform dispatch. Pass a nil logger
// to use slog.Default.
func NewDesktopNotifier(log *slog.Logger) *DesktopNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &DesktopNotifier{
		log:    log,
		runner: runReal,
		goos:   runtime.GOOS,
	}
}

// Notify implements [Notifier]. The Summary becomes the notification
// body; ReviewURL is appended so the user has a clickable target.
// Approvers and IntentID are unused on this surface — the desktop
// notifier is the single-user case (#418) where the rich governance
// fields don't apply.
func (n *DesktopNotifier) Notify(ctx context.Context, notif Notification) error {
	switch n.goos {
	case "darwin":
		return n.notifyDarwin(ctx, notif)
	case "linux":
		return n.notifyLinux(ctx, notif)
	default:
		n.log.Warn("desktop notifications not supported on this platform",
			"goos", n.goos,
			"approval_id", notif.ApprovalID,
			"summary", notif.Summary,
		)
		return nil
	}
}

// notifyDarwin invokes osascript with a single `display notification`
// command. Format:
//
//	osascript -e 'display notification "BODY" with title "TITLE" subtitle "SUB"'
//
// Apple's display-notification command takes literal strings; we
// quote-escape any double quotes in the input fields so the literal
// closes correctly. AppleScript has no shell-style command injection
// here because the whole thing crosses as one argv element.
func (n *DesktopNotifier) notifyDarwin(ctx context.Context, notif Notification) error {
	title := "Aileron approval"
	subtitle := truncate(notif.Summary, 80)
	body := notif.ReviewURL
	if body == "" {
		body = "Open the Aileron webapp to approve or deny."
	}
	script := fmt.Sprintf(
		`display notification "%s" with title "%s" subtitle "%s"`,
		escapeAppleScript(body),
		escapeAppleScript(title),
		escapeAppleScript(subtitle),
	)
	if err := n.runner(ctx, "osascript", "-e", script); err != nil {
		n.log.Warn("desktop notification failed (osascript)",
			"error", err,
			"approval_id", notif.ApprovalID,
		)
		return nil
	}
	return nil
}

// notifyLinux invokes notify-send with the standard summary + body
// argument shape. When the binary isn't installed, exec.Command
// returns an error which we swallow with a logged warning.
func (n *DesktopNotifier) notifyLinux(ctx context.Context, notif Notification) error {
	title := "Aileron approval"
	summary := truncate(notif.Summary, 80)
	body := notif.ReviewURL
	if body == "" {
		body = "Open the Aileron webapp to approve or deny."
	}
	combined := summary
	if body != "" {
		combined = summary + "\n" + body
	}
	if err := n.runner(ctx, "notify-send", "-a", "Aileron", title, combined); err != nil {
		n.log.Warn("desktop notification failed (notify-send)",
			"error", err,
			"approval_id", notif.ApprovalID,
			"hint", "install libnotify-bin if missing",
		)
		return nil
	}
	return nil
}

// runReal is the production exec hook. Tests inject a fake to assert
// the right commands are invoked without actually shelling out.
func runReal(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// escapeAppleScript escapes the only AppleScript literal-breaking
// character relevant for notification text: double quotes. Backslashes
// are literal in AppleScript strings (no escape sequences), so this
// is a single-character pass.
func escapeAppleScript(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

// truncate caps a notification field at n characters. Both macOS and
// Linux truncate longer strings anyway, but they do it ungracefully —
// we'd rather control the cutoff than have the OS clip mid-word.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
