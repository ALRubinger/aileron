package launch

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveDaemonBinary contract:
//   - sibling 'server' next to the running test binary takes priority
//   - falls back to PATH lookup when no sibling exists
//   - returns an error when neither is reachable

func TestResolveDaemonBinary_FindsSibling(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	sibling := filepath.Join(filepath.Dir(self), "server")
	created := false
	if _, err := os.Stat(sibling); err != nil {
		if err := os.WriteFile(sibling, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Skipf("cannot write sibling for test (sandboxed?): %v", err)
		}
		created = true
		t.Cleanup(func() { _ = os.Remove(sibling) })
	}

	got, err := resolveDaemonBinary()
	if err != nil {
		t.Fatalf("resolveDaemonBinary: %v", err)
	}
	if got != sibling {
		t.Errorf("got %q, want sibling %q", got, sibling)
	}
	_ = created
}

func TestResolveDaemonBinary_FallsBackToPATH(t *testing.T) {
	// Make sure no sibling is in the way (best effort).
	if self, err := os.Executable(); err == nil {
		_ = os.Remove(filepath.Join(filepath.Dir(self), "server"))
	}
	binDir := t.TempDir()
	server := filepath.Join(binDir, "server")
	if err := os.WriteFile(server, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	got, err := resolveDaemonBinary()
	if err != nil {
		t.Fatalf("resolveDaemonBinary: %v", err)
	}
	if got == "" {
		t.Fatal("got empty path")
	}
}

func TestResolveDaemonBinary_NotFound(t *testing.T) {
	if self, err := os.Executable(); err == nil {
		_ = os.Remove(filepath.Join(filepath.Dir(self), "server"))
	}
	t.Setenv("PATH", "")
	if _, err := resolveDaemonBinary(); err == nil {
		t.Fatal("expected error when no daemon binary is reachable")
	}
}

// resolveStateDir is a one-line helper but the env-driven branching
// lives in os.UserHomeDir; the happy-path cover suffices here.

func TestResolveStateDir_UnderHome(t *testing.T) {
	t.Setenv("HOME", "/test/home")
	got, err := resolveStateDir()
	if err != nil {
		t.Fatalf("resolveStateDir: %v", err)
	}
	want := "/test/home/.aileron"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// composeAgentEnv contract: empty endpointEnv or empty url leaves the
// agent's env untouched; non-empty pairs add the override.

func TestComposeAgentEnv_NoOverride(t *testing.T) {
	src := map[string]string{"FOO": "bar"}
	got := composeAgentEnv(src, "", "http://x")
	if v, ok := got["FOO"]; !ok || v != "bar" {
		t.Errorf("FOO not preserved: %+v", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("empty endpointEnv should not produce an entry")
	}
}

func TestComposeAgentEnv_EmptyURL(t *testing.T) {
	got := composeAgentEnv(nil, "ANTHROPIC_BASE_URL", "")
	if _, ok := got["ANTHROPIC_BASE_URL"]; ok {
		t.Error("empty URL should not produce an override entry")
	}
}

func TestComposeAgentEnv_SetsOverride(t *testing.T) {
	got := composeAgentEnv(map[string]string{"FOO": "bar"}, "ANTHROPIC_BASE_URL", "http://daemon:9999")
	if got["FOO"] != "bar" {
		t.Errorf("FOO lost: %+v", got)
	}
	if got["ANTHROPIC_BASE_URL"] != "http://daemon:9999" {
		t.Errorf("override missing: %+v", got)
	}
}

// httpStatusError is exercised end-to-end via the RegisterSession_NonOK
// and EndSession_NonOK_ReturnsStatusError tests in daemon_client_test.go;
// they pin both the status code and the daemon's structured
// error.code+message in the surfaced error string. No standalone test
// here — the formatter's only call site is via those two methods.
