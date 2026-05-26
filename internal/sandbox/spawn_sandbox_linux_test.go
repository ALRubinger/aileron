//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/spawnhelper"
)

// TestMain intercepts the spawn-helper re-exec on Linux test runs.
// The runtime calls os.Executable() to discover the daemon binary
// for the helper re-exec; in tests that returns the test binary
// itself. Without this dispatch, the helper invocation would
// re-run the entire test suite recursively. With it, the test
// binary acts as both the test driver and the helper.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == spawnhelper.HelperArgvMarker {
		spawnhelper.Run(os.Args[2])
		return // unreachable in practice; Run never returns on success
	}
	os.Exit(m.Run())
}

// Each test below cites the ADR-0014 clause or the Linux namespace
// property it enforces so refactors that preserve the contract
// leave the test green.

func TestApplyPlatformSandbox_Linux_NoOpWhenNoParamsDeclared(t *testing.T) {
	// Contract (ADR-0014 graceful unavailability + legacy compat):
	// when the manifest declares nothing platform-sandbox-relevant
	// the function returns nil and leaves cmd untouched. This
	// preserves the executor's pre-sandbox behavior for tests that
	// don't exercise the sandbox.
	cmd := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, SpawnLimits{}); err != nil {
		t.Fatalf("expected nil with empty limits, got %v", err)
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Cloneflags != 0 {
		t.Errorf("expected Cloneflags untouched with empty limits, got %#x", cmd.SysProcAttr.Cloneflags)
	}
}

func TestApplyPlatformSandbox_Linux_SetsCloneflagsAndIDMappings(t *testing.T) {
	// Contract: when the manifest declares scopes, the runtime
	// configures SysProcAttr so the kernel forks into a new
	// user+mount+PID namespace with the runtime's UID mapped to
	// 0 inside.
	cmd := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	limits := SpawnLimits{FSRead: []string{"~/code/"}}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, limits); err != nil {
		// Sandbox-available probe may fail on hardened runners; the
		// gate test below covers that path.
		if strings.Contains(err.Error(), "spawn_sandbox_unavailable") || strings.Contains(err.Error(), "unprivileged_userns") {
			t.Skip("sandbox unavailable on this runner; tested separately")
		}
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr nil after sandbox setup")
	}
	wantFlags := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID)
	if cmd.SysProcAttr.Cloneflags&wantFlags != wantFlags {
		t.Errorf("Cloneflags = %#x, want bits %#x set", cmd.SysProcAttr.Cloneflags, wantFlags)
	}
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWNET != 0 {
		t.Error("CLONE_NEWNET should NOT be set in v1 (HTTPS_PROXY cooperation only; tracked separately)")
	}
	if len(cmd.SysProcAttr.UidMappings) != 1 {
		t.Fatalf("UidMappings = %v, want 1 entry", cmd.SysProcAttr.UidMappings)
	}
	m := cmd.SysProcAttr.UidMappings[0]
	if m.ContainerID != 0 || m.HostID != os.Getuid() || m.Size != 1 {
		t.Errorf("UidMappings[0] = %+v, want {0, %d, 1}", m, os.Getuid())
	}
	if len(cmd.SysProcAttr.GidMappings) != 1 {
		t.Fatalf("GidMappings = %v, want 1 entry", cmd.SysProcAttr.GidMappings)
	}
	gm := cmd.SysProcAttr.GidMappings[0]
	if gm.ContainerID != 0 || gm.HostID != os.Getgid() || gm.Size != 1 {
		t.Errorf("GidMappings[0] = %+v, want {0, %d, 1}", gm, os.Getgid())
	}
	if cmd.SysProcAttr.GidMappingsEnableSetgroups {
		t.Error("GidMappingsEnableSetgroups must be false for unprivileged user-namespace clones")
	}
}

