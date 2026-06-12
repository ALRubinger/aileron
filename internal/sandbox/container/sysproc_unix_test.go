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
