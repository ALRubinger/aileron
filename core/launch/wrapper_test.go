package launch_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestInstallWrapper_CreatesExecutableScript(t *testing.T) {
	// Override HOME so we don't write to the real home directory.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	shimPath := "/usr/local/bin/aileron-sh"
	wrapperPath, err := launch.InstallWrapper(shimPath)
	if err != nil {
		t.Fatalf("InstallWrapper failed: %v", err)
	}

	// Path should end with .aileron/bash
	if !strings.HasSuffix(wrapperPath, filepath.Join(".aileron", "bash")) {
		t.Errorf("expected path ending in .aileron/bash, got %q", wrapperPath)
	}

	// Path must contain "bash" (the whole point of this approach)
	if !strings.Contains(wrapperPath, "bash") {
		t.Errorf("wrapper path must contain 'bash', got %q", wrapperPath)
	}

	// File should exist and be executable
	info, err := os.Stat(wrapperPath)
	if err != nil {
		t.Fatalf("wrapper not found at %s: %v", wrapperPath, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("wrapper script is not executable")
	}

	// Content should reference the shim path
	content, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("reading wrapper: %v", err)
	}
	if !strings.Contains(string(content), shimPath) {
		t.Errorf("wrapper content should reference shim %q, got:\n%s", shimPath, content)
	}
	if !strings.HasPrefix(string(content), "#!/bin/bash") {
		t.Error("wrapper should start with #!/bin/bash shebang")
	}
}

func TestInstallWrapper_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	shimPath := "/usr/local/bin/aileron-sh"

	path1, err := launch.InstallWrapper(shimPath)
	if err != nil {
		t.Fatalf("first install failed: %v", err)
	}

	path2, err := launch.InstallWrapper(shimPath)
	if err != nil {
		t.Fatalf("second install failed: %v", err)
	}

	if path1 != path2 {
		t.Errorf("paths differ: %q vs %q", path1, path2)
	}
}

func TestInstallWrapper_UpdatesOnShimChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path, err := launch.InstallWrapper("/old/aileron-sh")
	if err != nil {
		t.Fatal(err)
	}

	_, err = launch.InstallWrapper("/new/aileron-sh")
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "/new/aileron-sh") {
		t.Error("wrapper should reference the new shim path after update")
	}
	if strings.Contains(string(content), "/old/aileron-sh") {
		t.Error("wrapper should no longer reference the old shim path")
	}
}

func TestInstallWrapper_ScriptExecutes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Use "echo" as the shim stand-in — the wrapper will exec echo with args
	echoPath, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo not found on PATH")
	}

	wrapperPath, err := launch.InstallWrapper(echoPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(wrapperPath, "hello", "from", "wrapper")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("wrapper execution failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello from wrapper" {
		t.Errorf("expected 'hello from wrapper', got %q", got)
	}
}
