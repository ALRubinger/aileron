//go:build windows

package main

import (
	"errors"
	"os"
)

// signalDaemonStop terminates the daemon process. Windows has no SIGTERM
// equivalent that flows to a non-console child in another session, so
// this calls TerminateProcess (via os.Process.Kill). The daemon's
// shutdown defer does NOT run, so daemon.json and daemon.pid may be
// left behind — the next aileron command will detect the stale entry
// (via this same function returning notRunning=true) and clean up.
//
// Returns (true, nil) when the PID is not alive — either FindProcess
// failed to open the process (e.g. PID does not exist), or Kill
// reported the process was already done.
func signalDaemonStop(pid int) (notRunning bool, err error) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Windows os.FindProcess returns an error when OpenProcess
		// fails — typically because the PID doesn't exist or the caller
		// lacks permission. Treat both as "not running" for daemon stop:
		// either the daemon is gone, or we can't terminate it anyway.
		return true, nil
	}
	defer func() { _ = proc.Release() }()
	if err := proc.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
