package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func newTestRegistry() *launch.Registry {
	r := launch.NewRegistry()
	r.Register(agents.Claude{})
	return r
}

func TestRun_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stdout.String(), "aileron launch") {
		t.Error("expected usage in stdout")
	}
}

func TestRun_Help(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{arg}, newTestRegistry(), &stdout, &stderr)
		if code != 0 {
			t.Errorf("help (%s): expected exit code 0, got %d", arg, code)
		}
		if !strings.Contains(stdout.String(), "aileron launch") {
			t.Errorf("help (%s): expected usage output", arg)
		}
	}
}

func TestRun_Version(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{arg}, newTestRegistry(), &stdout, &stderr)
		if code != 0 {
			t.Errorf("version (%s): expected exit code 0, got %d", arg, code)
		}
		if !strings.Contains(stdout.String(), "aileron") {
			t.Errorf("version (%s): expected version output", arg)
		}
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown command: "bogus"`) {
		t.Errorf("expected unknown command error, got %q", stderr.String())
	}
}

func TestRun_LaunchNoAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron launch") {
		t.Error("expected launch usage in stderr")
	}
}

func TestRun_LaunchValidAgentNoShim(t *testing.T) {
	// "claude" is a valid agent, but aileron-sh won't be found next to the
	// test binary or on PATH (in CI), so this exercises the shim error path.
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "claude"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("expected error about shim resolution, got %q", stderr.String())
	}
}

func TestRun_LaunchUnknownAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "bogus-agent"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown agent") {
		t.Error("expected unknown agent error in stderr")
	}
	if !strings.Contains(stderr.String(), "claude") {
		t.Error("expected available agents list in stderr")
	}
}

func TestRun_LaunchLogLevelFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// --log-level is parsed before the agent name; exercises the flag path.
	code := run([]string{"launch", "--log-level=debug", "claude"}, newTestRegistry(), &stdout, &stderr)
	// Will fail at shim resolution, but the flag parsing should succeed.
	if code != 1 {
		t.Errorf("expected exit code 1 (shim not found), got %d", code)
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("expected shim error, got %q", stderr.String())
	}
}

func TestRun_LaunchLogLevelNoAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "--log-level=debug"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron launch") {
		t.Error("expected launch usage in stderr")
	}
}

func TestRunInit_CreatesFile(t *testing.T) {
	dir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"init"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "aileron.yaml") {
		t.Errorf("expected file creation message, got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "built in") {
		t.Errorf("expected built-in defaults message, got: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "aileron.yaml")); err != nil {
		t.Error("aileron.yaml was not created")
	}
}

func TestRunInit_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1"), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"init"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %s", stderr.String())
	}
}

func TestRunInit_OutputMessage(t *testing.T) {
	dir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"init"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	// Should explain that defaults are built in.
	if !strings.Contains(stdout.String(), "built in") {
		t.Errorf("expected built-in message, got: %s", stdout.String())
	}
}

func TestRunInit_InHelp(t *testing.T) {
	var stdout bytes.Buffer
	run([]string{"help"}, newTestRegistry(), &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "aileron init") {
		t.Error("expected 'aileron init' in help output")
	}
}

func TestRunLog_WithEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(path, audit.ShellEntry{
		SessionID: "s1", Command: "echo hello", Disposition: "allow", RuleID: "allow_0",
	})
	audit.AppendShellEntry(path, audit.ShellEntry{
		SessionID: "s1", Command: "rm -rf /", Disposition: "deny", RuleID: "deny_0",
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"log", "--path", path}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "echo hello") {
		t.Errorf("expected 'echo hello' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "deny") {
		t.Errorf("expected 'deny' in output, got:\n%s", out)
	}
}

func TestRunLog_FilterByDisposition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(path, audit.ShellEntry{
		SessionID: "s1", Command: "echo hello", Disposition: "allow",
	})
	audit.AppendShellEntry(path, audit.ShellEntry{
		SessionID: "s1", Command: "rm -rf /", Disposition: "deny",
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"log", "--path", path, "--disposition", "deny"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if strings.Contains(out, "echo hello") {
		t.Error("should not show allow entries when filtering by deny")
	}
	if !strings.Contains(out, "rm -rf") {
		t.Error("expected denied entry in output")
	}
}

func TestRunLog_NoEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	os.WriteFile(path, nil, 0o644)

	var stdout, stderr bytes.Buffer
	code := run([]string{"log", "--path", path}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "No audit entries") {
		t.Error("expected 'No audit entries' message")
	}
}

func TestRunLog_MissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"log", "--path", "/nonexistent/audit.jsonl"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

func TestRunPolicyTest_Allow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: deny
allow:
  - "echo *"
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "test", "echo hello"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0 for allowed command, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "allow") {
		t.Errorf("expected 'allow' in output, got:\n%s", stdout.String())
	}
}

func TestRunPolicyTest_Deny(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "test", "rm -rf /tmp"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 for denied command, got %d", code)
	}
	if !strings.Contains(stdout.String(), "deny") {
		t.Errorf("expected 'deny' in output, got:\n%s", stdout.String())
	}
}

func TestRunPolicyTest_MultipleCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: ask
allow:
  - "echo *"
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "test", "echo hello", "curl evil.com"}, newTestRegistry(), &stdout, &stderr)
	_ = code // mixed results
	out := stdout.String()
	if !strings.Contains(out, "allow") {
		t.Error("expected allow for echo")
	}
	if !strings.Contains(out, "ask") {
		t.Error("expected ask for curl")
	}
}

func TestRunPolicyTest_NoPolicyFile(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "test", "echo hello"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "no aileron.yaml") {
		t.Errorf("expected 'no aileron.yaml' error, got: %s", stderr.String())
	}
}

func TestRunPolicyTest_NoCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "test"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 for no commands, got %d", code)
	}
}

func TestRunPolicyTest_InHelp(t *testing.T) {
	var stdout bytes.Buffer
	run([]string{"help"}, newTestRegistry(), &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "aileron policy test") {
		t.Error("expected 'aileron policy test' in help output")
	}
}

func TestRunStatus_All(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Policy") {
		t.Error("expected Policy section")
	}
	if !strings.Contains(out, "Environment") {
		t.Error("expected Environment section")
	}
	if !strings.Contains(out, "Notifications") {
		t.Error("expected Notifications section")
	}
	if !strings.Contains(out, "Vault") {
		t.Error("expected Vault section")
	}
	if !strings.Contains(out, "Built-in defaults") {
		t.Error("expected built-in defaults count")
	}
}

func TestRunStatus_Policy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: ask
deny:
  - command: "deploy --force *"
    description: "no force deploy"
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "policy"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "aileron.yaml") {
		t.Error("expected project policy path")
	}
	if !strings.Contains(out, "1 deny") {
		t.Error("expected project deny count")
	}
}

func TestRunStatus_Env(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
env:
  scrub:
    - "AWS_*"
  passthrough:
    - "HOME"
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "env"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "AWS_*") {
		t.Error("expected scrub pattern")
	}
	if !strings.Contains(out, "HOME") {
		t.Error("expected passthrough pattern")
	}
}

func TestRunStatus_Notifications(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
notifications:
  slack:
    app_token: vault:slack_app
    bot_token: vault:slack_bot
    channels:
      - name: "#backend"
        show: all
        auto_draft: true
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "notifications"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Slack") {
		t.Error("expected Slack section")
	}
	if !strings.Contains(out, "vault:slack_bot") {
		t.Error("expected vault reference for bot token")
	}
	if !strings.Contains(out, "#backend") {
		t.Error("expected channel name")
	}
	if !strings.Contains(out, "auto-draft") {
		t.Error("expected auto-draft indicator")
	}
}

func TestRunStatus_Vault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "vault"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "not created") {
		t.Error("expected 'not created' for missing vault")
	}
}

func TestRunStatus_VaultWithSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	vaultPath := filepath.Join(dir, ".aileron", "secrets.json")
	os.MkdirAll(filepath.Dir(vaultPath), 0o700)
	os.WriteFile(vaultPath, []byte(`{"salt":"AAAA","secrets":{"slack_bot":{"value":"ZW5j","metadata":{"type":"secret"}}}}`), 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "vault"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "1 stored") {
		t.Error("expected '1 stored'")
	}
	if !strings.Contains(out, "slack_bot") {
		t.Error("expected secret name")
	}
}

func TestRunStatus_UnknownSection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown status section") {
		t.Error("expected unknown section error")
	}
}

func TestRunStatus_InHelp(t *testing.T) {
	var stdout bytes.Buffer
	run([]string{"help"}, newTestRegistry(), &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "aileron status") {
		t.Error("expected 'aileron status' in help output")
	}
}

func TestRunStatus_NotificationsWithDiscord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
notifications:
  discord:
    bot_token: vault:discord_bot
    channels:
      - name: "123456789"
        show: all
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "notifications"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Discord") {
		t.Error("expected Discord section")
	}
	if !strings.Contains(out, "vault:discord_bot") {
		t.Error("expected vault reference for discord token")
	}
	if !strings.Contains(out, "123456789") {
		t.Error("expected channel ID")
	}
}

func TestRunStatus_PolicyWithUserSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Create user settings with a rule.
	settingsDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.yaml"), []byte(`
