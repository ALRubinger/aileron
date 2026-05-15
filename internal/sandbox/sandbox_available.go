package sandbox

// SandboxAvailable reports whether the spawn primitive's
// platform sandbox is usable on this host. Returns nil when the
// runtime can install the confinement layers ADR-0014 commits
// to (FS scoping, network egress denial, process scoping);
// returns a wrapped [ErrSpawnUnavailable] with an actionable
// remediation hint otherwise.
//
// Intended for install-time use so a user installing a spawn-
// using connector on a host without kernel-level confinement
// gets a clear error before any binary is fetched or stored —
// per ADR-0014's "Graceful unavailability" section and #719's
// cross-platform checklist.
//
// Implementation lives in build-tagged sibling files: each
// platform probes the OS-specific primitives the runtime would
// need at spawn time:
//
//   - Linux: `unprivileged_userns_clone` sysctl.
//   - macOS: presence of `/usr/bin/sandbox-exec`.
//   - Windows: open + close a session handle on the Windows
//     Filtering Platform engine (validates BFE is running and
//     the daemon has the rights to install filters).
//   - Other (BSD, illumos, etc.): the fallback returns
//     [ErrSpawnUnavailable] unconditionally; ADR-0014 designates
//     those platforms as deny-spawn.
func SandboxAvailable() error {
	return checkPlatformSandboxAvailable()
}
