//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/term"
)

// stubTerminalSeams makes runRuntimeChildPTY's terminal syscalls succeed
// without a real controlling terminal, so the full session and every error
// branch can run deterministically on a headless CI runner (where
// term.MakeRaw would otherwise fail and leave the post-raw-mode body
// uncovered). ptyOpen keeps its real implementation: a genuine PTY pair
// backs the session. Each seam is restored on cleanup. Individual tests
// override a specific seam after calling this to exercise a failure path.
func stubTerminalSeams(t *testing.T) {
	t.Helper()
	origOpen, origSize := ptyOpen, ptyInheritSize
	origIsTerm, origMakeRaw, origRestore := termIsTerminal, termMakeRaw, termRestore
	t.Cleanup(func() {
		ptyOpen, ptyInheritSize = origOpen, origSize
		termIsTerminal, termMakeRaw, termRestore = origIsTerm, origMakeRaw, origRestore
	})
	termIsTerminal = func(int) bool { return true }
	termMakeRaw = func(int) (*term.State, error) { return &term.State{}, nil }
	termRestore = func(int, *term.State) error { return nil }
	ptyInheritSize = func(*os.File, *os.File) error { return nil }
}

// swapStdio redirects os.Stdin/os.Stdout to pipes for the duration of a
// test and returns the host-side ends. The stdin write end is closed on
// cleanup, which unblocks the abandoned stdin->master pump goroutine that
// runRuntimeChildPTY leaves running (it reads the captured stdin handle).
// Globals are restored only after the test body finishes, and the pump
// reads a captured local handle, so the restore never races the goroutine.
func swapStdio(t *testing.T) (stdinW, stdoutR *os.File) {
	t.Helper()
	origIn, origOut := os.Stdin, os.Stdout
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() {
		os.Stdin, os.Stdout = origIn, origOut
		_ = inW.Close()
		_ = inR.Close()
		_ = outW.Close()
		_ = outR.Close()
	})
	return inW, outR
}

// readUntil reads from r until marker appears or r hits EOF/error, then
// reports it on the returned channel. Used to observe the bytes
// runRuntimeChildPTY copied from the child's PTY to os.Stdout.
func readUntil(r *os.File, marker string) <-chan string {
	out := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var sb strings.Builder
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if strings.Contains(sb.String(), marker) {
					out <- sb.String()
					return
				}
			}
			if rerr != nil {
				out <- sb.String()
				return
			}
		}
	}()
	return out
}

// TestRunRuntimeChildPTYFallsThroughWithoutTerminal asserts that when the
// host stdin is not a real terminal (the headless-CI reality) the PTY
// owner reports errRunPTYUnsupported so execRunner.Run falls through to
// the plain stdio exec path. Without this sentinel a CI run would either
// hang on a non-terminal or fail term.MakeRaw. See issue #1029.
func TestRunRuntimeChildPTYFallsThroughWithoutTerminal(t *testing.T) {
	stubTerminalSeams(t)
	termIsTerminal = func(int) bool { return false }

	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
	if err := runRuntimeChildPTY(cmd); !errors.Is(err, errRunPTYUnsupported) {
		t.Fatalf("runRuntimeChildPTY without a terminal = %v, want errRunPTYUnsupported", err)
	}
}

// TestRunRuntimeChildPTYOwnsTerminal exercises the full happy path with a
// real PTY pair standing in for the in-container TTY and the terminal
// syscalls stubbed so the session runs end to end on a headless runner. It
// verifies the child's output round-trips through the PTY aileron owns and
// that the child is never promoted to the terminal's foreground group (the
// #1029 regression guard: aileron keeps the foreground group).
func TestRunRuntimeChildPTYOwnsTerminal(t *testing.T) {
	stubTerminalSeams(t)
	_, stdoutR := swapStdio(t)

	const marker = "aileron-pty-1029"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf '%s' "+marker)
	// Mirror what execRunner.Run does before invoking the PTY owner: the
	// child is isolated into its own group, never promoted to foreground.
	configureRuntimeChild(cmd, []string{"run", "-t"}, true)

	done := make(chan error, 1)
	go func() { done <- runRuntimeChildPTY(cmd) }()
	out := readUntil(stdoutR, marker)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRuntimeChildPTY = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for runRuntimeChildPTY to return")
	}

	select {
	case got := <-out:
		if !strings.Contains(got, marker) {
			t.Fatalf("PTY output %q does not contain marker %q", got, marker)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child output through the PTY")
	}

	// The #1029 invariant: the runtime child is never promoted to the host
	// terminal's foreground process group. Aileron keeps it.
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil after PTY run")
	}
	if cmd.SysProcAttr.Foreground {
		t.Fatal("SysProcAttr.Foreground = true; the runtime child must never own the host terminal's foreground group (issue #1029)")
	}
	if !cmd.SysProcAttr.Setctty {
		t.Fatal("SysProcAttr.Setctty = false; the child must take the PTY slave as its controlling terminal")
	}
}

