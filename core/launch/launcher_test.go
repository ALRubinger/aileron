package launch_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
	"github.com/creack/pty/v2"
)

func TestResolveBinary_Found(t *testing.T) {
	// "echo" should be on every Unix PATH
	path, err := launch.ResolveBinary([]string{"echo"})
	if err != nil {
		t.Fatalf("expected to find 'echo': %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("expected absolute path, got %q", path)
	}
}

func TestResolveBinary_FallsBackToSecondCandidate(t *testing.T) {
	path, err := launch.ResolveBinary([]string{"nonexistent-xyz-1234", "echo"})
	if err != nil {
		t.Fatalf("expected to find 'echo' as fallback: %v", err)
	}
	if !strings.HasSuffix(path, "echo") {
		t.Errorf("expected path ending in 'echo', got %q", path)
	}
}

func TestResolveBinary_NotFound(t *testing.T) {
	_, err := launch.ResolveBinary([]string{"nonexistent-binary-xyz-9999"})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestResolveShim_NextToSelf(t *testing.T) {
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "aileron-sh")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfPath := filepath.Join(dir, "aileron")

	resolved, err := launch.ResolveShim(selfPath)
	if err != nil {
		t.Fatalf("expected to find shim next to self: %v", err)
	}
	if resolved != shimPath {
		t.Errorf("expected %q, got %q", shimPath, resolved)
	}
}

func TestResolveShim_NotFound(t *testing.T) {
	_, err := launch.ResolveShim("/nonexistent/dir/aileron")
	if err == nil {
		t.Fatal("expected error when shim not found")
	}
}

// envAgent is a test agent that launches "env" to print the environment.
type envAgent struct {
	extraEnv map[string]string
}

func (a envAgent) Name() string           { return "test-env" }
func (a envAgent) BinaryNames() []string  { return []string{"env"} }
func (a envAgent) Env() map[string]string { return a.extraEnv }

func TestLaunch_EnvironmentSetup(t *testing.T) {
	// Capture the child's env by launching "env" and reading stdout.
	// We need to redirect stdout to a file since Launch connects to os.Stdout.
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")

	// Create a wrapper script that runs env and writes to file
	script := filepath.Join(dir, "capture-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	shimPath := "/tmp/fake-aileron-sh"
	agent := scriptAgent{script: script}

	result, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: shimPath,
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading env output: %v", err)
	}
	envStr := string(data)

	if !strings.Contains(envStr, "SHELL="+shimPath) {
		t.Error("SHELL not set to shim path in child env")
	}
	if !strings.Contains(envStr, "AILERON_REAL_SHELL=") {
		t.Error("AILERON_REAL_SHELL not set in child env")
	}
}

func TestLaunch_AgentSpecificEnv(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")

	script := filepath.Join(dir, "capture-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	agent := scriptAgent{
		script:   script,
		extraEnv: map[string]string{"CUSTOM_VAR": "hello"},
	}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "CUSTOM_VAR=hello") {
		t.Error("agent-specific env var not set in child env")
	}
}

func TestLaunch_ExitCodePropagation(t *testing.T) {
	agent := scriptAgent{script: "/bin/sh"}
	result, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Args:      []string{"-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", result.ExitCode)
	}
}

func TestLaunch_BinaryNotFound(t *testing.T) {
	agent := testAgent{name: "nonexistent-binary-xyz"}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestComputeAgentRows(t *testing.T) {
	tests := []struct {
		totalRows, barHeight, want int
	}{
		{24, 2, 22},
		{10, 2, 8},
		{3, 2, 1},
		{2, 2, 1}, // clamped to 1
		{1, 2, 1}, // clamped to 1
	}
	for _, tt := range tests {
		got := launch.ComputeAgentRows(tt.totalRows, tt.barHeight)
		if got != tt.want {
			t.Errorf("ComputeAgentRows(%d, %d) = %d, want %d", tt.totalRows, tt.barHeight, got, tt.want)
		}
	}
}

func TestSetupTerminalScreen(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	var buf strings.Builder
	launch.SetupTerminalScreen(&buf, 22, bar)
	out := buf.String()

	// Should clear screen
	if !strings.Contains(out, "\033[2J") {
		t.Error("expected clear screen escape")
	}
	// Should set scroll region
	if !strings.Contains(out, "\033[1;22r") {
		t.Error("expected scroll region escape")
	}
	// Should contain status bar content
	if !strings.Contains(out, "test") {
		t.Error("expected status bar text")
	}
}

func TestHandleResize(t *testing.T) {
	// Create a real pty pair so we have valid file descriptors.
	ptmx, pts, err := pty.Open()
	if err != nil {
		t.Fatalf("failed to open pty: %v", err)
	}
	defer ptmx.Close()
	defer pts.Close()

	bar := launch.NewStatusBar(24, 80, "test")
	var buf strings.Builder

	// HandleResize reads the terminal size from the fd. Using the pty master
	// fd, it will get the pty's current size and update accordingly.
	launch.HandleResize(&buf, int(ptmx.Fd()), ptmx, bar)

	out := buf.String()
	// Should have written scroll region and bar resize output
	if len(out) == 0 {
		t.Error("expected HandleResize to produce output")
	}
}

func TestCleanupTerminalScreen(t *testing.T) {
	var buf strings.Builder
	launch.CleanupTerminalScreen(&buf, 24)
	out := buf.String()

	// Should reset scroll region
	if !strings.Contains(out, "\033[r") {
		t.Error("expected reset scroll region")
	}
	// Should move to bar area and clear
	if !strings.Contains(out, "\033[23;1H\033[J") {
		t.Errorf("expected clear at row 23, got %q", out)
	}
}

// scriptAgent launches a specific script/binary directly.
type scriptAgent struct {
	script   string
	extraEnv map[string]string
}

func (a scriptAgent) Name() string           { return "test-script" }
func (a scriptAgent) BinaryNames() []string  { return []string{a.script} }
func (a scriptAgent) Env() map[string]string { return a.extraEnv }
