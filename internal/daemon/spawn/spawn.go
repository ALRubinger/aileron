// Package spawn is the client-side helper for finding (or starting)
// the Aileron daemon. CLI commands and `aileron launch` call
// [Resolve] before they make any HTTP request; the helper either
// returns an already-running daemon's URL or fork-execs a fresh
// daemon and waits for its discovery file to appear.
//
// Concurrency invariant: multiple clients racing on first run all
// observe a single spawn. The spawn-coordination uses
// [discovery.Lock] held briefly across the SpawnFn call; the daemon's
// own startup lock independently guards against duplicate daemons,
// so even if two clients somehow trip the spawn at once only one
// daemon survives.
//
// See ADR-0012.
package spawn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
)

// Options configures Resolve.
type Options struct {
	// StateDir holds discovery files (daemon.json, daemon.lock).
	// Required.
	StateDir string

	// SpawnFn brings a daemon up. Defaults to fork-execing Binary
	// (with BinaryArgs) detached from the parent. Tests substitute
	// a function that synthetically writes daemon.json so the helper
	// can be exercised without an OS process.
	//
	// The optional `exited` channel emits the spawned child's exit
	// error (nil on clean exit) when the child process exits.
	// waitForDaemon uses it to fail fast with the child's exit
	// status plus a tail of <stateDir>/daemon.log when the daemon
	// dies before publishing daemon.json — instead of timing out at
	// SpawnTimeout with a generic message. SpawnFns that synthesize
	// a daemon (no real child process) can return a nil channel.
	SpawnFn func(ctx context.Context, stateDir string) (exited <-chan error, err error)

	// Binary is the daemon binary path used by the default SpawnFn.
	// Ignored when SpawnFn is set.
	Binary string

	// BinaryArgs are forwarded to Binary by the default SpawnFn.
	BinaryArgs []string

	// SpawnTimeout caps how long Resolve waits for daemon.json after
	// triggering a spawn. Default 5s.
	SpawnTimeout time.Duration

	// PollInterval is how often Resolve polls daemon.json after
	// spawning. Default 50ms.
	PollInterval time.Duration

	// LivenessTimeout caps the TCP-connect liveness probe per
	// daemon.json read. Default 200ms.
	LivenessTimeout time.Duration

	// EnvLookup overrides os.Getenv. Tests inject this to drive the
	// AILERON_API_URL fast path in isolation.
	EnvLookup func(string) string
}

// ErrNoBinary is returned when SpawnFn is unset and Binary is empty —
// the helper has nothing to fork-exec.
var ErrNoBinary = errors.New("daemon binary path is empty (set Options.Binary)")

