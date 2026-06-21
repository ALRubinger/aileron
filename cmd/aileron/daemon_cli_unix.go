//go:build unix

package main

import (
	"errors"
	"syscall"
)

// signalDaemonStop sends SIGTERM to pid for graceful shutdown. The
// daemon's own signal handler tears down the listener and removes
// daemon.json/daemon.pid via its shutdown defer (see internal/server),
// so selfCleans is true: the caller waits for the daemon to remove its
// own discovery files rather than removing them itself.
//
// Returns (notRunning=true, selfCleans=false, nil) when the PID is not
// alive (ESRCH) — the caller treats this as a stale daemon.json from a
// prior crash and cleans up.
func signalDaemonStop(pid int) (notRunning, selfCleans bool, err error) {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return true, false, nil
		}
		return false, false, err
	}
	return false, true, nil
}
