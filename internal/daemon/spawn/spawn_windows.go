//go:build windows

package spawn

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachProcAttrs returns the SysProcAttr that detaches the daemon
// from the parent's console and signal group. CREATE_NO_WINDOW is the
// Microsoft-documented flag for running a console application with no
// console window: unlike DETACHED_PROCESS, it does not let Windows
// allocate a fresh console window for the console-subsystem
// aileron-server binary when it is launched from an interactive shell.
// CREATE_NEW_PROCESS_GROUP isolates the daemon from Ctrl-C /
// Ctrl-Break sent to the parent's group, so the daemon survives the
// parent's exit. The daemon's stdio is already redirected to
// <stateDir>/daemon.log, so it needs no console.
//
// CREATE_NO_WINDOW is mutually exclusive with DETACHED_PROCESS and
// CREATE_NEW_CONSOLE but composes with CREATE_NEW_PROCESS_GROUP.
func detachProcAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
