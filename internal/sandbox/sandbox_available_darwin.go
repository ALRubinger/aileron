//go:build darwin

package sandbox

import (
	"fmt"
	"os"
)

// checkPlatformSandboxAvailable probes the macOS sandbox primitive:
// `/usr/bin/sandbox-exec` presence. The binary is part of the base
// macOS install and has been since 10.5; absence indicates a
// drastically non-standard system the runtime can't confine.
func checkPlatformSandboxAvailable() error {
	if _, err := os.Stat(sandboxExecPath); err != nil {
		return fmt.Errorf("%w: sandbox-exec not present at %s", ErrSpawnUnavailable, sandboxExecPath)
	}
	return nil
}