version: 1
allow:
  - "my-custom-tool *"
`), 0o644)

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: deny
deny:
  - command: "deploy --force *"
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "policy"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "1 allow") {
		t.Error("expected user settings allow count")
	}
	if !strings.Contains(out, "deny") {
		t.Error("expected default disposition 'deny'")
	}
}

func TestRunStatus_EnvNoPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "env"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "No env scrubbing") {
		t.Error("expected no env scrubbing message when no policy")
	}
}

func TestRunStatus_NotificationsNoPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "notifications"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "No notifications") {
		t.Error("expected no notifications message")
	}
}

func TestRunLog_HelpShownInUsage(t *testing.T) {
	var stdout bytes.Buffer
	run([]string{"help"}, newTestRegistry(), &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "aileron log") {
		t.Error("expected 'aileron log' in help output")
	}
}

func TestRunSecret_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"secret"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron secret") {
		t.Errorf("expected usage message, got: %s", stderr.String())
	}
}

func TestRunSecret_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown secret command: "bogus"`) {
		t.Errorf("expected unknown command error, got: %s", stderr.String())
	}
}

func TestRunSecret_SetNoName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "set"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron secret set") {
		t.Errorf("expected usage message, got: %s", stderr.String())
	}
}

func TestRunSecret_ListEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No secrets stored") {
		t.Errorf("expected 'No secrets stored', got: %s", stdout.String())
	}
}

