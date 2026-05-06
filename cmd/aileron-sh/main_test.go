package main_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
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
// Uses a relative path so the project deny (rm -rf *) matches but the
// built-in deny (rm -rf /*) does not, exercising the project rule directly.
func TestShimPolicyDeny(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)

	cmd := exec.Command(binary, "-c", "rm -rf something")
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
	if !strings.Contains(string(out), "Denied") {
		t.Errorf("expected 'denied' in output, got %q", string(out))
	}
	if !strings.Contains(string(out), "no recursive delete") {
		t.Errorf("expected deny reason in output, got %q", string(out))
	}
}

// TestShimPolicyAskDeniesWithoutTTY verifies that ask commands default to
// deny when there is no controlling terminal. Uses "docker" because it is
// not on the built-in allow list, so default:ask actually applies.
func TestShimPolicyAskDeniesWithoutTTY(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: ask
`)

	cmd := exec.Command(binary, "-c", "docker run myimage")
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

	// Simulate the exact wrapper Claude Code sends. Inner command uses a
	// relative path so the project deny ("rm -rf *") matches and the built-in
	// deny ("rm -rf /*") does not.
	wrapped := "shopt -u extglob 2>/dev/null || true && eval 'rm -rf tmp/test' < /dev/null && pwd -P >| /tmp/claude-test-cwd"
	cmd := exec.Command(binary, "-c", "-l", wrapped)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash", "AILERON_AGENT=claude")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected deny for rm -rf inside Claude Code wrapper")
	}
	if !strings.Contains(string(out), "Denied") {
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

// TestShimDenyStderrShortMessage verifies that the deny writes a short
// "aileron: denied" to stderr for the agent, plus the detailed message
// (which falls back to stderr when no tty is available).
func TestShimDenyStderrShortMessage(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)

	cmd := exec.Command(binary, "-c", "rm -rf something")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/sh")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected denied command to fail")
	}
	out := stderr.String()

	if !strings.Contains(out, "Aileron] Denied") {
		t.Errorf("expected denial message in stderr, got %q", out)
	}
	if !strings.Contains(out, "no recursive delete") {
		t.Errorf("expected deny reason in stderr, got %q", out)
	}
}

// TestShimDenyAuditEntry verifies that a deny writes an audit entry.
func TestShimDenyAuditEntry(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`)

	stateDir := dir
	auditPath := audit.DailyPath(stateDir)
	cmd := exec.Command(binary, "-c", "rm -rf /something")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"AILERON_REAL_SHELL=/bin/sh",
		"AILERON_AUDIT_DIR="+stateDir,
		"AILERON_SESSION_ID=test-session",
		"AILERON_AGENT=test",
	)
	cmd.Run() // ignore error (expected deny)

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("expected audit log at %s: %v", auditPath, err)
	}
	content := string(data)
	if !strings.Contains(content, `"disposition":"deny"`) {
		t.Errorf("expected deny disposition in audit, got %q", content)
	}
	if !strings.Contains(content, `"session_id":"test-session"`) {
		t.Errorf("expected session ID in audit, got %q", content)
	}
}

// TestShimAskDeniesWithoutTTYAudit verifies that ask commands that
// auto-deny (no tty) write an audit entry. Uses "docker" because it is
// not on the built-in allow list, so default:ask actually applies.
func TestShimAskDeniesWithoutTTYAudit(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: ask
`)

	stateDir := dir
	auditPath := audit.DailyPath(stateDir)
	cmd := exec.Command(binary, "-c", "docker run myimage")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"AILERON_REAL_SHELL=/bin/sh",
		"AILERON_AUDIT_DIR="+stateDir,
		"AILERON_SESSION_ID=test-session",
	)
	cmd.Run()

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("expected audit log: %v", err)
	}
	if !strings.Contains(string(data), `"disposition":"ask_denied"`) {
		t.Errorf("expected ask_denied in audit, got %q", string(data))
	}
}

// TestShimAllowAuditEntry verifies that allowed commands write an audit entry.
func TestShimAllowAuditEntry(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: deny
allow:
  - "echo *"
`)

	stateDir := dir
	auditPath := audit.DailyPath(stateDir)
	cmd := exec.Command(binary, "-c", "echo audit-test")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"AILERON_REAL_SHELL=/bin/sh",
		"AILERON_AUDIT_DIR="+stateDir,
		"AILERON_SESSION_ID=test-session",
		"AILERON_AGENT=test",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expected allowed command to succeed: %v", err)
	}
	if !strings.Contains(string(out), "audit-test") {
		t.Errorf("expected 'audit-test' in output, got %q", out)
	}

	data, _ := os.ReadFile(auditPath)
	if !strings.Contains(string(data), `"disposition":"allow"`) {
		t.Errorf("expected allow disposition in audit, got %q", string(data))
	}
}

// TestShimClaudeInfrastructurePassthrough verifies that Claude Code
// infrastructure commands (no eval wrapper) pass through without
// policy evaluation, even with default: deny.
func TestShimClaudeInfrastructurePassthrough(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: deny
`)

	// This simulates a Claude Code snapshot script — no eval wrapper.
	infraCmd := `SNAPSHOT_FILE=/tmp/test.sh; echo "# snapshot" > "$SNAPSHOT_FILE"`
	cmd := exec.Command(binary, "-c", infraCmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash", "AILERON_AGENT=claude")
	err := cmd.Run()
	if err != nil {
		t.Fatalf("infrastructure command should pass through even with default:deny, got: %v", err)
	}
}

// TestShimClaudeUnquotedEval verifies that unquoted eval commands
// (eval ls \< /dev/null) are unwrapped correctly.
func TestShimClaudeUnquotedEval(t *testing.T) {
	binary := buildShim(t)
	dir := writePolicyDir(t, `
version: 1
default: deny
allow:
  - "echo *"
`)

	wrapped := `shopt -u extglob 2>/dev/null || true && eval echo hello-unquoted \< /dev/null && pwd -P >| /tmp/claude-test-cwd`
	cmd := exec.Command(binary, "-c", "-l", wrapped)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AILERON_REAL_SHELL=/bin/bash", "AILERON_AGENT=claude")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expected allowed unquoted eval command to succeed: %v", err)
	}
	if !strings.Contains(string(out), "hello-unquoted") {
		t.Errorf("expected 'hello-unquoted' in output, got %q", string(out))
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
