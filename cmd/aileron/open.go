package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
)

// runOpen dispatches `aileron open [target [id]]` — a small surface
// that resolves the running daemon's webapp URL and shells out to the
// host's OS opener (open / xdg-open / cmd /c start) to launch it in
// the user's default browser.
//
// Subcommands:
//
//   - `aileron open`                       → <webapp>/
//   - `aileron open approvals`             → <webapp>/approvals
//   - `aileron open approval <approval-id>` → <webapp>/approvals?focus=<id>
//
// The per-approval form mirrors the URL printed by the terminal
// notifier when an action-approval lands, so the operator can paste
// the same `aileron open approval <id>` command into any shell on the
// host to surface the approval — there is no requirement that this
// run from the same terminal where the agent is paused.
//
// Resolution: reads <stateDir>/daemon.json to find the daemon URL.
// If the daemon isn't running, exits 1 with a hint (auto-spawning the
// daemon just to open the webapp would be surprising; the user
// running `aileron launch` already started one).
func runOpen(args []string, stdout, stderr io.Writer) int {
	target, approvalID, err := parseOpenArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "aileron: %v\n", err)
		fmt.Fprintln(stderr, "usage: aileron open [approvals | approval <id>]")
		return 1
	}

	stateDir, err := defaultStateDirFn()
	if err != nil {
		fmt.Fprintf(stderr, "aileron: %v\n", err)
		return 1
	}
	info, err := discoveryReadFn(stateDir)
	if err != nil {
		if errors.Is(err, discovery.ErrNotRunning) {
			fmt.Fprintln(stderr, "aileron: daemon is not running.")
			fmt.Fprintln(stderr, "Hint: any 'aileron <command>' or 'aileron launch <agent>' will start it.")
			return 1
		}
		fmt.Fprintf(stderr, "aileron: read daemon.json: %v\n", err)
		return 1
	}

	openURL := buildOpenURL(info.URL, target, approvalID)

	if err := openInBrowserFn(context.Background(), openURL); err != nil {
		// Print the URL on failure so the user can copy-paste even
		// when the OS opener isn't wired up (no DISPLAY on Linux, no
		// xdg-open installed, etc.).
		fmt.Fprintf(stderr, "aileron: failed to launch browser: %v\n", err)
		fmt.Fprintf(stderr, "Open this URL manually: %s\n", openURL)
		return 1
	}
	fmt.Fprintln(stdout, openURL)
	return 0
}

// openTarget enumerates the subcommands `aileron open` accepts. An
// empty target opens the webapp root.
type openTarget int

const (
	openTargetRoot openTarget = iota
	openTargetApprovals
	openTargetApproval
)

// parseOpenArgs validates the argv and returns the target + an
// optional approval ID. Returns an error for unknown subcommands and
// for `approval` without an ID.
func parseOpenArgs(args []string) (openTarget, string, error) {
	if len(args) == 0 {
		return openTargetRoot, "", nil
	}
	switch args[0] {
	case "approvals":
		if len(args) > 1 {
			return 0, "", fmt.Errorf("`open approvals` takes no arguments; did you mean `open approval %s`?", args[1])
		}
		return openTargetApprovals, "", nil
	case "approval":
		if len(args) < 2 || args[1] == "" {
			return 0, "", fmt.Errorf("`open approval` requires an approval ID")
		}
		if len(args) > 2 {
			return 0, "", fmt.Errorf("`open approval` takes one argument; got %d", len(args)-1)
		}
		return openTargetApproval, args[1], nil
	default:
		return 0, "", fmt.Errorf("unknown open target %q", args[0])
	}
}

// buildOpenURL composes the destination URL from the daemon's base URL
// and the parsed target. Trailing slashes on the base URL are
// tolerated and stripped so the result never has a doubled slash
// before the first path segment.
func buildOpenURL(base string, target openTarget, approvalID string) string {
	base = strings.TrimRight(base, "/")
	switch target {
	case openTargetApprovals:
		return base + "/approvals"
	case openTargetApproval:
		return base + "/approvals?focus=" + url.QueryEscape(approvalID)
	default:
		return base + "/"
	}
}

// openInBrowser shells out to the platform's URL-opener. The argv
// shape is single-string-URL on every supported platform; we never
// invoke a shell, so a URL containing meta characters can't inject.
//
// Platforms not in the switch (e.g. freebsd) return an error rather
// than silently no-oping — the operator at least learns why the
// browser didn't open, and the caller falls back to printing the URL.
func openInBrowser(ctx context.Context, target string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{target}
	case "linux":
		name, args = "xdg-open", []string{target}
	case "windows":
		// `start` is a cmd builtin, not a binary. Empty string is the
		// window title (a quirk of `start`'s argument grammar).
		name, args = "cmd", []string{"/c", "start", "", target}
	default:
		return fmt.Errorf("no browser opener registered for %s", runtime.GOOS)
	}
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Test seams. Tests replace these with fakes to avoid touching
// ~/.aileron, depending on a real daemon, or actually launching a
// browser process.
var (
	defaultStateDirFn = defaultStateDir
	openInBrowserFn   = openInBrowser
)
