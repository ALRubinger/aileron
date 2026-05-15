package sandbox

import (
	"errors"
	"runtime"
	"testing"
)

func TestSandboxAvailable_ReturnsNilOnSupportedPlatformsByDefault(t *testing.T) {
	// Contract: on the three supported platforms (linux, darwin,
	// windows) SandboxAvailable returns nil on a healthy host.
	// CI runners are healthy by construction; if the test fails
	// the runner has a sandbox-disabling configuration the user
	// would also hit at install time, which is the right signal.
	err := SandboxAvailable()
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		if err != nil {
			t.Errorf("%s: SandboxAvailable returned %v on a supported platform", runtime.GOOS, err)
		}
	default:
		// On unsupported platforms the contract is
		// ErrSpawnUnavailable. Asserts the fallback wires through.
		if !errors.Is(err, ErrSpawnUnavailable) {
			t.Errorf("%s: expected wrapped ErrSpawnUnavailable, got %v", runtime.GOOS, err)
		}
	}
}
