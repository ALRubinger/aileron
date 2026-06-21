//go:build !windows

package container

import (
	"os/exec"
	"syscall"
)

// setRuntimeChildPgid places the container-runtime child (docker)
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
