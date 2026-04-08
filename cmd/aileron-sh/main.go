// aileron-sh is a shell shim that intercepts commands from AI coding agents.
//
// It is installed as the agent's SHELL, so every command the agent runs passes
// through here. Currently a pure passthrough to the real shell; policy
// evaluation will be added in a future phase.
//
// Invocation modes (all standard shell modes):
//   - aileron-sh -c "command"   (how agents typically run commands)
//   - aileron-sh script.sh      (script execution)
//   - aileron-sh                (interactive shell)
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func main() {
	realShell := os.Getenv("AILERON_REAL_SHELL")
	if realShell == "" {
		realShell = "/bin/sh"
	}

	binary, err := exec.LookPath(realShell)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aileron-sh: cannot find shell %q: %v\n", realShell, err)
		os.Exit(127)
	}

	// Use syscall.Exec to replace this process with the real shell.
	// This avoids an intermediate process — the real shell gets our PID,
	// which is what the calling agent expects.
	argv := append([]string{realShell}, os.Args[1:]...)
	env := os.Environ()

	if err := syscall.Exec(binary, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "aileron-sh: exec %q failed: %v\n", binary, err)
		os.Exit(126)
	}
}