func TestRunSecret_ListWithSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Pre-populate the vault file directly (skip encryption for test).
	vaultPath := filepath.Join(dir, ".aileron", "secrets.json")
	os.MkdirAll(filepath.Dir(vaultPath), 0o700)
	os.WriteFile(vaultPath, []byte(`{"salt":"AAAA","secrets":{"slack_bot_token":{"value":"ZW5j","metadata":{"type":"secret"}},"discord_token":{"value":"ZW5j","metadata":{"type":"secret"}}}}`), 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "slack_bot_token") {
		t.Errorf("expected 'slack_bot_token' in output, got: %s", out)
	}
	if !strings.Contains(out, "discord_token") {
		t.Errorf("expected 'discord_token' in output, got: %s", out)
	}
}

// `aileron binding list` reads vault metadata without unlocking, per
// ADR-0011 acceptance: metadata is plaintext on disk, so the user
// can inspect what's bound before paying the passphrase prompt.

func TestRunBinding_ListEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No bindings stored") {
		t.Errorf("output = %q, want 'No bindings stored' message", stdout.String())
	}
}

func TestRunBinding_ListShowsMetadata(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	vaultPath := filepath.Join(dir, ".aileron", "secrets.json")
	os.MkdirAll(filepath.Dir(vaultPath), 0o700)
	// metadata is plaintext per ADR-0011; the value can be opaque.
	os.WriteFile(vaultPath, []byte(`{
  "version": 1,
  "salt": "AAAA",
  "secrets": {
    "oauth2/slack/work": {"value":"ZW5j","metadata":{"type":"oauth2","labels":{"connector":"slack","scope":"chat:write"}}},
    "connectors/github/default": {"value":"ZW5j","metadata":{"type":"api_key","labels":{"connector":"github"}}}
  }
}`), 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"PATH", "TYPE", "LABELS",
		"oauth2/slack/work", "oauth2", "connector=slack",
		"connectors/github/default", "api_key", "connector=github",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunBinding_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit code with no subcommand")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr missing usage hint: %s", stderr.String())
	}
}

func TestRunBinding_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "rebind"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit code for unknown subcommand")
	}
}

func TestRunPolicySave_NoEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	os.WriteFile(path, nil, 0o644)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", path}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No user-approved commands") {
		t.Errorf("expected 'No user-approved' message, got: %s", stdout.String())
	}
}

func TestRunPolicySave_WithApprovedCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Create audit log with ask_approved entries.
	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "git push", Disposition: "ask_approved",
	})
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "npm install", Disposition: "ask_approved",
	})
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "echo hello", Disposition: "allow",
	})

	// Create a project policy file.
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1\n"), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--scope", "project"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "2 approved command(s)") {
		t.Errorf("expected '2 approved command(s)', got:\n%s", out)
	}
	if !strings.Contains(out, "Saved 2 rule(s)") {
		t.Errorf("expected 'Saved 2 rule(s)', got:\n%s", out)
	}

	// Verify the rules were written to the policy file.
	data, _ := os.ReadFile(filepath.Join(dir, "aileron.yaml"))
	content := string(data)
	if !strings.Contains(content, "git push") {
		t.Errorf("expected 'git push' in policy file, got:\n%s", content)
	}
	if !strings.Contains(content, "npm install") {
		t.Errorf("expected 'npm install' in policy file, got:\n%s", content)
	}
}

