package sandbox

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The malicious-CLI fixture is the unified version of the per-
// platform "kernel blocks reads outside fs_read" test #619 / #719
// expect. The same Go fake binary is compiled per runner and run
// through `applyPlatformSandbox` so the test surface stays
// portable across the CI matrix. The assertions live here once
// instead of duplicated per platform with subtly different
// system binaries (`/bin/cat` on Unix, `cmd.exe /c type` on
// Windows, etc.).
//
// Scope:
//
//   - Linux and macOS: full read-confinement test (kernel
//     Landlock or SBPL blocks reads outside the manifest's
//     fs_read scope; reads inside succeed).
//   - Windows: skipped. v1's confinement model is Low-integrity
//     mandatory access control, which blocks WRITES from a Low
//     subject to a Medium object, but does NOT block reads. Real
//     read-confinement on Windows requires AppContainer
//     (ADR-0014 defers). A Windows-tagged write-confinement
//     fixture is a sensible follow-up; conflating it with the
//     cross-platform test here would obscure the threat-model
//     boundaries each platform actually enforces.
//
// The fixture proves the kernel does its job. If the test fails
// the runtime's security claims are not delivered on the
// implicated platform — escalate, do not paper over.

func TestMaliciousCLI_FSReadEnforcement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v1 confinement is Low-integrity (writes only); read confinement requires AppContainer (deferred per ADR-0014)")
	}

	fake := buildFakeBinary(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir on this runner")
	}

	allowedDir := filepath.Join(home, "aileron-mci-allowed")
	deniedDir := filepath.Join(home, "aileron-mci-denied")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	if err := os.MkdirAll(deniedDir, 0o755); err != nil {
		t.Fatalf("mkdir denied: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(allowedDir)
		_ = os.RemoveAll(deniedDir)
	})

	allowedSentinel := filepath.Join(allowedDir, "data.txt")
	deniedSentinel := filepath.Join(deniedDir, "secret.txt")
	if err := os.WriteFile(allowedSentinel, []byte("open data"), 0o600); err != nil {
		t.Fatalf("write allowed sentinel: %v", err)
	}
	if err := os.WriteFile(deniedSentinel, []byte("secret data"), 0o600); err != nil {
		t.Fatalf("write denied sentinel: %v", err)
	}

	t.Run("InsideScope/Allowed", func(t *testing.T) {
		out, runErr := runFakeBinaryThroughSandbox(t, fake, []string{allowedDir}, "read", allowedSentinel)
		if runErr != nil {
			t.Fatalf("read inside scope should succeed, got error %v\noutput: %s", runErr, out)
		}
		if !bytes.Contains(out, []byte("read 9 bytes from "+allowedSentinel)) {
			t.Errorf("expected successful-read output, got: %s", out)
		}
	})

	t.Run("OutsideScope/Blocked", func(t *testing.T) {
		out, runErr := runFakeBinaryThroughSandbox(t, fake, []string{allowedDir}, "read", deniedSentinel)
		if runErr == nil {
			t.Fatalf("read outside scope should be blocked by kernel; succeeded with output: %s", out)
		}
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("expected exec.ExitError, got %T: %v", runErr, runErr)
		}
		if exitErr.ExitCode() == 0 {
			t.Errorf("expected non-zero exit, got %d (output: %s)", exitErr.ExitCode(), out)
		}
		// fakebinary exits 1 with "read failed:" on stderr when
		// the kernel blocks. Exit 2 means the scenario was
		// misused (a real test bug); we differentiate so a
		// misuse doesn't masquerade as enforcement.
		if exitErr.ExitCode() == 2 {
			t.Errorf("fakebinary reported misuse, not kernel-block; output: %s", out)
		}
	})
}

// buildFakeBinary compiles the test fixture's Go program into
// the test's tempdir and returns the resulting binary's path.
// Compilation happens once per `go test` invocation; tests share
// the binary across subtests via t.TempDir's per-test scope
// (the cost of recompiling is ~1s on a warm cache, acceptable).
func buildFakeBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fakebinary")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fakebinary")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build fakebinary: %v", err)
	}
	return out
}

// runFakeBinaryThroughSandbox wraps the fake binary with the
// platform sandbox (using `applyPlatformSandbox`) and runs it
// with the given scenario arguments. Returns combined stdout+
// stderr and the runCaptured error (an *exec.ExitError when the
// subprocess exited non-zero).
//
// The system-essentials read scopes are added automatically per
// runtime.GOOS — on Linux the helper applies Landlock which
// would otherwise block dyld from loading libSystem-equivalent;
// on macOS the SBPL profile's permissive baseline handles
// system reads, so no addition is needed.
func runFakeBinaryThroughSandbox(t *testing.T, fakeBinary string, fsRead []string, scenario, path string) ([]byte, error) {
	t.Helper()
	if err := SandboxAvailable(); err != nil {
		t.Skipf("sandbox not available on this runner: %v", err)
	}

	// Platform-specific system read scopes the wrapped binary
	// needs to start. Linux's Landlock is the strict path-
	// beneath model; macOS handles essentials via its permissive
	// baseline. The runtime treats a trailing-slash entry as a
	// directory subtree (subpath) and slashless as a single
	// file literal — directory scopes need to end with /.
	scopes := make([]string, 0, len(fsRead))
	for _, p := range fsRead {
		scopes = append(scopes, ensureSubpath(p))
	}
	for _, sys := range linuxSystemReadScopes() {
		if _, err := os.Stat(sys); err == nil {
			scopes = append(scopes, ensureSubpath(sys))
		}
	}
	// fakeBinary itself needs to be readable; its directory
	// (typically t.TempDir() under /tmp or /var/folders) must be
	// in the read scope.
	scopes = append(scopes, ensureSubpath(filepath.Dir(fakeBinary)))

	cmd := exec.Command(fakeBinary, "--scenario="+scenario, "--path="+path)
	limits := SpawnLimits{FSRead: scopes}
	if _, err := applyPlatformSandbox(cmd, SpawnEnvelope{Program: fakeBinary}, limits); err != nil {
		t.Fatalf("applyPlatformSandbox: %v", err)
	}
	return cmd.CombinedOutput()
}

// linuxSystemReadScopes returns the paths a Linux subprocess
// needs to read to start under Landlock: dynamic linker plus
// the standard libc paths. macOS / Windows handle these via
// their own permissive baselines and return nil.
func linuxSystemReadScopes() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	return []string{"/lib", "/lib64", "/usr", "/etc", "/bin"}
}

// ensureSubpath appends a trailing slash to the path when one is
// not present. The manifest's `fs_read` semantics distinguish
// subtree scopes (trailing slash → subpath) from literal-file
// scopes (slashless → exact match); the integration test always
// wants subtree scopes, so canonicalize here rather than rely on
// each caller remembering.
func ensureSubpath(p string) string {
	if p == "" {
		return p
	}
	if p[len(p)-1] == '/' {
		return p
	}
	return p + "/"
}
