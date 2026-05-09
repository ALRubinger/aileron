//go:build unix

package spawn

import "syscall"

// detachProcAttrs returns the SysProcAttr that detaches the daemon
// from the parent's terminal session. Setsid creates a new session and
// process group so SIGHUP from the parent's terminal does not
// propagate, and the daemon survives the parent's exit.
func detachProcAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