// Resolve returns the URL of the running Aileron daemon, spawning
// one if none is reachable. The fast path is one daemon.json read
// plus a sub-millisecond TCP probe; the spawn path acquires a
// singleton lock, runs SpawnFn, and polls daemon.json for the
// configured SpawnTimeout.
func Resolve(ctx context.Context, opts Options) (string, error) {
	if opts.StateDir == "" {
		return "", errors.New("spawn: StateDir is required")
	}
	if opts.EnvLookup == nil {
		opts.EnvLookup = os.Getenv
	}
	if v := opts.EnvLookup("AILERON_API_URL"); v != "" {
		return v, nil
	}

	if opts.SpawnTimeout <= 0 {
		opts.SpawnTimeout = 5 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 50 * time.Millisecond
	}
	if opts.LivenessTimeout <= 0 {
		opts.LivenessTimeout = 200 * time.Millisecond
	}
	if opts.SpawnFn == nil {
		opts.SpawnFn = forkExec(opts.Binary, opts.BinaryArgs)
	}

	// Fast path: daemon already running and reachable.
	if url, ok := readAlive(opts.StateDir, opts.LivenessTimeout); ok {
		return url, nil
	}

	// Singleton spawn lock — at most one client triggers SpawnFn.
	lockCtx, cancelLock := context.WithTimeout(ctx, 2*time.Second)
	release, err := discovery.Lock(lockCtx, opts.StateDir)
	cancelLock()
	if err != nil {
		// Lock-acquire timed out. The most common cause is that a
		// surviving daemon already grabbed the lock and is holding
		// it for its lifetime (`internal/server.run`'s `defer
		// releaseLock`), so no client can ever acquire it again.
		// In that case the daemon is up — retry readAlive once
		// before surfacing the error. (#528 finding 3a)
		if errors.Is(err, context.DeadlineExceeded) {
			if url, ok := readAlive(opts.StateDir, opts.LivenessTimeout); ok {
				return url, nil
			}
		}
		return "", fmt.Errorf("spawn: acquire lock: %w", err)
	}

	// Re-check after lock — another client may have spawned during
	// our lock-acquire window.
	if url, ok := readAlive(opts.StateDir, opts.LivenessTimeout); ok {
		_ = release()
		return url, nil
	}

	exited, err := opts.SpawnFn(ctx, opts.StateDir)
	if err != nil {
		_ = release()
		return "", fmt.Errorf("spawn: %w", err)
	}

	// Release before waitForDaemon. The daemon's own startup-lock
	// acquisition (`internal/server.run`) takes over as the singleton
	// check; holding the lock through the polling loop would deadlock
	// against it — the daemon would time out, exit, and never write
	// daemon.json, leaving the helper to time out at SpawnTimeout.
	//
	// Concurrent clients that race past us during waitForDaemon either
	// observe the daemon's daemon.json once it lands (fast-path return)
	// or fork a redundant daemon — the daemon's own lock then
	// guarantees only one survives, so the worst case is a few wasted
	// process starts, not duplicated daemons.
	_ = release()

	return waitForDaemon(ctx, opts, exited)
}

