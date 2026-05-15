//go:build linux

package sandbox

// checkPlatformSandboxAvailable probes the Linux sandbox primitive:
// `kernel.unprivileged_userns_clone` sysctl. See
// [checkLinuxSandboxAvailable] for the full discussion of fallback
// semantics on kernels that don't expose the knob.
//
// Landlock LSM is NOT probed here separately even though Linux v2
// uses it: the helper applies Landlock at spawn time and emits a
// structured error there if the kernel rejects the syscalls. A
// host that has user namespaces but not Landlock can still run
// connectors that omit fs_read / fs_write; the install-time probe
// is intentionally permissive so the kernel-level enforcement
// surface is the precise failure point.
func checkPlatformSandboxAvailable() error {
	return checkLinuxSandboxAvailable()
}
