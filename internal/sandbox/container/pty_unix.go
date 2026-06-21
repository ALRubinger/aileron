//go:build !windows

package container

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// errRunPTYUnsupported signals that the caller should fall through to the
// plain stdio exec path because there is no host PTY for aileron to own
// (Windows, or a host stdout/stdin that is not a real terminal). It is a
// sentinel, never a launch failure.
var errRunPTYUnsupported = errors.New("container: no host PTY to own")

// Seams for the PTY/terminal syscalls runRuntimeChildPTY depends on. They
// are package-level vars (not direct calls) so unit tests can drive the
// full session and each failure path deterministically on a headless CI
// runner that has no real controlling terminal — where term.MakeRaw would
// otherwise fail and leave the post-raw-mode body uncovered. Production
// behavior is unchanged: each var holds the real function.
var (
	ptyOpen        = pty.Open
	ptyInheritSize = pty.InheritSize
	termIsTerminal = term.IsTerminal
	termMakeRaw    = term.MakeRaw
	termRestore    = term.Restore
)

// runRuntimeChildPTY runs the runtime child (`docker run -t` / `exec -t`)
// with aileron owning the PTY (ADR-0025, issue #1029).
//
// Before #1029 the child was promoted to the host terminal's foreground
// process group (SysProcAttr.Foreground + Ctty=0) so its raw-mode
// tcsetattr would target the real terminal. That handed the in-container
// agent the foreground group, so a terminal Ctrl-C went to the agent
// instead of aileron — defeating aileron's SIGINT/SIGTERM teardown owner
// role and the #802 approval substrate.
//
// Instead aileron now:
//   - allocates a pseudo-terminal pair;
//   - wires the child's stdin/stdout/stderr to the PTY slave and makes
//     that slave the child's controlling terminal (Setsid + Setctty), so
//     the child's tcsetattr targets a TTY it owns rather than the host
//     terminal;
//   - puts the host terminal into raw mode and copies bytes both ways
//     between the host terminal and the PTY master;
//   - sizes the PTY to the host terminal and propagates SIGWINCH so
//     resizes reach the in-container agent;
//   - restores the host terminal on exit.
//
// Aileron stays in the host terminal's foreground process group the whole
// time, so a Ctrl-C reaches aileron and its salvage handler, not the
// container. The child keeps the own-process-group isolation that
// configureRuntimeChild applied; only the Foreground/Ctty handoff is
// dropped.
//
// cmd.Stdout/Stderr set by the caller are overwritten here: an interactive
// raw-TTY session multiplexes stdout and stderr onto the single PTY, which
// is the behavior a real terminal already gives an in-container agent.
func runRuntimeChildPTY(cmd *exec.Cmd) (err error) {
	// Capture the stdio handles once. The byte-pump goroutine started below
	// outlives this function (it blocks on a host read that has no clean
	// unblock), so it must read a local handle rather than the os.Stdin /
	// os.Stdout globals: a caller (or test) that swaps and restores those
	// globals would otherwise race the abandoned goroutine. Pinning to one
	// pair of handles also keeps the whole session consistent.
	stdin, stdout := os.Stdin, os.Stdout
	hostIn := int(stdin.Fd())
	if !termIsTerminal(hostIn) {
		return errRunPTYUnsupported
	}

	// errRunPTYUnsupported above (the not-a-terminal case) is the only
	// fall-through-to-stdio path. A genuine pty.Open failure on a real
	// terminal (fd exhaustion, /dev/pts unavailable) is a hard error: the
	// caller asked for an interactive raw-TTY session aileron cannot
	// provide, so surface it rather than silently degrading.
	ptmx, tty, err := ptyOpen()
	if err != nil {
		return fmt.Errorf("allocate sandbox PTY: %w", err)
	}
	defer func() {
		// Backstop for the pre-Start error paths (pty sizing / raw-mode entry
		// failures) that return before the explicit hand-off close below. On
		// the success path the slave is already closed, so this is a harmless
		// no-op double close whose ErrClosed is intentionally ignored.
		_ = tty.Close()
		if cerr := ptmx.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	// Match the PTY to the host terminal's current size, then keep them in
	// sync on SIGWINCH so the in-container agent re-renders on resize.
	if err := ptyInheritSize(stdin, ptmx); err != nil {
		// A failed initial sizing is non-fatal: the session still runs at
		// the PTY default size. Fall through rather than aborting launch.
		_ = err
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			_ = ptyInheritSize(stdin, ptmx)
		}
	}()

	// Put the host terminal in raw mode so keystrokes reach the
	// in-container agent unmodified; restore on exit. Aileron remains the
	// foreground process, so the SIGINT/SIGTERM handler installed by the
	// launcher still owns teardown.
	oldState, err := termMakeRaw(hostIn)
	if err != nil {
		// A real terminal that cannot enter raw mode is a hard error, not a
		// fall-through: returning the plain-stdio sentinel would silently
		// leave the host terminal cooked while the child drives a raw PTY.
		return fmt.Errorf("set host terminal raw mode: %w", err)
	}
	defer func() {
		if rerr := termRestore(hostIn, oldState); rerr != nil && err == nil {
			err = rerr
		}
	}()

	// Hand the child the slave side as its stdio and controlling terminal.
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Keep the own-process-group isolation configureRuntimeChild applied,
	// but make the PTY slave the child's controlling terminal rather than
	// promoting the child to the host terminal's foreground group. Setsid
	// starts a new session (required so the slave can become its ctty);
	// Setctty + Ctty pointed at the child's stdin (fd 0 = the slave)
	// installs it. Foreground is intentionally NOT set: aileron keeps the
	// host terminal's foreground group. See issue #1029.
	cmd.SysProcAttr.Setpgid = false
	cmd.SysProcAttr.Foreground = false
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = 0

	if err := cmd.Start(); err != nil {
		return err
	}
	// The child has inherited the slave fds, so close the parent's copy now.
	// If the parent held the slave open until this function returned, the
	// master would never see EOF after the child exits and the master->stdout
	// copy below would block forever (a hang observed on Linux). Closing here
	// is the standard PTY hand-off: the master closes cleanly once the child
	// is the only remaining slave owner and then exits.
	_ = tty.Close()

	// Pump host terminal <-> PTY master. The stdin->master copy runs in a
	// goroutine because it blocks on host reads with no clean unblock; it
	// is abandoned when the child exits (the process is short-lived and
	// teardown owns process lifetime). The master->stdout copy returns
	// when the child closes the slave, which is our cue that the session
	// ended.
	go func() { _, _ = io.Copy(ptmx, stdin) }()
	_, _ = io.Copy(stdout, ptmx)

	return cmd.Wait()
}
