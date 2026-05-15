//go:build linux

package spawnhelper

import (
	"fmt"
	"os"
	"syscall"
)

// Run is the entry point the daemon's main() dispatches to when
// it sees [HelperArgvMarker] as its first argv. The function
// parses the request, applies Landlock filesystem restrictions,
// optionally chdir's, and then `syscall.Exec`s into the wrapped
// program — replacing this helper process with the CLI under
// the configured confinement.
//
// Never returns on success; the Exec syscall replaces the
// process image. On any failure (malformed request, Landlock
// unavailable, Exec failure) writes a structured error message
// to stderr and exits with code 1 so the runtime audit row
// records a sandbox-setup failure rather than a silently
// unconfined spawn.
func Run(encodedReq string) {
	req, err := DecodeRequest(encodedReq)
	if err != nil {
		failHelper("decode request: %v", err)
	}

	if err := applyLandlock(req.FSRead, req.FSWrite); err != nil {
		failHelper("apply Landlock: %v", err)
	}

	if req.Cwd != "" {
		if err := os.Chdir(req.Cwd); err != nil {
			failHelper("chdir %s: %v", req.Cwd, err)
		}
	}

	// syscall.Exec replaces this process with the wrapped CLI.
	// The Env defaults to os.Environ() when the request didn't
	// carry one; the runtime always provides Env in production
	// but the fallback keeps the helper testable in isolation.
	env := req.Env
	if env == nil {
		env = os.Environ()
	}
	if err := syscall.Exec(req.Program, req.Argv, env); err != nil {
		// Exec only returns on failure.
		failHelper("exec %s: %v", req.Program, err)
	}
}

// failHelper writes a `spawn-helper: <msg>` line to stderr and
// exits the process with code 1. Centralizes the failure shape
// so audit logs and tests can match on it.
func failHelper(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "spawn-helper: "+format+"\n", args...)
	os.Exit(1)
}
