//go:build !windows

package container

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
)

// TestSetRuntimeChildPgidSetsPgid asserts the Unix helper places the
// runtime child in its own process group so a terminal SIGINT does not
// reach docker directly (issue #999, ADR-0025).
func TestSetRuntimeChildPgidSetsPgid(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
	setRuntimeChildPgid(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil; Setpgid was not configured")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("SysProcAttr.Setpgid = false, want true")
	}
}

// TestSetRuntimeChildPgidPreservesExistingFields verifies the helper
// only forces Setpgid on and leaves any pre-set SysProcAttr fields
// intact.
func TestSetRuntimeChildPgidPreservesExistingFields(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
	cmd.SysProcAttr = &syscall.SysProcAttr{Foreground: false, Pgid: 0}
	existing := cmd.SysProcAttr
	setRuntimeChildPgid(cmd)
	if cmd.SysProcAttr != existing {
		t.Fatal("helper replaced the existing SysProcAttr rather than mutating it")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("SysProcAttr.Setpgid = false, want true")
	}
}

// TestExecRunnerRunWithPgidPropagatesExit confirms that setting the
// child process group does not break normal execution: a fast no-op
// command still succeeds, and a failing command still propagates a
// non-nil error. A real terminal-Ctrl-C-to-process-group assertion is
// not feasible in a headless test runner (no controlling TTY), which is
// why the isolation correctness is covered by the Setpgid unit checks
// above plus the salvage tests in internal/launch.
func TestExecRunnerRunWithPgidPropagatesExit(t *testing.T) {
	if err := (execRunner{}).Run(context.Background(), "/bin/sh", []string{"-c", ":"}, nil, nil); err != nil {
		t.Fatalf("no-op command returned %v, want nil", err)
	}
	if err := (execRunner{}).Run(context.Background(), "/bin/sh", []string{"-c", "exit 3"}, nil, nil); err == nil {
		t.Fatal("expected non-nil error from a failing command")
	}
}

// TestConfigureRuntimeChildNeverPromotesForeground pins the #1029
// contract: configureRuntimeChild isolates the runtime child into its own
// process group but never promotes it to the host terminal's foreground
// group. Aileron keeps the foreground group so it owns SIGINT/SIGTERM
// teardown and is the #802 approval-TUI substrate; the in-container agent
// gets its raw TTY from a PTY slave aileron hands it (see pty_unix.go),
// not from the host terminal's foreground group. This is the regression
// guard against re-introducing the #1028 Foreground+Ctty handoff that sent
// Ctrl-C to the agent instead of aileron.
func TestConfigureRuntimeChildNeverPromotesForeground(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		stdinTTY bool
	}{
		{"interactive run on terminal", []string{"run", "--rm", "-i", "-t", "img", "claude"}, true},
		{"interactive run without terminal", []string{"run", "--rm", "-i", "-t", "img", "claude"}, false},
		{"non-tty run on terminal", []string{"run", "--rm", "-i", "img", "claude"}, true},
		{"interactive exec on terminal", []string{"exec", "-i", "-t", "c", "gh", "auth", "login"}, true},
		{"non-tty exec on terminal", []string{"exec", "-i", "c", "gh", "auth", "token"}, true},
		{"build on terminal", []string{"build", "-t", "img", "."}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
			configureRuntimeChild(cmd, tc.args, tc.stdinTTY)
			if cmd.SysProcAttr == nil {
				t.Fatal("SysProcAttr is nil; Setpgid isolation was not configured")
			}
			if !cmd.SysProcAttr.Setpgid {
				t.Fatal("SysProcAttr.Setpgid = false; isolation must always apply")
			}
			if cmd.SysProcAttr.Foreground {
				t.Fatal("SysProcAttr.Foreground = true; the runtime child must never own the host terminal's foreground group (issue #1029)")
			}
		})
	}
}