func TestRunPolicySave_DryRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "git push", Disposition: "ask_approved",
	})

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1\n"), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--dry-run"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry run") {
		t.Errorf("expected 'dry run' message, got: %s", stdout.String())
	}

	// Verify nothing was written.
	data, _ := os.ReadFile(filepath.Join(dir, "aileron.yaml"))
	if strings.Contains(string(data), "git push") {
		t.Error("dry run should not modify the policy file")
	}
}

func TestRunPolicySave_DeduplicatesCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "git push", Disposition: "ask_approved",
	})
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s2", Command: "git push", Disposition: "ask_approved",
	})

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1\n"), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--scope", "project"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 approved command(s)") {
		t.Errorf("expected deduplication to 1 command, got:\n%s", stdout.String())
	}
}

func TestRunPolicySave_SkipsAlreadyAllowed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "git push", Disposition: "ask_approved",
	})

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1\nallow:\n  - \"git push\"\n"), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--scope", "project"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already in the policy") {
		t.Errorf("expected 'already in the policy' message, got: %s", stdout.String())
	}
}

func TestRunPolicySave_UserScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "curl api.com", Disposition: "ask_approved",
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--scope", "user"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "user settings") {
		t.Errorf("expected 'user settings' label, got: %s", stdout.String())
	}

	// Verify written to user settings.
	data, _ := os.ReadFile(filepath.Join(dir, ".aileron", "settings.yaml"))
	if !strings.Contains(string(data), "curl api.com") {
		t.Errorf("expected 'curl api.com' in user settings, got:\n%s", string(data))
	}
}

func TestRunPolicySave_FilterBySession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "git push", Disposition: "ask_approved",
	})
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s2", Command: "npm install", Disposition: "ask_approved",
	})

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1\n"), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--session", "s1", "--scope", "project"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 approved command(s)") {
		t.Errorf("expected 1 command for session s1, got:\n%s", stdout.String())
	}
}

func TestRunPolicySave_InvalidScope(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "echo test", Disposition: "ask_approved",
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--scope", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 for invalid scope, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid scope") {
		t.Errorf("expected 'invalid scope' error, got: %s", stderr.String())
	}
}

func TestRunPolicySave_MissingAuditFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", "/nonexistent/audit.jsonl"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

func TestRunPolicySave_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"policy"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron policy") {
		t.Errorf("expected usage message, got: %s", stderr.String())
	}
}

func TestRunPolicySave_InHelp(t *testing.T) {
	var stdout bytes.Buffer
	run([]string{"help"}, newTestRegistry(), &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "aileron policy save") {
		t.Error("expected 'aileron policy save' in help output")
	}
}

func TestRunPolicySave_NoPolicyForProject(t *testing.T) {
	dir := t.TempDir()

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "echo test", Disposition: "ask_approved",
	})

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--scope", "project"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 when no aileron.yaml, got %d", code)
	}
	if !strings.Contains(stderr.String(), "no aileron.yaml") {
		t.Errorf("expected 'no aileron.yaml' error, got: %s", stderr.String())
	}
}

func TestRunPolicySave_DefaultScopeIsProject(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "make build", Disposition: "ask_approved",
	})

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1\n"), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	// No --scope flag: should default to "project".
	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "project policy") {
		t.Errorf("expected 'project policy' label when no scope given, got:\n%s", out)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "aileron.yaml"))
	if !strings.Contains(string(data), "make build") {
		t.Errorf("expected 'make build' in policy file, got:\n%s", string(data))
	}
}

func TestRunPolicySave_AppendRuleError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "echo test", Disposition: "ask_approved",
	})

	// Create aileron.yaml as a directory to trigger a write error.
	os.MkdirAll(filepath.Join(dir, "aileron.yaml"), 0o755)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	_ = run([]string{"policy", "save", "--path", logPath, "--scope", "project"}, newTestRegistry(), &stdout, &stderr)
	// Should still exit 0 since it reports partial saves.
	out := stdout.String()
	if !strings.Contains(out, "Saved 0 rule(s)") {
		t.Errorf("expected 'Saved 0 rule(s)' when write fails, got:\n%s", out)
	}
	if !strings.Contains(stderr.String(), "error saving rule") {
		t.Errorf("expected error saving rule in stderr, got: %s", stderr.String())
	}
}

