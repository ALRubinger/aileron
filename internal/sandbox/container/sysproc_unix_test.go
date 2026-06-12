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
// reach docker/podman directly (issue #999, ADR-0025).
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

// TestSetRuntimeChildForegroundSetsForeground asserts the helper hands
// the child the controlling terminal's foreground process group so an
// interactive `docker run -t` can enter raw mode. Regression for the
// "unable to set IO streams as raw terminal: interrupted system call"
// failure introduced when #1014 isolated the runtime child into a
// background process group.
func TestSetRuntimeChildForegroundSetsForeground(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
	setRuntimeChildForeground(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil; foreground was not configured")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("SysProcAttr.Setpgid = false, want true (Foreground requires Setpgid)")
	}
	if !cmd.SysProcAttr.Foreground {
		t.Fatal("SysProcAttr.Foreground = false, want true")
	}
	if cmd.SysProcAttr.Ctty != 0 {
		t.Fatalf("SysProcAttr.Ctty = %d, want 0 (child stdin)", cmd.SysProcAttr.Ctty)
	}
}

// TestSetRuntimeChildForegroundPreservesExistingFields verifies the
// helper mutates rather than replaces a pre-set SysProcAttr.
func TestSetRuntimeChildForegroundPreservesExistingFields(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", ":")
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	existing := cmd.SysProcAttr
	setRuntimeChildForeground(cmd)
	if cmd.SysProcAttr != existing {
		t.Fatal("helper replaced the existing SysProcAttr rather than mutating it")
	}
}

// TestConfigureRuntimeChildGating pins the foreground gate: an
// interactive `run -t` on a real terminal gets the foreground handoff,
// while a non-terminal stdin or a non-`run`/non-TTY argv stays on the
// background-isolation path. The background path is what #1014 wants for
// CI and for graceful-stop salvage; the foreground path is what an
// interactive raw-mode container needs.
func TestConfigureRuntimeChildGating(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		stdinTTY       bool
		wantForeground bool
	}{
		{"interactive run on terminal", []string{"run", "--rm", "-i", "-t", "img", "claude"}, true, true},
		{"interactive run without terminal", []string{"run", "--rm", "-i", "-t", "img", "claude"}, false, false},
		{"non-tty run on terminal", []string{"run", "--rm", "-i", "img", "claude"}, true, false},
		{"build on terminal", []string{"build", "-t", "img", "."}, true, false},
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
			if cmd.SysProcAttr.Foreground != tc.wantForeground {
				t.Fatalf("SysProcAttr.Foreground = %v, want %v", cmd.SysProcAttr.Foreground, tc.wantForeground)
			}
		})
	}
}
