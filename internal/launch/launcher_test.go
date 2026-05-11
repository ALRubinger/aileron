package launch_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
)

func TestResolveBinary_Found(t *testing.T) {
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

// scriptAgent launches a specific script/binary directly. Used to
// exercise Launch's environment + arg flow against a real subprocess
// without requiring a real coding-agent binary.
type scriptAgent struct {
	script   string
	extraEnv map[string]string
	mcpArgs  []string
}

func (a scriptAgent) Name() string                                  { return "test-script" }
func (a scriptAgent) BinaryNames() []string                         { return []string{a.script} }
func (a scriptAgent) Args() []string                                { return nil }
func (a scriptAgent) Env() map[string]string                        { return a.extraEnv }
func (a scriptAgent) LLMEndpointEnv() string                        { return "" }
func (a scriptAgent) ConfigureMCP(string, map[string]string, string) ([]string, error) {
	return a.mcpArgs, nil
}

func TestLaunch_AgentEnvVarsFlowThrough(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "capture.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := scriptAgent{script: script, extraEnv: map[string]string{
		"CUSTOM_VAR": "hello",
	}}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{Agent: agent})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !strings.Contains(string(data), "CUSTOM_VAR=hello") {
		t.Errorf("CUSTOM_VAR not propagated to child env")
	}
}

func TestLaunch_ShimEnvVarsNotInjected(t *testing.T) {
	// ADR-0015: launch no longer sets SHELL=<shim>, AILERON_REAL_SHELL,
	// AILERON_AGENT, or AILERON_AUDIT_DIR. The child inherits the
	// parent's $SHELL untouched.
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "capture.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", "/bin/zsh")

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: scriptAgent{script: script},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, _ := os.ReadFile(outFile)
	env := string(data)
	if !strings.Contains(env, "SHELL=/bin/zsh") {
		t.Errorf("expected SHELL=/bin/zsh to flow through; got:\n%s", env)
	}
	for _, key := range []string{"AILERON_REAL_SHELL=", "AILERON_AGENT=", "AILERON_AUDIT_DIR="} {
		if strings.Contains(env, key) {
			t.Errorf("expected %q to not be set in child env (per ADR-0015):\n%s", key, env)
		}
	}
}

func TestLaunch_ExitCodePropagation(t *testing.T) {
	agent := scriptAgent{script: "/bin/sh"}
	result, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: agent,
		Args:  []string{"-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", result.ExitCode)
	}
}

func TestLaunch_BinaryNotFound(t *testing.T) {
	agent := scriptAgent{script: "nonexistent-binary-xyz-9999"}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: agent,
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestLaunch_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(dir, "pwd.txt")
	script := filepath.Join(dir, "pwd.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: scriptAgent{script: script},
		Dir:   workDir,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	got, _ := os.ReadFile(outFile)
	if !strings.Contains(string(got), workDir) {
		t.Errorf("child pwd = %q, want it under %q", string(got), workDir)
	}
}

func TestLaunch_AgentMCPArgs_Appended(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "args.txt")
	// Capture argv as one line per argument so we can assert presence.
	script := filepath.Join(dir, "args.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: scriptAgent{
			script:  script,
			mcpArgs: []string{"--mcp-flag", "mcp-value"},
		},
		Args: []string{"user-arg"},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, _ := os.ReadFile(outFile)
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"user-arg", "--mcp-flag", "mcp-value"}
	for _, w := range want {
		found := false
		for _, a := range args {
			if a == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected arg %q in child argv, got %v", w, args)
		}
	}
}

func TestSessionLogPath_UnderCWD(t *testing.T) {
	dir := t.TempDir()
	got := launch.SessionLogPath(dir)
	want := filepath.Join(dir, ".aileron", "session.log")
	if got != want {
		t.Errorf("SessionLogPath = %q, want %q", got, want)
	}
}

func TestSessionLogPath_EmptyDirFallsBackToCWD(t *testing.T) {
	got := launch.SessionLogPath("")
	cwd, _ := os.Getwd()
	want := filepath.Join(cwd, ".aileron", "session.log")
	if got != want {
		t.Errorf("SessionLogPath(\"\") = %q, want %q", got, want)
	}
}
