//go:build unix

package main

import (
	"errors"
	"syscall"
)

// signalDaemonStop sends SIGTERM to pid for graceful shutdown. The
// daemon's own signal handler tears down the listener and removes
// daemon.json/daemon.pid via its shutdown defer (see internal/server).
//
// Returns (true, nil) when the PID is not alive (ESRCH) — the caller
// treats this as a stale daemon.json from a prior crash and cleans up.
func signalDaemonStop(pid int) (notRunning bool, err error) {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