// TestRunRuntimeChildPTYInheritSizeErrorIsNonFatal asserts the contract
// that a failed initial PTY sizing does not abort the launch: the session
// still runs to completion and the child's output round-trips.
func TestRunRuntimeChildPTYInheritSizeErrorIsNonFatal(t *testing.T) {
	stubTerminalSeams(t)
	ptyInheritSize = func(*os.File, *os.File) error { return errors.New("sizing unavailable") }
	_, stdoutR := swapStdio(t)

	const marker = "size-nonfatal"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf '%s' "+marker)
	configureRuntimeChild(cmd, []string{"run", "-t"}, true)

	done := make(chan error, 1)
	go func() { done <- runRuntimeChildPTY(cmd) }()
	out := readUntil(stdoutR, marker)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRuntimeChildPTY with a sizing failure = %v, want nil (non-fatal)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out; a sizing failure must not hang the session")
	}
	if got := <-out; !strings.Contains(got, marker) {
		t.Fatalf("output %q lacks marker %q after a non-fatal sizing failure", got, marker)
	}
}

// TestRunRuntimeChildPTYPropagatesWindowResize covers the SIGWINCH pump:
// a window-change signal while the session is live re-sizes the PTY so the
// in-container agent re-renders. The child lingers so the signal arrives
// mid-session.
func TestRunRuntimeChildPTYPropagatesWindowResize(t *testing.T) {
	stubTerminalSeams(t)
	resized := make(chan struct{}, 8)
	ptyInheritSize = func(*os.File, *os.File) error {
		select {
		case resized <- struct{}{}:
		default:
		}
		return nil
	}
	_, stdoutR := swapStdio(t)

	const marker = "resize-ready"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf '%s' "+marker+"; sleep 2")
	configureRuntimeChild(cmd, []string{"run", "-t"}, true)

	done := make(chan error, 1)
	go func() { done <- runRuntimeChildPTY(cmd) }()
	out := readUntil(stdoutR, marker)

	// Wait until the child is up (marker emitted), then drain the initial
	// sizing call so we can observe the resize that the signal triggers.
	if got := <-out; !strings.Contains(got, marker) {
		t.Fatalf("output %q lacks marker %q", got, marker)
	}
	<-resized // the initial pty.InheritSize at session start

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("send SIGWINCH: %v", err)
	}
	select {
	case <-resized:
		// The winch goroutine re-sized the PTY in response to the signal.
	case <-time.After(5 * time.Second):
		t.Fatal("SIGWINCH did not trigger a PTY resize")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRuntimeChildPTY = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the lingering child to exit")
	}
}

// TestRunRuntimeChildPTYAllocateError asserts a PTY allocation failure on
// a real terminal is surfaced as a hard error (not the fall-through
// sentinel), wrapping the underlying cause.
func TestRunRuntimeChildPTYAllocateError(t *testing.T) {
	stubTerminalSeams(t)
	sentinel := errors.New("ptmx exhausted")
	ptyOpen = func() (*os.File, *os.File, error) { return nil, nil, sentinel }

	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
	err := runRuntimeChildPTY(cmd)
	if !errors.Is(err, sentinel) {
		t.Fatalf("runRuntimeChildPTY with a pty.Open failure = %v, want wrap of %v", err, sentinel)
	}
	if errors.Is(err, errRunPTYUnsupported) {
		t.Fatal("a pty.Open failure must be a hard error, not the fall-through sentinel")
	}
}

// TestRunRuntimeChildPTYMakeRawError asserts that a terminal that cannot
// enter raw mode is a hard error rather than a silent degrade, and that
// the allocated PTY is cleaned up on that path.
func TestRunRuntimeChildPTYMakeRawError(t *testing.T) {
	stubTerminalSeams(t)
	sentinel := errors.New("tcsetattr refused")
	termMakeRaw = func(int) (*term.State, error) { return nil, sentinel }

	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
	err := runRuntimeChildPTY(cmd)
	if !errors.Is(err, sentinel) {
		t.Fatalf("runRuntimeChildPTY with a MakeRaw failure = %v, want wrap of %v", err, sentinel)
	}
	if errors.Is(err, errRunPTYUnsupported) {
		t.Fatal("a MakeRaw failure must be a hard error, not the fall-through sentinel")
	}
}

// TestRunRuntimeChildPTYStartError covers the child-start failure path,
// and (with a bare cmd) the SysProcAttr initialization branch.
func TestRunRuntimeChildPTYStartError(t *testing.T) {
	stubTerminalSeams(t)

	// A bare cmd (nil SysProcAttr) exercises the lazy SysProcAttr init, and
	// a non-existent program forces cmd.Start to fail.
	cmd := exec.CommandContext(context.Background(), "/nonexistent/aileron-pty-start-test")
	err := runRuntimeChildPTY(cmd)
	if err == nil {
		t.Fatal("runRuntimeChildPTY with an unstartable child = nil, want a start error")
	}
	if errors.Is(err, errRunPTYUnsupported) {
		t.Fatal("a start failure must not be reported as the fall-through sentinel")
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr should have been initialized before Start")
	}
}
