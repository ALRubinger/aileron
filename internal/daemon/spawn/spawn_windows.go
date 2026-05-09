//go:build windows

package spawn

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachProcAttrs returns the SysProcAttr that detaches the daemon
// from the parent's console and signal group. DETACHED_PROCESS gives
// the daemon no console; CREATE_NEW_PROCESS_GROUP isolates it from
// Ctrl-C / Ctrl-Break sent to the parent's group, so the daemon
// survives the parent's exit.
func detachProcAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
