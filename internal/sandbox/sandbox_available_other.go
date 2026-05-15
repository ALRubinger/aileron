//go:build !darwin && !linux && !windows

package sandbox

// checkPlatformSandboxAvailable on unsupported platforms (BSD,
// illumos, etc.) returns [ErrSpawnUnavailable] unconditionally.
// ADR-0014 designates these as deny-spawn at install time so the
// user discovers the constraint before any binary is fetched.
func checkPlatformSandboxAvailable() error {
	return ErrSpawnUnavailable
}
