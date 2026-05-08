package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/launch"
)

// sessionsWatchTailInterval is how often the watch loop polls the
// session log file for new bytes when following. 200ms is the same
// order of magnitude as `tail -F` and feels live for an interactive
// command without spinning the CPU.
//
// Replaceable in tests so they don't pay real-time waits.
var sessionsWatchTailInterval = 200 * time.Millisecond

// sessionsWatchSignals is the set of signals that gracefully end the
// watch loop. Replaceable in tests where signal.Notify on SIGINT is
// hostile (it interferes with `go test`'s own signal handling).
var sessionsWatchSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// runSessionsWatch follows the per-session log written by `aileron
// launch` for sessionID, printing existing content first, then
// streaming new bytes as they appear. Resolves session → working_dir
// via GET /v1/sessions/{id} so the working_dir lookup matches what
// the launcher recorded; computes the on-disk path via
// [launch.SessionLogPath] so the resolution rules (policy-file walk,
// fallback to .aileron/session.log under dir) stay in lockstep.
//
// Exits when the user sends SIGINT/SIGTERM, or — when --no-follow is
// set — after printing the file's existing content. An ended session
// is followed identically to a running one (no special-case): if the
// file is static the loop just sleeps. The user picks the right
// stopping point with Ctrl-C.
//
// Resolves finding #7c of #532 — `kubectl logs -f`-style visibility
// into a launch session.
func runSessionsWatch(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sessions watch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	noFollow := flags.Bool("no-follow", false, "Print the existing content and exit; do not stream new lines")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: aileron sessions watch <session-id> [--no-follow]")
		return 1
	}
	sessionID := rest[0]

	s, status, err := sessionsGetFetcher(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(stderr, "session %q not found\n", sessionID)
		return 1
	}

	logPath := launch.SessionLogPath(s.WorkingDir)
	if logPath == "" {
		fmt.Fprintln(stderr, "error: could not resolve session log path (no working_dir on session record)")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), sessionsWatchSignals...)
	defer stop()

	if err := tailSessionLog(ctx, stdout, logPath, *noFollow); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// tailSessionLog opens path, copies existing content to w, and (when
// follow is true) keeps appending new bytes as they're written by the
// launcher. Returns nil on graceful end (ctx cancel for follow=true,
// EOF for follow=false). Returns an error only on unrecoverable I/O
// failure — a missing file at start is treated as "not yet" and
// polled for, since the watch command may legitimately race the
// launcher creating session.log on first write.
//
// Split out from runSessionsWatch so the loop is testable without a
// running daemon.
func tailSessionLog(ctx context.Context, w io.Writer, path string, noFollow bool) error {
	f, err := openSessionLogPolling(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Seek to start; the contract is "print existing content first."
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek session log: %w", err)
	}

	// Copy whatever is in the file right now.
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("read session log: %w", err)
	}

	if noFollow {
		return nil
	}

	// Follow loop: poll for new bytes appended after our current
	// offset. ENOSYS-clean implementation — works on every platform
	// where os.File supports Seek + Read at an offset.
	buf := make([]byte, 4096)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if n > 0 {
			// Got data — try to drain immediately rather than sleep.
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sessionsWatchTailInterval):
		}
	}
}

// openSessionLogPolling opens path for reading, polling at
// sessionsWatchTailInterval if the file does not yet exist. Returns
// when the file appears or ctx is cancelled. The poll handles the
// race where the user runs `aileron sessions watch <id>` immediately
// after `aileron launch <agent>` and beats the launcher to writing
// session.log.
func openSessionLogPolling(ctx context.Context, path string) (*os.File, error) {
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("open session log %q: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sessionsWatchTailInterval):
		}
	}
}

