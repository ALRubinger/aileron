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

	"github.com/ALRubinger/aileron/internal/sandbox/spawnhelper"
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

// osExecutable is the function the runtime calls to discover the
// path of the daemon binary it should re-exec into for the
// post-namespace helper. Declared as a var so tests can swap it
// without needing to actually rename the test binary.
var osExecutable = os.Executable

// applyPlatformSandbox wires the wrapped subprocess to launch in a
// fresh user + mount + PID namespace, providing process-level
// isolation per ADR-0014's Linux section. When the manifest
// declares `fs_read` or `fs_write` scopes, the runtime also
// re-execs into the daemon binary's [spawnhelper.Run] entry point
// so Landlock LSM rules apply inside the namespace before the
// wrapped CLI exec's.
//
// Scope of this implementation:
//
//   - **User namespace** maps the runtime's UID to UID 0 inside
//     the namespace; the subprocess cannot escalate to host-root
//     or affect any other UID's resources.
//   - **Mount namespace** is created (CLONE_NEWNS) so the
//     subprocess's future mount operations cannot leak to the
//     host. The mount tree itself is not rewritten yet; a future
//     PR adds pivot_root + bind-mounted fs scopes if Landlock
//     coverage proves insufficient.
//   - **PID namespace** makes the subprocess PID 1 inside its
//     namespace; it cannot see or signal host processes.
//   - **Landlock LSM** is applied inside the helper (when
//     `fs_read` or `fs_write` is declared) so the wrapped CLI
//     cannot read or write outside the declared paths. This is
//     the load-bearing kernel-enforced FS confinement; without
//     it the namespace alone leaves the host filesystem
//     reachable.
//   - **Network namespace is NOT activated** so the subprocess
//     shares the host's network. The CONNECT proxy runs on host
//     loopback; the wrapped CLI honors `HTTPS_PROXY`. The
//     trade-off: direct egress to non-allowlisted hosts is not
//     kernel-enforced. A future PR adds `CLONE_NEWNET` +
//     in-namespace TCP-to-UDS shim wiring ([#718]) for kernel-
//     enforced egress.
//
// Returns [ErrSpawnUnavailable] when unprivileged user namespaces
// are disabled at the sysctl level or when the daemon binary
// path cannot be discovered. Returns nil with no modification to
// `cmd` when no manifest sandbox parameters were declared
// (legacy path; pre-BYOCLI connectors continue to run unchanged).
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

	// When the manifest declares FS scopes, re-exec into the
	// daemon binary's spawn-helper entry point so Landlock applies
	// inside the namespace before the wrapped CLI starts. Network-
	// only scoping (no fs_read / fs_write) keeps the v1 behavior
	// where the wrapped CLI exec's directly.
	if len(limits.FSRead) > 0 || len(limits.FSWrite) > 0 {
		if err := rewireForHelper(cmd, limits); err != nil {
			return platformSandboxHooks{}, err
		}
	}

	return platformSandboxHooks{}, nil
}

// rewireForHelper captures the wrapped program from `cmd`,
// encodes a [spawnhelper.Request] carrying it along with the
// manifest's FS scopes, and rewires `cmd` so the kernel exec's
// the daemon binary in helper mode instead. The helper applies
// Landlock then `syscall.Exec`s the wrapped program — replacing
// itself with the CLI under the configured confinement.
func rewireForHelper(cmd *exec.Cmd, limits SpawnLimits) error {
	helperPath, err := osExecutable()
	if err != nil {
		return fmt.Errorf("%w: locate daemon binary for spawn helper: %v", ErrSpawnUnavailable, err)
	}

	// Snapshot the wrapped invocation before reassigning cmd.Path
	// and cmd.Args; the helper will use these for syscall.Exec.
	req := spawnhelper.Request{
		Program: cmd.Path,
		Argv:    append([]string(nil), cmd.Args...),
		Env:     append([]string(nil), cmd.Env...),
		Cwd:     cmd.Dir,
		FSRead:  append([]string(nil), limits.FSRead...),
		FSWrite: append([]string(nil), limits.FSWrite...),
	}
	encoded, err := spawnhelper.EncodeRequest(req)
	if err != nil {
		return fmt.Errorf("%w: encode helper request: %v", ErrSpawnUnavailable, err)
	}

	cmd.Path = helperPath
	cmd.Args = []string{"aileron", spawnhelper.HelperArgvMarker, encoded}
	// cmd.Env and cmd.Dir carry through to the helper unchanged;
	// the helper uses req.Env / req.Cwd for the wrapped exec.
	return nil
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
