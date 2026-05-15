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
)

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
	if err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, SpawnLimits{}); err != nil {
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
	if err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, limits); err != nil {
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
	limits := SpawnLimits{FSRead: []string{"~/code/"}}
	if err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/sh"}, limits); err != nil {
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
	err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: "/bin/echo"}, limits)
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
