//go:build !darwin && !linux && !windows

package sandbox

import "os/exec"

// applyPlatformSandbox is the per-OS hook that hardens a prepared
// os/exec.Cmd before Run. The platform-specific implementations
// (Linux namespaces + Landlock, macOS sandbox-exec, Windows job
// objects + restricted token) live in build-tagged sibling files.
//
// This fallback runs on platforms ADR-0014 designates as
// unsupported (BSD, illumos, any non-Linux/macOS/Windows OS).
// The decision logic is in [applyUnsupportedSandbox] so it
// remains testable from any platform's CI; this build-tagged
// entry point just forwards.
func applyPlatformSandbox(cmd *exec.Cmd, env SpawnEnvelope, limits SpawnLimits) (platformSandboxHooks, error) {
	_ = cmd
	_ = env
	return applyUnsupportedSandbox(limits)
}
