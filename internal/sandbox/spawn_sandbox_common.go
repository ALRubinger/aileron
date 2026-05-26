package sandbox

// applyUnsupportedSandbox is the OS-agnostic decision for what to
// do when a connector requests platform-sandbox confinement on a
// platform whose [applyPlatformSandbox] has no implementation
// (BSD, illumos, etc., per ADR-0014's "Graceful unavailability"
// section). Returning [ErrSpawnUnavailable] makes the runtime
// refuse to spawn rather than spawn unconfined; the host function
// translates the wrapped error into a structured
// `spawn_sandbox_unavailable` class.
//
// A manifest that declares no FS scopes, no network, and no proxy
// address (zero-value SpawnLimits) gets the legacy no-op behavior
// so pre-sandbox tests and internal smoke checks continue to
// function. Connectors that declare any sandbox-relevant scope
// land in the unavailable path on unsupported OSes.
//
// Lives in a non-build-tagged file so every supported platform's
// CI run measures it. The build-tagged entry point in
// spawn_sandbox.go forwards here.
func applyUnsupportedSandbox(limits SpawnLimits) (platformSandboxHooks, error) {
	if limits.PlatformSandboxRequested() {
		return platformSandboxHooks{}, ErrSpawnUnavailable
	}
	return platformSandboxHooks{}, nil
}