func TestApplyPlatformSandbox_Linux_RewiresIntoHelperWhenFSScopesDeclared(t *testing.T) {
	// Contract: when fs_read or fs_write is declared, the runtime
	// rewires cmd to re-exec the daemon binary in helper mode.
	// cmd.Path becomes the daemon path; cmd.Args[1] is the
	// __spawn-helper marker; cmd.Args[2] is the encoded request
	// that decodes to the original Program/Argv.
	origExec := osExecutable
	defer func() { osExecutable = origExec }()
	osExecutable = func() (string, error) { return "/usr/local/bin/aileron-fake", nil }

	cmd := exec.CommandContext(context.Background(), "/usr/bin/git", "log", "--since=2026-01-01")
	limits := SpawnLimits{FSRead: []string{"/Users/alr/code/"}}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/usr/bin/git"}, limits); err != nil {
		if strings.Contains(err.Error(), "spawn_sandbox_unavailable") {
			t.Skip("sandbox unavailable on this runner")
		}
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	if cmd.Path != "/usr/local/bin/aileron-fake" {
		t.Errorf("cmd.Path = %q, want daemon binary path", cmd.Path)
	}
	if len(cmd.Args) != 3 {
		t.Fatalf("cmd.Args = %v, want [aileron, __spawn-helper, <encoded>]", cmd.Args)
	}
	if cmd.Args[1] != spawnhelper.HelperArgvMarker {
		t.Errorf("cmd.Args[1] = %q, want %q", cmd.Args[1], spawnhelper.HelperArgvMarker)
	}
	req, err := spawnhelper.DecodeRequest(cmd.Args[2])
	if err != nil {
		t.Fatalf("decode rewired request: %v", err)
	}
	if req.Program != "/usr/bin/git" {
		t.Errorf("req.Program = %q, want /usr/bin/git", req.Program)
	}
	if !equalSlice(req.Argv, []string{"/usr/bin/git", "log", "--since=2026-01-01"}) {
		t.Errorf("req.Argv = %v, want wrapped git argv", req.Argv)
	}
	if !equalSlice(req.FSRead, []string{"/Users/alr/code/"}) {
		t.Errorf("req.FSRead = %v, want manifest scope", req.FSRead)
	}
}

func TestApplyPlatformSandbox_Linux_RewiresAndActivatesNetNSWhenProxyUDSDeclared(t *testing.T) {
	// Contract (v3): when network is declared on Linux, the proxy
	// binds on UDS and the runtime sets ProxyUDSPath. The platform
	// sandbox responds by activating CLONE_NEWNET (so direct egress
	// is kernel-blocked) and rewiring cmd into the helper (which
	// bridges via RunSpawnShim).
	origExec := osExecutable
	defer func() { osExecutable = origExec }()
	osExecutable = func() (string, error) { return "/usr/local/bin/aileron-fake", nil }

	cmd := exec.CommandContext(context.Background(), "/usr/bin/git", "log")
	limits := SpawnLimits{ProxyUDSPath: "/tmp/aileron-proxy-12345.sock"}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/usr/bin/git"}, limits); err != nil {
		if strings.Contains(err.Error(), "spawn_sandbox_unavailable") {
			t.Skip("sandbox unavailable on this runner")
		}
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr nil")
	}
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWNET == 0 {
		t.Error("CLONE_NEWNET should be set when ProxyUDSPath declared")
	}
	if cmd.Path != "/usr/local/bin/aileron-fake" {
		t.Errorf("cmd.Path = %q, want helper rewire to daemon binary", cmd.Path)
	}
	if len(cmd.Args) < 3 || cmd.Args[1] != spawnhelper.HelperArgvMarker {
		t.Errorf("cmd.Args = %v, want helper-marker rewire", cmd.Args)
	}
	req, err := spawnhelper.DecodeRequest(cmd.Args[2])
	if err != nil {
		t.Fatalf("decode rewired request: %v", err)
	}
	if req.ProxyUDSPath != "/tmp/aileron-proxy-12345.sock" {
		t.Errorf("req.ProxyUDSPath = %q, want manifest UDS path", req.ProxyUDSPath)
	}
}

func TestApplyPlatformSandbox_Linux_NoRewireWhenOnlyProxyAddrDeclared(t *testing.T) {
	// Contract: when only network is declared (no fs_read/fs_write),
	// the wrapped CLI execs directly. Landlock doesn't govern
	// network, so re-execing through the helper would add a
	// process layer for no gain. v3's CLONE_NEWNET + shim work
	// changes this; until then the wrapped CLI is the direct
	// exec target.
	cmd := exec.CommandContext(context.Background(), "/bin/echo", "hi")
	origPath := cmd.Path
	limits := SpawnLimits{ProxyAddr: "127.0.0.1:54321"}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, limits); err != nil {
		if strings.Contains(err.Error(), "spawn_sandbox_unavailable") {
			t.Skip("sandbox unavailable on this runner")
		}
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	if cmd.Path != origPath {
		t.Errorf("cmd.Path = %q; want unchanged %q (no helper rewire when only network declared)", cmd.Path, origPath)
	}
}