func TestRunPolicySave_UserScopeNoHome(t *testing.T) {
	dir := t.TempDir()
	// Set HOME to empty so UserHomeDir returns empty.
	t.Setenv("HOME", "")

	logPath := filepath.Join(dir, "audit.jsonl")
	audit.AppendShellEntry(logPath, audit.ShellEntry{
		SessionID: "s1", Command: "echo test", Disposition: "ask_approved",
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--path", logPath, "--scope", "user"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 when HOME is empty, got %d", code)
	}
	if !strings.Contains(stderr.String(), "cannot determine home directory") {
		t.Errorf("expected home directory error, got: %s", stderr.String())
	}
}

func TestRunPolicySave_InvalidFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--bogus-flag"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 for invalid flag, got %d", code)
	}
}

func TestRunPolicySave_DefaultAuditLogPath(t *testing.T) {
	// When --path is not given, runPolicySave uses ResolveAuditLogFromCwd.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	// Create an audit file at the default location.
	auditDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(auditDir, 0o755)
	auditPath := filepath.Join(auditDir, "audit.jsonl")
	audit.AppendShellEntry(auditPath, audit.ShellEntry{
		SessionID: "s1", Command: "docker push myimage", Disposition: "ask_approved",
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"policy", "save", "--dry-run"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "docker push myimage") {
		t.Errorf("expected command in dry-run output, got: %s", stdout.String())
	}
}

func TestTokenStatus(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"", "(not set)"},
		{"vault:my_secret", "vault:my_secret"},
		{"xoxb-plaintext-token", "(plaintext"},
	}
	for _, tt := range tests {
		got := tokenStatus(tt.value)
		if !strings.Contains(got, tt.want) {
			t.Errorf("tokenStatus(%q) = %q, want substring %q", tt.value, got, tt.want)
		}
	}
}

func TestRunSecret_InHelp(t *testing.T) {
	var stdout bytes.Buffer
	run([]string{"help"}, newTestRegistry(), &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "aileron secret set") {
		t.Error("expected 'aileron secret set' in help output")
	}
	if !strings.Contains(stdout.String(), "aileron secret list") {
		t.Error("expected 'aileron secret list' in help output")
	}
}

func mockPromptPassphrase(responses []string) func() {
	calls := 0
	old := promptPassphrase
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		if calls >= len(responses) {
			return "", fmt.Errorf("unexpected prompt call %d", calls)
		}
		r := responses[calls]
		calls++
		return r, nil
	}
	return func() { promptPassphrase = old }
}

func TestRunSecretSet_NewVault(t *testing.T) {
	dir := t.TempDir()
	origDefault := launch.DefaultVaultPath
	launch.DefaultVaultPath = func() string { return filepath.Join(dir, "secrets.json") }
	defer func() { launch.DefaultVaultPath = origDefault }()

	// Passphrase, confirm, secret value.
	restore := mockPromptPassphrase([]string{"mypass", "mypass", "secret-value"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runSecretSet([]string{"test_token"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Stored secret") {
		t.Error("expected success message")
	}
	if !strings.Contains(stderr.String(), "Creating a new Aileron vault") {
		t.Error("expected new vault warning")
	}
}

func TestRunSecretSet_WrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, "secrets.json")
	origDefault := launch.DefaultVaultPath
	launch.DefaultVaultPath = func() string { return vaultPath }
	defer func() { launch.DefaultVaultPath = origDefault }()

	// Create a vault with one secret.
	restore := mockPromptPassphrase([]string{"correct", "correct", "val"})
	var discard bytes.Buffer
	runSecretSet([]string{"existing"}, &discard, &discard)
	restore()

	// Now try to add with wrong passphrase.
	restore = mockPromptPassphrase([]string{"wrong", "also-wrong"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runSecretSet([]string{"new_token"}, &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit for wrong passphrase")
	}
}

func TestRunSecretSet_MismatchedConfirmation(t *testing.T) {
	dir := t.TempDir()
	origDefault := launch.DefaultVaultPath
	launch.DefaultVaultPath = func() string { return filepath.Join(dir, "secrets.json") }
	defer func() { launch.DefaultVaultPath = origDefault }()

	// Passphrase and confirm don't match.
	restore := mockPromptPassphrase([]string{"pass1", "pass2"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runSecretSet([]string{"test_token"}, &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit for mismatched passphrases")
	}
	if !strings.Contains(stderr.String(), "do not match") {
		t.Error("expected mismatch error")
	}
}
