//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// unprivilegedUserNSSysctl is the kernel knob distros sometimes
// disable for hardening (notably old Debian / Ubuntu LTS releases
// and some grsecurity-patched kernels). When set to 0, an
// unprivileged process cannot create a user namespace via
// `unshare(CLONE_NEWUSER)`, which makes [ADR-0014]'s Linux spawn
// sandbox unavailable.
//
// Declared as a var (not const) so tests can swap it to a tempfile
// with controlled contents and exercise the disabled-sysctl and
// IO-error paths that the production kernel won't reproduce on a
// healthy CI runner.
//
// [ADR-0014]: https://docs.withaileron.ai/adr/0014-spawn-sandbox-technology
var unprivilegedUserNSSysctl = "/proc/sys/kernel/unprivileged_userns_clone"

// applyPlatformSandbox wires the wrapped subprocess to launch in a
// fresh user + mount + PID namespace, providing process-level
// isolation per ADR-0014's Linux section.
//
// Scope of this implementation (Linux v1):
//
//   - **User namespace** maps the runtime's UID to UID 0 inside the
//     namespace; the subprocess cannot escalate to host-root or
//     affect any other UID's resources.
//   - **Mount namespace** is created (CLONE_NEWNS) so the
//     subprocess's future mount operations cannot leak to the host.
//     The mount tree itself is not rewritten in v1; a follow-up PR
//     adds pivot_root + bind-mounted fs_read/fs_write scopes for
//     real kernel-enforced FS confinement (paired with Landlock LSM
//     on kernels that support it).
//   - **PID namespace** makes the subprocess PID 1 inside its
//     namespace; it cannot see or signal host processes.
//   - **Network namespace is NOT activated in v1** so the
//     subprocess shares the host's network. The CONNECT proxy
//     [#716] runs on host loopback, so HTTPS_PROXY injection
//     works without additional shim plumbing. The trade-off: direct
//     egress to non-allowlisted hosts is not kernel-enforced. A
//     follow-up PR adds CLONE_NEWNET plus the in-namespace TCP-to-UDS
//     shim ([#718]) for kernel-enforced egress confinement.
//
// Returns [ErrSpawnUnavailable] when unprivileged user namespaces
// are disabled at the sysctl level. Returns nil with no
// modification to `cmd` when no manifest sandbox parameters were
// declared (legacy path; pre-BYOCLI connectors continue to run
// unchanged).
func applyPlatformSandbox(cmd *exec.Cmd, env SpawnEnvelope, limits SpawnLimits) (platformSandboxHooks, error) {
	_ = env
	if !limits.PlatformSandboxRequested() {
		return platformSandboxHooks{}, nil
	}
	if err := checkLinuxSandboxAvailable(); err != nil {
		return platformSandboxHooks{}, err
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: os.Getuid(), Size: 1},
	}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: os.Getgid(), Size: 1},
	}
	// GidMappingsEnableSetgroups must be false for an unprivileged
	// user-namespace clone: writing to /proc/<pid>/setgroups before
	// the gid_map is the kernel's required permission gate, and
	// Go's exec package handles this when this flag is false.
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	return platformSandboxHooks{}, nil
}

// checkLinuxSandboxAvailable returns ErrSpawnUnavailable when the
// running kernel disables unprivileged user namespaces. Older
// kernels that don't expose the sysctl at all are assumed
// permissive (the namespace API has been GA since 3.8, and the
// sysctl is a Debian/Ubuntu-ism rather than a kernel default).
//
// Returns nil on systems where the spawn primitive can run.
// Returns ErrSpawnUnavailable with a remediation hint in the
// error message when the sysctl reports unavailability so the
// host function emits a structured `spawn_sandbox_unavailable`
// error with actionable text.
func checkLinuxSandboxAvailable() error {
	raw, err := os.ReadFile(unprivilegedUserNSSysctl)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Sysctl absent: kernel doesn't expose this knob.
			// Assume namespaces are available; the clone() will
			// fail at exec time with EPERM if it isn't.
			return nil
		}
		// Read error other than absence: surface as unavailable
		// since we cannot determine the state.
		return fmt.Errorf("%w: could not read %s: %v", ErrSpawnUnavailable, unprivilegedUserNSSysctl, err)
	}
	val := strings.TrimSpace(string(raw))
	if val == "1" {
		return nil
	}
	return fmt.Errorf("%w: unprivileged user namespaces disabled (kernel.unprivileged_userns_clone=%s); enable with `sudo sysctl -w kernel.unprivileged_userns_clone=1`",
		ErrSpawnUnavailable, val)
}