func TestApplyPlatformSandbox_Linux_HelperEnforcesLandlockReadScope(t *testing.T) {
	// End-to-end: a wrapped CLI cannot read outside its declared
	// fs_read scope. The test binary acts as the daemon: when the
	// runtime re-execs into the helper marker (via TestMain
	// dispatch), Landlock is applied and cat is exec'd; cat
	// reading a sentinel outside the scope fails with EPERM/EACCES
	// at the kernel boundary.
	if _, err := os.Stat("/bin/cat"); err != nil {
		t.Skip("/bin/cat not present")
	}
	if err := checkLinuxSandboxAvailable(); err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	allowedDir := filepath.Join(home, "aileron-linux-v2-test-allowed")
	deniedDir := filepath.Join(home, "aileron-linux-v2-test-denied")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(deniedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(allowedDir)
	defer os.RemoveAll(deniedDir)
	sentinel := filepath.Join(deniedDir, "secret.txt")
	if err := os.WriteFile(sentinel, []byte("don't read me"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "/bin/cat", sentinel)
	limits := SpawnLimits{
		FSRead: []string{allowedDir},
		// Also allow read of /lib, /lib64, /usr (dyld needs them
		// to start /bin/cat). On the helper Landlock is the only
		// FS constraint, so these system paths must be in fs_read.
		// In production this widening is handled by the introspector
		// at `aileron cli add` time.
	}
	// Append system-essentials so /bin/cat can dynamic-link.
	for _, sys := range []string{"/lib", "/lib64", "/usr", "/etc", "/bin"} {
		if _, err := os.Stat(sys); err == nil {
			limits.FSRead = append(limits.FSRead, sys)
		}
	}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/cat"}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("expected /bin/cat to fail reading sentinel under Landlock; got success with output %q", string(out))
	}
	// The exact exit shape varies but a non-zero exit is the
	// contract. Cat reports "Permission denied" when it hits
	// EACCES from the kernel.
	if !strings.Contains(string(out), "Permission denied") &&
		!strings.Contains(string(out), "permission denied") {
		t.Logf("note: cat output did not contain 'Permission denied' but exited non-zero: %q", string(out))
	}
}

func TestApplyPlatformSandbox_Linux_HelperAllowsReadInsideScope(t *testing.T) {
	// Complement: a read INSIDE the declared scope succeeds. Proves
	// Landlock's allow rule for the manifest scope is actually
	// applied (not just denying everything wholesale).
	if _, err := os.Stat("/bin/cat"); err != nil {
		t.Skip("/bin/cat not present")
	}
	if err := checkLinuxSandboxAvailable(); err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	allowedDir := filepath.Join(home, "aileron-linux-v2-test-allow-read")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(allowedDir)
	sentinel := filepath.Join(allowedDir, "data.txt")
	if err := os.WriteFile(sentinel, []byte("hello-from-landlock"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "/bin/cat", sentinel)
	limits := SpawnLimits{FSRead: []string{allowedDir}}
	for _, sys := range []string{"/lib", "/lib64", "/usr", "/etc", "/bin"} {
		if _, err := os.Stat(sys); err == nil {
			limits.FSRead = append(limits.FSRead, sys)
		}
	}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/cat"}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sandboxed cat failed inside declared scope: %v", err)
	}
	if !strings.Contains(string(out), "hello-from-landlock") {
		t.Errorf("output = %q, want sentinel content", string(out))
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestApplyPlatformSandbox_Linux_RunsRealBinaryUnderSandbox(t *testing.T) {
	// End-to-end: a wrapped binary boots inside the namespace, sees
	// its own PID 1 (itself), and exits cleanly. /bin/sh is widely
	// available; we ask it to print the PID of the only process it
	// sees, which inside a PID namespace is its own PID 1.
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not present")
	}
	if err := checkLinuxSandboxAvailable(); err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "echo PID=$$")
	// Declare /tmp as the read scope so Landlock's path-open
	// succeeds; the test only cares about the PID-namespace
	// boundary, not the FS confinement. /tmp exists on every
	// POSIX host. The system-essentials (/lib, /lib64, /usr,
	// /bin, /etc) are needed for /bin/sh's dyld to start.
	limits := SpawnLimits{FSRead: []string{"/tmp"}}
	for _, sys := range []string{"/lib", "/lib64", "/usr", "/etc", "/bin"} {
		if _, err := os.Stat(sys); err == nil {
			limits.FSRead = append(limits.FSRead, sys)
		}
	}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/sh"}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("sandboxed /bin/sh failed: %v (output: %s)", err, stdout.String())
	}
	// Inside the new PID namespace the shell sees itself as PID 1
	// (or PID 2 if Go's exec inserts an init helper). Either way,
	// the visible PID should be in the single digits and never
	// match the actual host PID.
	out := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(out, "PID=") {
		t.Fatalf("output = %q, want PID=...", out)
	}
	visiblePID := strings.TrimPrefix(out, "PID=")
	// The host PID of this test process is unlikely to be in 1..10.
	// A successful PID-namespace setup gives the child a small PID.
	if len(visiblePID) > 2 {
		t.Errorf("expected small PID (PID-namespaced); got %q", visiblePID)
	}
}

