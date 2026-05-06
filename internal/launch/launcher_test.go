package launch_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/launch"
)

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

func (a envAgent) Name() string            { return "test-env" }
func (a envAgent) BinaryNames() []string   { return []string{"env"} }
func (a envAgent) Args() []string          { return nil }
func (a envAgent) Env() map[string]string  { return a.extraEnv }
func (a envAgent) LLMEndpointEnv() string  { return "" }

func TestLaunch_EnvironmentSetup(t *testing.T) {
	// Capture the child's env by launching "env" and reading stdout.
	// We need to redirect stdout to a file since Launch connects to os.Stdout.
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")

	// Isolate HOME so InstallWrapper doesn't touch the real home directory.
	t.Setenv("HOME", dir)

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
	if !strings.Contains(envStr, "AILERON_AGENT=test-script") {
		t.Error("AILERON_AGENT not set in child env")
	}
	// CLAUDE_CODE_SHELL should NOT be set for non-Claude agents.
	// Only Claude sets it via its Env() method.
	if strings.Contains(envStr, "CLAUDE_CODE_SHELL=") {
		t.Error("CLAUDE_CODE_SHELL should not be set for non-Claude agents")
	}
}

func TestLaunch_AgentSpecificEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
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
	t.Setenv("HOME", t.TempDir())
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

func TestLaunch_EmptyShellFallback(t *testing.T) {
	// When SHELL is empty, buildEnv should default AILERON_REAL_SHELL to /bin/sh
	// (unless agent overrides it).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("SHELL", "")
	outFile := filepath.Join(dir, "env.txt")

	script := filepath.Join(dir, "capture-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	agent := scriptAgent{script: script}
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
	// With no agent override and empty SHELL, should fall back to /bin/sh
	if !strings.Contains(string(data), "AILERON_REAL_SHELL=/bin/sh") {
		t.Error("expected AILERON_REAL_SHELL=/bin/sh when SHELL is empty")
	}
}

func TestLaunch_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	outFile := filepath.Join(dir, "cwd.txt")

	script := filepath.Join(dir, "capture-cwd.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(dir, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       workDir,
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "workdir") {
		t.Errorf("expected working directory to be set, got %q", string(data))
	}
}

