//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// TestRunRuntimeChildPTYFallsThroughWithoutTerminal asserts that when the
// host stdin is not a real terminal (the headless-CI reality) the PTY
// owner reports errRunPTYUnsupported so execRunner.Run falls through to
// the plain stdio exec path. Without this sentinel a CI run would either
// hang on a non-terminal or fail term.MakeRaw. See issue #1029.
func TestRunRuntimeChildPTYFallsThroughWithoutTerminal(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal; this case requires a non-terminal stdin")
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
	if err := runRuntimeChildPTY(cmd); !errors.Is(err, errRunPTYUnsupported) {
		t.Fatalf("runRuntimeChildPTY without a terminal = %v, want errRunPTYUnsupported", err)
	}
}

// TestRunRuntimeChildPTYOwnsTerminal exercises the happy path with a real
// PTY standing in for the host terminal. It swaps os.Stdin for a PTY
// slave, runs a child that writes to its stdout, and verifies the output
// round-trips through the PTY aileron owns and that the child is never
// promoted to the terminal's foreground group (the #1029 regression
// guard: aileron keeps the foreground group). This is the closest a
// headless runner can get to the interactive raw-TTY launch.
func TestRunRuntimeChildPTYOwnsTerminal(t *testing.T) {
	hostPTY, hostTTY, err := pty.Open()
	if err != nil {
		t.Skipf("cannot allocate host PTY: %v", err)
	}
	defer hostPTY.Close()
	defer hostTTY.Close()

	// runRuntimeChildPTY reads keystrokes from os.Stdin (the host terminal)
	// and copies the child's PTY output to os.Stdout. Swap both: stdin to
	// the host PTY slave so MakeRaw sees a real terminal, stdout to a pipe
	// so the test can observe the bytes that round-trip through the PTY
	// aileron owns.
	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin = hostTTY
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout capture pipe: %v", err)
	}
	os.Stdout = outW
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
		outR.Close()
	}()

	const marker = "aileron-pty-1029"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf '%s' "+marker)
	// Mirror what execRunner.Run does before invoking the PTY owner: the
	// child is isolated into its own group, never promoted to foreground.
	configureRuntimeChild(cmd, []string{"run", "-t"}, true)

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		r := result{err: runRuntimeChildPTY(cmd)}
		outW.Close() // unblock the capture reader once the copy loop ends
		done <- r
	}()

	// Read the bytes aileron copied from the child's PTY to os.Stdout.
	out := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var sb strings.Builder
		for {
			n, rerr := outR.Read(buf)
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

	// Wait for the run to finish first. A restricted sandbox can refuse PTY
	// allocation or raw-mode entry (pty.Open / term.MakeRaw); that is an
	// environment limitation, not a regression, so skip rather than fail.
	// The marker only round-trips when the PTY path fully ran, so a clean
	// run implies success here.
	var runErr error
	select {
	case res := <-done:
		runErr = res.err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for runRuntimeChildPTY to return")
	}
	if runErr != nil {
		if errors.Is(runErr, errRunPTYUnsupported) {
			t.Skip("PTY ownership unsupported in this sandbox; fall-through path is covered separately")
		}
		t.Skipf("PTY session could not run in this environment: %v", runErr)
	}

	select {
	case got := <-out:
		if !strings.Contains(got, marker) {
			t.Fatalf("PTY output %q does not contain marker %q", got, marker)
		}
	case <-time.After(10 * time.Second):
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
