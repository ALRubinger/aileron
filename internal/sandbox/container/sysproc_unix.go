//go:build !windows

package container

import (
	"os/exec"
	"syscall"
)

// setRuntimeChildPgid places the container-runtime child (docker/podman)
// in its own process group so a terminal Ctrl-C does not reach it
// directly.
//
// A one-shot sandbox container runs in the foreground, so without this
// the `docker run` child shares aileron's process group. A terminal
// SIGINT is delivered to the whole foreground process group, which would
// hit `docker` concurrently with aileron's own salvage handler and tear
// the container down out from under the graceful `docker stop --time`.
// Setpgid isolates the child so the terminal SIGINT reaches only
// aileron, leaving aileron's handler solely responsible for an orderly
// stop-then-Capture. See ADR-0025 and issue #999.
//
// Existing SysProcAttr fields are preserved; only Setpgid is forced on.
func setRuntimeChildPgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// setRuntimeChildForeground promotes the runtime child's new process
// group to the foreground of the controlling terminal so an interactive
// `docker run -t` can put the terminal into raw mode. Without it the
// child sits in the background group set by setRuntimeChildPgid and the
// raw-mode tcsetattr fails with SIGTTOU/EINTR.
//
// The Go runtime performs the tcsetpgrp(Ctty) handoff in the child with
// SIGTTOU masked. Ctty is the child's stdin (fd 0), which execRunner
// wires to the host terminal. Callers gate this on a real stdin
// terminal; setting Foreground without a controlling terminal would make
// the exec fail. Setpgid is required for Foreground, so it is forced on
// here too. See ADR-0025 and issue #1014.
func setRuntimeChildForeground(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Foreground = true
	cmd.SysProcAttr.Ctty = 0
}