func TestLaunch_AgentEnvVarsFlowThrough(t *testing.T) {
	// Agent-specific env vars (from Env()) should appear in the child
	// process environment. This covers vars like CLAUDE_CODE_SHELL
	// which are now set by the agent, not the launcher.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	outFile := filepath.Join(dir, "env.txt")

	script := filepath.Join(dir, "capture-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	agent := scriptAgent{
		script:   script,
		extraEnv: map[string]string{"MY_AGENT_VAR": "test-value"},
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
	if !strings.Contains(string(data), "MY_AGENT_VAR=test-value") {
		t.Error("expected agent-specific env var to flow through to child process")
	}
	if !strings.Contains(string(data), "AILERON_AGENT=") {
		t.Error("expected AILERON_AGENT to be set")
	}
}

func TestInstallWrapper_BadHomeDir(t *testing.T) {
	// Point HOME at a read-only path to trigger the MkdirAll error.
	t.Setenv("HOME", "/dev/null")
	_, err := launch.InstallWrapper("/usr/local/bin/aileron-sh")
	if err == nil {
		t.Fatal("expected error when HOME is invalid")
	}
}

func TestInstallWrapper_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Create .aileron as a read-only directory so WriteFile fails
	aileronDir := filepath.Join(dir, ".aileron")
	if err := os.MkdirAll(aileronDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a file at the wrapper path that's not writable
	wrapperPath := filepath.Join(aileronDir, "bash")
	if err := os.WriteFile(wrapperPath, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Make directory read-only
	os.Chmod(aileronDir, 0o555)
	t.Cleanup(func() { os.Chmod(aileronDir, 0o755) })

	_, err := launch.InstallWrapper("/usr/local/bin/aileron-sh")
	if err == nil {
		t.Fatal("expected error when wrapper dir is read-only")
	}
}

func TestLaunch_EnvScrubbing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Set env vars that should be scrubbed.
	t.Setenv("AWS_SECRET_KEY", "supersecret")
	t.Setenv("MY_TOKEN", "tok123")
	t.Setenv("SAFE_VAR", "keepme")

	// Write an aileron.yaml with scrub config.
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
env:
  scrub:
    - "AWS_*"
    - "*_TOKEN"
  passthrough:
    - "SAFE_VAR"
`), 0o644)

	outFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "capture-env.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755)

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       dir,
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}

	data, _ := os.ReadFile(outFile)
	envStr := string(data)

	if strings.Contains(envStr, "AWS_SECRET_KEY") {
		t.Error("AWS_SECRET_KEY should have been scrubbed")
	}
	if strings.Contains(envStr, "MY_TOKEN") {
		t.Error("MY_TOKEN should have been scrubbed")
	}
	if !strings.Contains(envStr, "SAFE_VAR=keepme") {
		t.Error("SAFE_VAR should have been preserved (passthrough)")
	}
}

func TestLaunch_EnvScrubPassthroughBeatsScrub(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("HOME_DIR", "/home/user")

	// HOME_DIR matches HOME* scrub pattern but HOME is in passthrough.
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
env:
  scrub:
    - "HOME*"
  passthrough:
    - "HOME"
    - "HOME_DIR"
`), 0o644)

	outFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "capture-env.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755)

	agent := scriptAgent{script: script}
	launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       dir,
	})

	data, _ := os.ReadFile(outFile)
	envStr := string(data)

	// HOME_DIR is in passthrough, so passthrough beats scrub.
	if !strings.Contains(envStr, "HOME_DIR=/home/user") {
		t.Error("HOME_DIR should be preserved (passthrough beats scrub)")
	}
}

func TestResolveAuditLogFromCwd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	got := launch.ResolveAuditLogFromCwd()
	want := filepath.Join(dir, ".aileron", "audit")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrintSessionSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	// Write some entries.
	for _, e := range []struct{ cmd, disp string }{
		{"echo 1", "allow"},
		{"echo 2", "allow"},
		{"rm -rf /", "deny"},
		{"git push", "ask_approved"},
		{"curl bad", "ask_denied"},
	} {
		audit.AppendShellEntry(path, audit.ShellEntry{
			SessionID:   "test-session",
			Command:     e.cmd,
			Disposition: e.disp,
		})
	}

	var buf strings.Builder
	launch.PrintSessionSummary(&buf, path, "test-session")
	out := buf.String()

	if !strings.Contains(out, "2 command(s) allowed") {
		t.Errorf("expected 2 allowed, got:\n%s", out)
	}
	if !strings.Contains(out, "1 command(s) denied by policy") {
		t.Errorf("expected 1 denied, got:\n%s", out)
	}
	if !strings.Contains(out, "1 command(s) approved") {
		t.Errorf("expected 1 approved, got:\n%s", out)
	}
	if !strings.Contains(out, "1 command(s) denied by user") {
		t.Errorf("expected 1 user denied, got:\n%s", out)
	}
}

func TestPrintSessionSummary_NoEntries(t *testing.T) {
	var buf strings.Builder
	launch.PrintSessionSummary(&buf, "/nonexistent/audit.jsonl", "nope")
	if buf.Len() != 0 {
		t.Errorf("expected no output for missing log, got %q", buf.String())
	}
}

func TestPrintSessionSummary_EmptySession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "other", Command: "echo", Disposition: "allow"})

	var buf strings.Builder
	launch.PrintSessionSummary(&buf, path, "nonexistent-session")
	if buf.Len() != 0 {
		t.Errorf("expected no output for unmatched session, got %q", buf.String())
	}
}

func TestEnvGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		// Exact match.
		{"HOME", "HOME", true},
		{"HOME", "HOME_DIR", false},
		// Wildcard * matches everything.
		{"*", "ANYTHING", true},
		{"*", "", true},
		// Prefix match (AWS_*).
		{"AWS_*", "AWS_SECRET_KEY", true},
		{"AWS_*", "HOME", false},
		// Suffix match (*_SECRET).
		{"*_SECRET", "MY_SECRET", true},
		{"*_SECRET", "SECRET_KEY", false},
		// Contains match (*TOKEN*).
		{"*TOKEN*", "MY_TOKEN_VALUE", true},
		{"*TOKEN*", "TOKEN", true},
		{"*TOKEN*", "NOTOK", false},
		// No match.
		{"SHELL", "HOME", false},
	}
	for _, tt := range tests {
		got := launch.EnvGlobMatch(tt.pattern, tt.name)
		if got != tt.want {
			t.Errorf("EnvGlobMatch(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

// scriptAgent launches a specific script/binary directly.
type scriptAgent struct {
	script   string
	extraEnv map[string]string
}

func (a scriptAgent) Name() string                              { return "test-script" }
func (a scriptAgent) BinaryNames() []string                     { return []string{a.script} }
func (a scriptAgent) Args() []string                            { return nil }
func (a scriptAgent) Env() map[string]string                    { return a.extraEnv }
func (a scriptAgent) LLMEndpointEnv() string                    { return "" }
func (a scriptAgent) NormalizeCommand(raw string) (string, bool) { return raw, true }
func (a scriptAgent) ConfigureShell(_, _ string) error           { return nil }

func TestOpenSessionLogger_Success(t *testing.T) {
	dir := t.TempDir()
	logger, cleanup := launch.OpenSessionLogger(dir, slog.LevelDebug)
	defer cleanup()

	if logger == nil {
		t.Fatal("expected non-nil logger")
	}

	// Verify the log file was created.
	logPath := launch.SessionLogPath(dir)
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("session log file not created at %s: %v", logPath, err)
	}

	// Write a message and verify it appears in the file.
	logger.Info("test message", "key", "value")
	cleanup() // flush and close

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "test message") {
		t.Errorf("log file should contain 'test message', got: %s", content)
	}
	if !strings.Contains(content, "key=value") {
		t.Errorf("log file should contain 'key=value', got: %s", content)
	}
}

func TestOpenSessionLogger_LevelFiltering(t *testing.T) {
	dir := t.TempDir()
	logger, cleanup := launch.OpenSessionLogger(dir, slog.LevelWarn)
	defer cleanup()

	// Debug message should be filtered out at warn level.
	logger.Debug("should not appear")
	logger.Warn("should appear")
	cleanup()

	data, err := os.ReadFile(launch.SessionLogPath(dir))
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "should not appear") {
		t.Error("debug message should be filtered at warn level")
	}
	if !strings.Contains(content, "should appear") {
		t.Error("warn message should be present")
	}
}

func TestOpenSessionLogger_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	readOnly := filepath.Join(dir, "readonly")
	if err := os.MkdirAll(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	// Use a subpath under the read-only dir so MkdirAll fails.
	badDir := filepath.Join(readOnly, "sub", "deep")
	logger, cleanup := launch.OpenSessionLogger(badDir, slog.LevelDebug)
	defer cleanup()

	if logger == nil {
		t.Fatal("expected non-nil discard logger, got nil")
	}
	// Logger should still be usable (writes to discard).
	logger.Info("this should not panic")
}

func TestSessionLogPath_WithPolicyFile(t *testing.T) {
	dir := t.TempDir()
	// Create an aileron.yaml to simulate a policy file.
	if err := os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := launch.SessionLogPath(dir)
	want := filepath.Join(dir, ".aileron", "session.log")
	if got != want {
		t.Errorf("SessionLogPath = %q, want %q", got, want)
	}
}

func TestSessionLogPath_WithoutPolicyFile(t *testing.T) {
	dir := t.TempDir()
	got := launch.SessionLogPath(dir)
	want := filepath.Join(dir, ".aileron", "session.log")
	if got != want {
		t.Errorf("SessionLogPath = %q, want %q", got, want)
	}
}
