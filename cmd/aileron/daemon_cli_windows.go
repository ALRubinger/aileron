//go:build windows

package main

import (
	"errors"
	"os"
)

// signalDaemonStop terminates the daemon process. Windows has no SIGTERM
// equivalent that flows to a non-console child in another session, so
// this calls TerminateProcess (via os.Process.Kill). The daemon's
// shutdown defer does NOT run, so daemon.json and daemon.pid are left
// behind. selfCleans is therefore false on a successful kill: the caller
// must remove the discovery files itself instead of waiting for the
// daemon to perform cleanup it can never run.
//
// Returns (notRunning=true, selfCleans=false, nil) when the PID is not
// alive — either FindProcess failed to open the process (e.g. PID does
// not exist), or Kill reported the process was already done.
func signalDaemonStop(pid int) (notRunning, selfCleans bool, err error) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Windows os.FindProcess returns an error when OpenProcess
		// fails — typically because the PID doesn't exist or the caller
		// lacks permission. Treat both as "not running" for daemon stop:
		// either the daemon is gone, or we can't terminate it anyway.
		return true, false, nil
	}
	defer func() { _ = proc.Release() }()
	if err := proc.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return true, false, nil
		}
		return false, false, err
	}
	// Kill via TerminateProcess is abrupt and uncatchable, so the daemon
	// never runs its shutdown defer. The caller cleans up discovery files.
	return false, false, nil
}
