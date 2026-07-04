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

// buildAuditDashboardURL composes the URL for the `/audit` provenance
// walk-back view from the daemon's base URL and an optional content
// hash. When a hash is supplied it is carried in the `content_hash`
// query parameter, matching the deep-link contract the webapp's /audit
// route honors (webapp/src/routes/audit/+page.svelte). Trailing slashes
// on the base URL are stripped so the result never has a doubled slash.
//
// It is a sibling of buildOpenURL rather than a new openTarget: the
// audit dashboard is a distinct surface with its own query grammar, and
// a separate helper keeps each independently unit-testable.
func buildAuditDashboardURL(base, contentHash string) string {
	base = strings.TrimRight(base, "/")
	if contentHash == "" {
		return base + "/audit"
	}
	return base + "/audit?content_hash=" + url.QueryEscape(contentHash)
}

// openInBrowser shells out to the platform's URL-opener. macOS and
// Linux pass the URL as a single argv element with no shell. Windows
// goes through `cmd /c start`, whose command-line parser treats `&` and
// other metacharacters specially, so the URL is caret-escaped first
// (see escapeURLForWindowsCmd) to keep every query parameter intact.
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
		//
		// cmd.exe parses the command line itself and treats `&` as a
		// command separator, so a URL with multiple query parameters
		// would be truncated at the first `&`. Caret-escape the cmd
		// metacharacters so the full URL reaches `start`. Today these
		// URLs carry at most one query parameter, but the defect is the
		// same one fixed in internal/oauth's browser opener.
		name, args = "cmd", []string{"/c", "start", "", escapeURLForWindowsCmd(target)}
	default:
		return fmt.Errorf("no browser opener registered for %s", runtime.GOOS)
	}
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// escapeURLForWindowsCmd caret-escapes the cmd.exe metacharacters in s
// so a URL handed to `cmd /c start "" <url>` reaches the browser intact
// rather than being truncated or mangled by cmd's command-line parser.
//
// The load-bearing case is `&` (cmd's command separator): without
// escaping it, every query parameter after the first is dropped. The
// full set of cmd metacharacters is escaped for safety — `^` itself
// (escaped first so the carets we add aren't re-escaped), `&`, `|`,
// `<`, `>`, `(`, `)`, and `%` (which `start` would otherwise treat as
// the start of a `%VAR%` expansion). Only Windows routes through cmd;
// the macOS/Linux openers pass the URL as a single argv element.
func escapeURLForWindowsCmd(s string) string {
	replacer := strings.NewReplacer(
		"^", "^^",
		"&", "^&",
		"|", "^|",
		"<", "^<",
		">", "^>",
		"(", "^(",
		")", "^)",
		"%", "^%",
	)
	return replacer.Replace(s)
}

// Test seams. Tests replace these with fakes to avoid touching
// ~/.aileron, depending on a real daemon, or actually launching a
// browser process.
var (
	defaultStateDirFn = defaultStateDir
	openInBrowserFn   = openInBrowser
)