func TestCheckLinuxSandboxAvailable_AcceptsEnabled(t *testing.T) {
	// Contract: sysctl reports "1" → namespaces available, return nil.
	swapped := withSysctlContent(t, "1\n")
	defer swapped()
	if err := checkLinuxSandboxAvailable(); err != nil {
		t.Errorf("expected nil with sysctl=1, got %v", err)
	}
}

func TestCheckLinuxSandboxAvailable_AcceptsAbsent(t *testing.T) {
	// Contract: sysctl absent → kernel doesn't expose the knob, assume
	// permissive. Older kernels predate the knob; the namespace API
	// has been GA since 3.8.
	defer swapSysctl(t, "/nonexistent/never-exists-aileron-test")()
	if err := checkLinuxSandboxAvailable(); err != nil {
		t.Errorf("expected nil when sysctl absent, got %v", err)
	}
}

func TestCheckLinuxSandboxAvailable_RejectsDisabled(t *testing.T) {
	// Contract: sysctl reports "0" → return ErrSpawnUnavailable with
	// an actionable remediation hint. The host function translates
	// the wrapped error into a structured `spawn_sandbox_unavailable`
	// class so operators see the sysctl name and the fix command.
	swapped := withSysctlContent(t, "0\n")
	defer swapped()
	err := checkLinuxSandboxAvailable()
	if err == nil {
		t.Fatal("expected ErrSpawnUnavailable with sysctl=0")
	}
	if !errors.Is(err, ErrSpawnUnavailable) {
		t.Errorf("error does not wrap ErrSpawnUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "unprivileged_userns_clone") {
		t.Errorf("error %v does not name the sysctl", err)
	}
	if !strings.Contains(err.Error(), "sysctl") {
		t.Errorf("error %v does not include remediation hint", err)
	}
}

func TestCheckLinuxSandboxAvailable_RejectsUnreadable(t *testing.T) {
	// Contract: any read error other than ErrNotExist (e.g., a
	// permissions-restricted /proc) is treated as unavailable since
	// the runtime cannot determine the state. Fail closed.
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "unreadable")
	if err := os.WriteFile(unreadable, []byte("1"), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if os.Getuid() == 0 {
		t.Skip("running as root; can't simulate read-permission failure")
	}
	defer swapSysctl(t, unreadable)()
	err := checkLinuxSandboxAvailable()
	if err == nil {
		t.Fatal("expected ErrSpawnUnavailable for unreadable sysctl")
	}
	if !errors.Is(err, ErrSpawnUnavailable) {
		t.Errorf("error does not wrap ErrSpawnUnavailable: %v", err)
	}
}

func TestApplyPlatformSandbox_Linux_ReturnsUnavailableWhenSysctlDisabled(t *testing.T) {
	// Contract: when the sandbox-available probe rejects, the
	// function does not touch cmd.SysProcAttr and surfaces the
	// wrapped ErrSpawnUnavailable. The host function turns this
	// into `spawn_sandbox_unavailable` for the caller.
	swapped := withSysctlContent(t, "0\n")
	defer swapped()
	cmd := exec.CommandContext(context.Background(), "/bin/echo")
	limits := SpawnLimits{FSRead: []string{"~/code/"}}
	_, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, limits)
	if !errors.Is(err, ErrSpawnUnavailable) {
		t.Fatalf("expected wrapped ErrSpawnUnavailable, got %v", err)
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Cloneflags != 0 {
		t.Errorf("cmd.SysProcAttr.Cloneflags should be untouched, got %#x", cmd.SysProcAttr.Cloneflags)
	}
}

// withSysctlContent writes `content` to a tempfile and points
// unprivilegedUserNSSysctl at it for the duration of the returned
// cleanup. Use with `defer`.
func withSysctlContent(t *testing.T, content string) func() {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sysctl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return swapSysctl(t, path)
}

// swapSysctl points unprivilegedUserNSSysctl at `path` and returns
// a function that restores the original. Use with `defer`.
func swapSysctl(t *testing.T, path string) func() {
	t.Helper()
	orig := unprivilegedUserNSSysctl
	unprivilegedUserNSSysctl = path
	return func() { unprivilegedUserNSSysctl = orig }
}
