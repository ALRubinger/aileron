package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"
)

// recordingRunner is a fake exec hook used by the tests below to
// capture argv without shelling out. cmds is the sequence of recorded
// invocations (one entry per Run call); err is what each call returns.
type recordingRunner struct {
	cmds [][]string
	err  error
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) error {
	r.cmds = append(r.cmds, append([]string{name}, args...))
	return r.err
}

// newTestNotifier returns a DesktopNotifier with the supplied exec
// runner and platform string. Driving the platform dispatch via the
// `goos` field — rather than runtime.GOOS — lets every branch run on
// any host without skip-on-GOOS (which would leak coverage on CI
// systems that don't have one of the platforms).
func newTestNotifier(goos string, runner func(ctx context.Context, name string, args ...string) error) *DesktopNotifier {
	return &DesktopNotifier{
		log:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		runner: runner,
		goos:   goos,
	}
}

// TestNewDesktopNotifier_DefaultsToHostGOOS covers the constructor:
// production wiring should pick up the host's runtime.GOOS so the
// dispatch picks the right platform branch without any extra config.
func TestNewDesktopNotifier_DefaultsToHostGOOS(t *testing.T) {
	n := NewDesktopNotifier(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if n.goos != runtime.GOOS {
		t.Errorf("goos = %q, want %q", n.goos, runtime.GOOS)
	}
	if n.runner == nil {
		t.Error("runner is nil; constructor must wire runReal")
	}
}

// TestNewDesktopNotifier_NilLoggerUsesDefault asserts the constructor
// gracefully handles a nil logger argument (production wiring may not
// always have a slog.Logger handy at construction time).
func TestNewDesktopNotifier_NilLoggerUsesDefault(t *testing.T) {
	n := NewDesktopNotifier(nil)
	if n.log == nil {
		t.Error("log is nil after NewDesktopNotifier(nil); want slog.Default")
	}
}

// TestDesktopNotifier_DarwinShellsOutToOSAScript covers the macOS
// path. Every branch runs on every host because we drive `goos`
// directly rather than skipping on runtime.GOOS.
func TestDesktopNotifier_DarwinShellsOutToOSAScript(t *testing.T) {
	rec := &recordingRunner{}
	n := newTestNotifier("darwin", rec.run)

	err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-1",
		Summary:    "send-email on github://x/y",
		ReviewURL:  "http://127.0.0.1:5173/approvals",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(rec.cmds) != 1 {
		t.Fatalf("got %d cmds, want 1: %+v", len(rec.cmds), rec.cmds)
	}
	cmd := rec.cmds[0]
	if cmd[0] != "osascript" {
		t.Errorf("argv[0] = %q, want osascript", cmd[0])
	}
	if len(cmd) != 3 || cmd[1] != "-e" {
		t.Fatalf("argv = %v, want [osascript -e <script>]", cmd)
	}
	script := cmd[2]
	for _, want := range []string{
		`display notification "http://127.0.0.1:5173/approvals"`,
		`with title "Aileron approval"`,
		`subtitle "send-email on github://x/y"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\nfull:\n%s", want, script)
		}
	}
}

// TestDesktopNotifier_LinuxShellsOutToNotifySend mirrors the macOS
// test for the Linux path. notify-send takes a separate `-a` for the
// app name and a `<title>` `<body>` pair, with the body containing
// summary + URL on separate lines.
func TestDesktopNotifier_LinuxShellsOutToNotifySend(t *testing.T) {
	rec := &recordingRunner{}
	n := newTestNotifier("linux", rec.run)

	err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-1",
		Summary:    "send-email",
		ReviewURL:  "http://127.0.0.1:5173/approvals",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(rec.cmds) != 1 {
		t.Fatalf("got %d cmds, want 1: %+v", len(rec.cmds), rec.cmds)
	}
	cmd := rec.cmds[0]
	if cmd[0] != "notify-send" {
		t.Errorf("argv[0] = %q, want notify-send", cmd[0])
	}
	if len(cmd) < 5 {
		t.Fatalf("argv = %v, want at least [notify-send -a Aileron <title> <body>]", cmd)
	}
	if cmd[1] != "-a" || cmd[2] != "Aileron" {
		t.Errorf("argv[1:3] = %v, want [-a Aileron]", cmd[1:3])
	}
	if cmd[3] != "Aileron approval" {
		t.Errorf("title = %q, want Aileron approval", cmd[3])
	}
	body := cmd[4]
	if !strings.Contains(body, "send-email") {
		t.Errorf("body missing summary; got %q", body)
	}
	if !strings.Contains(body, "http://127.0.0.1:5173/approvals") {
		t.Errorf("body missing URL; got %q", body)
	}
}

// TestDesktopNotifier_DarwinFailureReturnsNilAndLogs asserts the
// fail-soft contract on darwin's path: when osascript errors (e.g.
// silently denied by user-config), Notify returns nil. Notifications
// are best-effort; the queue's invariants don't depend on them firing.
func TestDesktopNotifier_DarwinFailureReturnsNilAndLogs(t *testing.T) {
	rec := &recordingRunner{err: errors.New("simulated osascript failure")}
	n := newTestNotifier("darwin", rec.run)
	err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-1", Summary: "send-email", ReviewURL: "http://example.com",
	})
	if err != nil {
		t.Errorf("Notify err = %v, want nil (fail-soft)", err)
	}
}

// TestDesktopNotifier_LinuxFailureReturnsNilAndLogs covers the
// missing-binary case on Linux: notify-send not installed → exec
// returns an error → we swallow with a logged warning.
func TestDesktopNotifier_LinuxFailureReturnsNilAndLogs(t *testing.T) {
	rec := &recordingRunner{err: errors.New("notify-send: command not found")}
	n := newTestNotifier("linux", rec.run)
	err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-1", Summary: "send-email", ReviewURL: "http://example.com",
	})
	if err != nil {
		t.Errorf("Notify err = %v, want nil (fail-soft)", err)
	}
}

// TestDesktopNotifier_UnsupportedPlatformIsNoOp asserts that platforms
// without a desktop-notification path return nil without invoking the
// runner. The runner's recording stays empty.
func TestDesktopNotifier_UnsupportedPlatformIsNoOp(t *testing.T) {
	rec := &recordingRunner{}
	n := newTestNotifier("plan9", rec.run)
	if err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-1",
		Summary:    "send-email",
	}); err != nil {
		t.Errorf("Notify err = %v, want nil", err)
	}
	if len(rec.cmds) != 0 {
		t.Errorf("recorded %d cmds, want 0 on unsupported platform", len(rec.cmds))
	}
}

// TestDesktopNotifier_DarwinFallbackBodyWhenURLEmpty covers the case
// where AILERON_WEBAPP_URL isn't configured: the notification still
// fires with a fallback prompt rather than dropping the URL silently.
// Same fallback applies on linux; this test asserts the darwin
// branch's behavior.
func TestDesktopNotifier_DarwinFallbackBodyWhenURLEmpty(t *testing.T) {
	rec := &recordingRunner{}
	n := newTestNotifier("darwin", rec.run)
	if err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-1", Summary: "send-email", ReviewURL: "",
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(rec.cmds) != 1 {
		t.Fatalf("cmds = %v", rec.cmds)
	}
	combined := strings.Join(rec.cmds[0], " ")
	if !strings.Contains(combined, "Open the Aileron webapp") {
		t.Errorf("expected fallback prompt; got %q", combined)
	}
}

// TestDesktopNotifier_LinuxFallbackBodyWhenURLEmpty mirrors the
// darwin fallback test for linux's notify-send path.
func TestDesktopNotifier_LinuxFallbackBodyWhenURLEmpty(t *testing.T) {
	rec := &recordingRunner{}
	n := newTestNotifier("linux", rec.run)
	if err := n.Notify(context.Background(), Notification{
		ApprovalID: "act-1", Summary: "send-email", ReviewURL: "",
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(rec.cmds) != 1 {
		t.Fatalf("cmds = %v", rec.cmds)
	}
	combined := strings.Join(rec.cmds[0], " ")
	if !strings.Contains(combined, "Open the Aileron webapp") {
		t.Errorf("expected fallback prompt; got %q", combined)
	}
}

// TestDesktopNotifier_EscapesAppleScriptDoubleQuotes verifies that an
// action name carrying double quotes (e.g. `send "weekly recap"`)
// can't break the AppleScript literal — the `\"` escape closes the
// string cleanly. AppleScript has no shell-meta interpretation here
// (the whole script is one argv element), so this is just literal-
// breaking, not an injection vector — but it would still produce a
// non-firing notification without the escape.
func TestDesktopNotifier_EscapesAppleScriptDoubleQuotes(t *testing.T) {
	rec := &recordingRunner{}
	n := newTestNotifier("darwin", rec.run)
	_ = n.Notify(context.Background(), Notification{
		ApprovalID: "act-1",
		Summary:    `send "weekly recap"`,
		ReviewURL:  "http://example.com",
	})
	if len(rec.cmds) != 1 {
		t.Fatalf("cmds = %v", rec.cmds)
	}
	script := rec.cmds[0][2]
	if !strings.Contains(script, `subtitle "send \"weekly recap\""`) {
		t.Errorf("expected escaped quotes in subtitle; got %q", script)
	}
}

// TestEscapeAppleScript covers the helper directly: backslashes are
// literal in AppleScript strings (no escape sequences), so the only
// character to escape is a double quote.
func TestEscapeAppleScript(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{`with "quote"`, `with \"quote\"`},
		{`backslash \ stays`, `backslash \ stays`},
		{"", ""},
	}
	for _, c := range cases {
		if got := escapeAppleScript(c.in); got != c.want {
			t.Errorf("escapeAppleScript(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTruncate covers the cap helper: under-cap stays as-is; at-cap
// stays as-is; over-cap gets truncated with an ellipsis to indicate
// the cut. The n=1 / n=0 edge cases trip up naive implementations
// (subtracting `n-1` for the ellipsis would underflow); the helper
// hard-cuts at byte boundaries instead.
func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exact-len", 9, "exact-len"},
		{"this is a long string", 10, "this is a…"},
		{"", 5, ""},
		{"abc", 1, "a"},  // n=1: ellipsis would leave nothing; hard-cut.
		{"abc", 0, ""},   // n=0: nothing fits.
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
