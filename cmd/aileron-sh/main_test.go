package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShimPassthrough builds aileron-sh and verifies it passes commands
// through to the real shell.
func TestShimPassthrough(t *testing.T) {
	binary := buildShim(t)

	cmd := exec.Command(binary, "-c", "echo hello-from-shim")
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shim execution failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello-from-shim" {
		t.Errorf("expected 'hello-from-shim', got %q", got)
	}
}

// TestShimExitCode verifies the shim propagates the real shell's exit code.
func TestShimExitCode(t *testing.T) {
	binary := buildShim(t)

	cmd := exec.Command(binary, "-c", "exit 42")
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 42 {
		t.Errorf("expected exit code 42, got %d", exitErr.ExitCode())
	}
}

// TestShimFallbackToSh verifies the shim defaults to /bin/sh when
// AILERON_REAL_SHELL is not set.
func TestShimFallbackToSh(t *testing.T) {
	binary := buildShim(t)

	// Build env without AILERON_REAL_SHELL
	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "AILERON_REAL_SHELL=") {
			env = append(env, e)
		}
	}

	cmd := exec.Command(binary, "-c", "echo fallback-works")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shim fallback failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "fallback-works" {
		t.Errorf("expected 'fallback-works', got %q", got)
	}
}

// TestShimInvalidShell verifies error handling when the real shell doesn't exist.
func TestShimInvalidShell(t *testing.T) {
	binary := buildShim(t)

	cmd := exec.Command(binary, "-c", "echo nope")
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/nonexistent/shell")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected error for invalid shell")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 127 {
		t.Errorf("expected exit code 127, got %d", exitErr.ExitCode())
	}
}

// buildShim compiles the aileron-sh binary and returns its path.
func buildShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "aileron-sh")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Dir = filepath.Join(findModuleRoot(t), "cmd", "aileron-sh")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build aileron-sh: %v\n%s", err, out)
	}
	return binary
}

// findModuleRoot walks up from the test file to find the repo root.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.work found)")
		}
		dir = parent
	}
}