// waitForDaemon polls daemon.json until the daemon is reachable, the
// SpawnTimeout deadline expires, or the spawned child exits early
// (signaled via the optional `exited` channel from SpawnFn).
//
// On early exit and on timeout, the error includes a tail of
// <stateDir>/daemon.log so the operator can see the underlying cause
// (missing vault, port-in-use, etc.) instead of a generic message.
func waitForDaemon(ctx context.Context, opts Options, exited <-chan error) (string, error) {
	deadline := time.Now().Add(opts.SpawnTimeout)
	for {
		if url, ok := readAlive(opts.StateDir, opts.LivenessTimeout); ok {
			return url, nil
		}
		if time.Now().After(deadline) {
			return "", spawnTimeoutError(opts.StateDir, opts.SpawnTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case waitErr := <-exited:
			// Re-check daemon.json once: the child may have exited
			// after publishing its URL (rare, but the fast-path read
			// at the top of the loop runs before this select). This
			// avoids spurious "exited before becoming ready" when in
			// fact the daemon ran, served, and exited cleanly.
			if url, ok := readAlive(opts.StateDir, opts.LivenessTimeout); ok {
				return url, nil
			}
			return "", spawnEarlyExitError(opts.StateDir, waitErr)
		case <-time.After(opts.PollInterval):
		}
	}
}

// spawnTimeoutError wraps the SpawnTimeout case with a tail of the
// daemon log when one is present. The daemon may still be running
// — it just didn't publish daemon.json in time — so the log tail is
// best-effort context, not a guaranteed cause.
func spawnTimeoutError(stateDir string, timeout time.Duration) error {
	tail := readLogTail(filepath.Join(stateDir, discovery.LogFile), logTailLines)
	if tail == "" {
		return fmt.Errorf("spawn: daemon did not become ready within %s", timeout)
	}
	return fmt.Errorf("spawn: daemon did not become ready within %s; recent daemon log:\n%s", timeout, tail)
}

// spawnEarlyExitError wraps the case where the spawned daemon child
// exited before publishing daemon.json. The wait error carries the
// child's exit code; the log tail typically carries the reason
// (e.g. "no vault file at ...; run `aileron vault init` first").
func spawnEarlyExitError(stateDir string, waitErr error) error {
	tail := readLogTail(filepath.Join(stateDir, discovery.LogFile), logTailLines)
	switch {
	case waitErr != nil && tail != "":
		return fmt.Errorf("spawn: daemon exited before becoming ready: %w; recent daemon log:\n%s", waitErr, tail)
	case waitErr != nil:
		return fmt.Errorf("spawn: daemon exited before becoming ready: %w", waitErr)
	case tail != "":
		return fmt.Errorf("spawn: daemon exited cleanly without becoming ready; recent daemon log:\n%s", tail)
	default:
		return errors.New("spawn: daemon exited cleanly without becoming ready")
	}
}

// logTailLines is the number of trailing lines included in spawn
// error messages. Tuned to capture the typical "context + cause"
// pair without producing wall-of-text errors.
const logTailLines = 20

// logTailMaxBytes caps how much of the log file is read for tailing.
// Daemon logs are JSON-per-line; 16 KiB is well over a few dozen
// recent lines without risking large allocations on accidentally-huge
// log files.
const logTailMaxBytes = 16 * 1024

// readLogTail returns the last n non-empty lines of the file at path,
// or "" if the file cannot be read. Best-effort: errors are swallowed
// because this is diagnostic context only — failing to read the log
// must not mask the original spawn error.
func readLogTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	readSize := int64(logTailMaxBytes)
	if size < readSize {
		readSize = size
	}
	if _, err := f.Seek(size-readSize, io.SeekStart); err != nil {
		return ""
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := bytes.Split(buf, []byte("\n"))
	// Drop empty trailing line from a final newline.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	// Drop the (possibly partial) first line if we seeked into the
	// middle of the file.
	if size > readSize && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}

// readAlive reads daemon.json and TCP-probes the URL. Returns the
// URL only when both succeed.
func readAlive(stateDir string, livenessTimeout time.Duration) (string, bool) {
	info, err := discovery.Read(stateDir)
	if err != nil {
		return "", false
	}
	addr, err := urlToHostPort(info.URL)
	if err != nil {
		return "", false
	}
	conn, err := net.DialTimeout("tcp", addr, livenessTimeout)
	if err != nil {
		return "", false
	}
	_ = conn.Close()
	return info.URL, true
}

// urlToHostPort extracts host:port from `http://host:port` without
// pulling in net/url. The daemon URL shape is fixed by ADR-0012.
func urlToHostPort(u string) (string, error) {
	const prefix = "http://"
	if !strings.HasPrefix(u, prefix) {
		return "", fmt.Errorf("expected http:// URL, got %q", u)
	}
	return u[len(prefix):], nil
}

// forkExec returns a SpawnFn that fork-execs binary with args. The
// daemon's stdio is detached and redirected to <stateDir>/daemon.log
// so it survives the parent's exit and produces a debuggable trail.
//
// Setsid puts the daemon in its own session so SIGHUP from the
// parent's terminal does not propagate. The daemon's lifetime is
// independent of the parent — the returned channel is sent on (with
// the wait error or nil) when the child exits, and waitForDaemon
// uses that signal to fail fast when the daemon dies before
// publishing daemon.json.
func forkExec(binary string, args []string) func(context.Context, string) (<-chan error, error) {
	return func(_ context.Context, stateDir string) (<-chan error, error) {
		if binary == "" {
			return nil, ErrNoBinary
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("create state dir: %w", err)
		}
		cmd := exec.Command(binary, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stdin = nil

		logPath := filepath.Join(stateDir, discovery.LogFile)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open daemon log: %w", err)
		}
		cmd.Stdout = f
		cmd.Stderr = f

		if err := cmd.Start(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("start daemon: %w", err)
		}
		// Close our copy of the log FD; the child has its own.
		_ = f.Close()

		// Reap the child in a goroutine so waitForDaemon can detect
		// early exit. The buffered slot guarantees the goroutine
		// never blocks even if waitForDaemon has already returned
		// (fast-path success, ctx cancel, etc.); the goroutine then
		// dies with the parent process.
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		return exited, nil
	}
}
