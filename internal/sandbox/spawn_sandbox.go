//go:build !darwin && !linux && !windows

package sandbox

import "os/exec"

// applyPlatformSandbox is the per-OS hook that hardens a prepared
// os/exec.Cmd before Run. The platform-specific implementations
// (Linux namespaces + Landlock, macOS sandbox-exec, Windows job
// objects + restricted token) live in build-tagged sibling files.
//
// This fallback implementation runs on platforms ADR-0014 designates
// as unsupported (BSD, illumos, any non-Linux/macOS/Windows OS).
// Per ADR-0014's "Graceful unavailability" section, the runtime
// refuses to spawn rather than spawn unconfined: the function
// returns [ErrSpawnUnavailable] when the manifest declares any
// sandbox parameters, which hostSpawn translates into a structured
// `spawn_sandbox_unavailable` error.
//
// A manifest that declares no FS scopes, no network, and no proxy
// address (`limits` zero-value) gets the legacy no-op behavior so
// pre-sandbox connectors continue to function for tests and
// internal smoke checks. Production BYOCLI connectors always
// declare at least an FS scope and therefore land in the
// unavailable path on these OSes.
func applyPlatformSandbox(cmd *exec.Cmd, env SpawnEnvelope, limits SpawnLimits) (platformSandboxHooks, error) {
	_ = cmd
	_ = env
	if limits.PlatformSandboxRequested() {
		return platformSandboxHooks{}, ErrSpawnUnavailable
	}
	return platformSandboxHooks{}, nil
}
