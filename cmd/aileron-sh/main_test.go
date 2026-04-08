package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShimPassthrough verifies commands pass through without policy.
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

// TestShimPolicyAllow verifies that commands matching allow rules pass through.
func TestShimPolicyAllow(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: deny
allow:
  - "echo *"
`)

	cmd := exec.Command(binary, "-c", "echo policy-allowed")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expected allowed command to succeed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "policy-allowed" {
		t.Errorf("expected 'policy-allowed', got %q", got)
	}
}

// TestShimPolicyDeny verifies that commands matching deny rules are blocked.
func TestShimPolicyDeny(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)

	cmd := exec.Command(binary, "-c", "rm -rf /something")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected denied command to fail")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("expected exit code 1, got %d", exitErr.ExitCode())
	}
	if !strings.Contains(string(out), "denied") {
		t.Errorf("expected 'denied' in output, got %q", string(out))
	}
	if !strings.Contains(string(out), "no recursive delete") {
		t.Errorf("expected deny reason in output, got %q", string(out))
	}
}

// TestShimPolicyAskDeniesWithoutTTY verifies that ask commands default to
// deny when there is no controlling terminal.
func TestShimPolicyAskDeniesWithoutTTY(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: ask
`)

	cmd := exec.Command(binary, "-c", "git push origin main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected ask command to fail without tty")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("expected exit code 1, got %d", exitErr.ExitCode())
	}
}

// TestShimPolicyDefaultAllow verifies that unmatched commands are allowed
// when default is "allow".
func TestShimPolicyDefaultAllow(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
`)

	cmd := exec.Command(binary, "-c", "echo anything-goes")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expected default-allow to succeed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "anything-goes" {
		t.Errorf("expected 'anything-goes', got %q", got)
	}
}

// TestShimPolicyDenyOverridesAllow verifies that deny rules win over allow
// rules (higher priority).
func TestShimPolicyDenyOverridesAllow(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
allow:
  - "git *"
deny:
  - command: "git push origin main"
    description: "no push to main"
`)

	cmd := exec.Command(binary, "-c", "git push origin main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected deny to override allow")
	}
}

// TestShimScriptModePassthrough verifies that script mode (no -c) passes
// through without policy evaluation.
func TestShimScriptModePassthrough(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: deny
`)

	// Write a script that would be denied in -c mode.
	script := filepath.Join(dir, "test.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho script-passthrough\n"), 0o755)

	cmd := exec.Command(binary, script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expected script mode to pass through: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "script-passthrough" {
		t.Errorf("expected 'script-passthrough', got %q", got)
	}
}

// TestShimNoPolicyFile verifies the shim passes through when no aileron.yaml
// exists — the developer hasn't opted into policy enforcement.
func TestShimNoPolicyFile(t *testing.T) {
	binary := buildShim(t)
	dir := t.TempDir() // no aileron.yaml

	cmd := exec.Command(binary, "-c", "echo no-policy-passthrough")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expected passthrough without policy file: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "no-policy-passthrough" {
		t.Errorf("expected 'no-policy-passthrough', got %q", got)
	}
}

// TestShimLoginFlag verifies that -c -l "command" works correctly.
// Claude Code spawns: shell -c -l "command" (login flag between -c and
// the command string).
func TestShimLoginFlag(t *testing.T) {
	binary := buildShim(t)

	cmd := exec.Command(binary, "-c", "-l", "echo login-flag-works")
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shim with -c -l failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "login-flag-works" {
		t.Errorf("expected 'login-flag-works', got %q", got)
	}
}

// TestShimLoginFlagPolicyAllow verifies that policy evaluation works
// correctly when -l flag is present between -c and the command.
func TestShimLoginFlagPolicyAllow(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: deny
allow:
  - "echo *"
`)

	cmd := exec.Command(binary, "-c", "-l", "echo allowed-with-login")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expected allowed command with -l to succeed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "allowed-with-login" {
		t.Errorf("expected 'allowed-with-login', got %q", got)
	}
}

// TestShimLoginFlagPolicyDeny verifies that deny rules still fire
// when -l flag is present.
func TestShimLoginFlagPolicyDeny(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)

	cmd := exec.Command(binary, "-c", "-l", "rm -rf /something")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected denied command with -l to fail")
	}
}

// TestShimUnwrapsClaudeCodeTemplate verifies that policy evaluation works
// against the inner command when Claude Code wraps it in its template.
func TestShimUnwrapsClaudeCodeTemplate(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)

	// Simulate the exact wrapper Claude Code sends.
	wrapped := "shopt -u extglob 2>/dev/null || true && eval 'rm -rf /tmp/test' < /dev/null && pwd -P >| /tmp/claude-test-cwd"
	cmd := exec.Command(binary, "-c", "-l", wrapped)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash", "AILERON_AGENT=claude")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected deny for rm -rf inside Claude Code wrapper")
	}
	if !strings.Contains(string(out), "denied") {
		t.Errorf("expected 'denied' in output, got %q", string(out))
	}
	if !strings.Contains(string(out), "no recursive delete") {
		t.Errorf("expected deny reason in output, got %q", string(out))
	}
}

// TestShimUnwrapsClaudeCodeTemplateAllow verifies allowed commands pass
// through the Claude Code wrapper.
func TestShimUnwrapsClaudeCodeTemplateAllow(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: deny
allow:
  - "echo *"
`)

	wrapped := "shopt -u extglob 2>/dev/null || true && eval 'echo unwrapped-ok' < /dev/null && pwd -P >| /tmp/claude-test-cwd"
	cmd := exec.Command(binary, "-c", "-l", wrapped)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash", "AILERON_AGENT=claude")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expected allowed command inside wrapper to succeed: %v", err)
	}
	if !strings.Contains(string(out), "unwrapped-ok") {
		t.Errorf("expected 'unwrapped-ok' in output, got %q", string(out))
	}
}

// TestShimNoUnwrapForOtherAgents verifies that the eval unwrap only
// applies when AILERON_AGENT=claude, not for other agents.
func TestShimNoUnwrapForOtherAgents(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: deny
allow:
  - "echo *"
`)

	// Same wrapped command, but agent is not claude — should NOT unwrap,
	// so "echo unwrapped-ok" won't be extracted and the full command
	// starting with "shopt" won't match "echo *" → denied.
	wrapped := "shopt -u extglob 2>/dev/null || true && eval 'echo unwrapped-ok' < /dev/null && pwd -P >| /tmp/claude-test-cwd"
	cmd := exec.Command(binary, "-c", wrapped)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash", "AILERON_AGENT=other")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-claude agent to NOT unwrap eval — full command should be denied")
	}
}

// TestShimUnwrapPreservesPlainCommands verifies that commands without the
// Claude Code wrapper are evaluated as-is.
func TestShimUnwrapPreservesPlainCommands(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)

	cmd := exec.Command(binary, "-c", "rm -rf /something")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected deny for plain rm -rf command")
	}
}

// writePolicyDir creates a temp directory with an aileron.yaml and returns
// the directory path.
func writePolicyDir(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
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
