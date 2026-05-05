package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestRunStatus_RuntimeUnreachable covers the offline path: with no
// daemon listening, `aileron status runtime` must exit cleanly and
// surface a "(not reachable)" line. The CLI is a thin client; users
// run it from arbitrary shells where the daemon may or may not be up.
func TestRunStatus_RuntimeUnreachable(t *testing.T) {
	// Force the fetcher to fail so the test doesn't depend on the host
	// having an actual server listening on the default port.
	prev := runtimeStatusFetcher
	runtimeStatusFetcher = func() (*runtimeStatus, error) {
		return nil, fmt.Errorf("connection refused")
	}
	defer func() { runtimeStatusFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "runtime"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Runtime") {
		t.Error("expected Runtime header")
	}
	if !strings.Contains(out, "not reachable") {
		t.Errorf("expected 'not reachable' hint when daemon is down; got: %s", out)
	}
}

// TestRunStatus_RuntimeReachable asserts the happy path: when the
// daemon answers /v1/status, the CLI surfaces version + counts +
// vault state. This is the primary `aileron status` deliverable from
// ADR-0004 (operational primitives).
func TestRunStatus_RuntimeReachable(t *testing.T) {
	commit := "abc1234"
	listen := "127.0.0.1:8721"
	gw := "http://127.0.0.1:54321"
	prev := runtimeStatusFetcher
	runtimeStatusFetcher = func() (*runtimeStatus, error) {
		return &runtimeStatus{
			Version:        "v0.0.42",
			Commit:         &commit,
			ListenAddr:     &listen,
			ActionCount:    3,
			ConnectorCount: 2,
			BindingCount:   5,
			VaultState:     "unlocked",
			GatewayUrl:     &gw,
		}, nil
	}
	defer func() { runtimeStatusFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "runtime"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"v0.0.42", "abc1234", "127.0.0.1:8721", "http://127.0.0.1:54321", "unlocked", "3 installed", "2 installed", "5 active"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in runtime output; got:\n%s", want, out)
		}
	}
}

// TestRunStatus_RuntimeIncludedInDefault asserts the bare `aileron
// status` (no section) renders the Runtime header alongside the
// existing sections. Operators reach for `aileron status` first; the
// daemon snapshot must be there without an extra subcommand.
func TestRunStatus_RuntimeIncludedInDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	prev := runtimeStatusFetcher
	runtimeStatusFetcher = func() (*runtimeStatus, error) {
		return &runtimeStatus{Version: "v0.0.42", VaultState: "missing"}, nil
	}
	defer func() { runtimeStatusFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Runtime") {
		t.Error("expected Runtime section in default status output")
	}
	if !strings.Contains(out, "v0.0.42") {
		t.Error("expected daemon version in default status output")
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

// fakeBindingServer stands up an in-process httptest.NewServer that
// implements just enough of the /v1/bindings surface to drive the
// CLI tests. Each test scopes AILERON_API_URL to this server's URL.
func fakeBindingServer(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/v1", fn))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
	return srv
}

func TestRunBinding_ListEmpty(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bindings" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No bindings.") {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestRunBinding_ListShowsTable(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[
			{"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team","connector_fqn":"github://aileron/linear","status":"active","created_at":"2024-01-01T00:00:00Z"},
			{"name":"oauth2/slack/work","kind":"oauth2","service":"slack","identity":"work","connector_fqn":"github://aileron/slack","status":"active","created_at":"2024-01-01T00:00:00Z"}
		]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NAME", "KIND", "STATUS",
		"api_key/linear/team", "oauth2/slack/work", "github://aileron/linear",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunBinding_ListPropagatesFilters(t *testing.T) {
	var seenQuery url.Values
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query()
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	var stdout, stderr bytes.Buffer
	run([]string{"binding", "list", "--connector", "github://aileron/linear", "--kind", "api_key"},
		newTestRegistry(), &stdout, &stderr)
	if got := seenQuery.Get("connector_fqn"); got != "github://aileron/linear" {
		t.Errorf("connector_fqn = %q", got)
	}
	if got := seenQuery.Get("kind"); got != "api_key" {
		t.Errorf("kind = %q", got)
	}
}

func TestRunBinding_InspectPrintsDetails(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bindings/api_key/linear/team" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team",
			"connector_fqn":"github://aileron/linear","scope":"issues:write","account":"alr@x",
			"created_at":"2024-01-01T12:00:00Z","status":"active"
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "inspect", "api_key/linear/team"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr = %s", code, stderr.String())
	}
	for _, want := range []string{
		"Name:       api_key/linear/team",
		"Kind:       api_key",
		"Connector:  github://aileron/linear",
		"Account:    alr@x",
		"Scope:      issues:write",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunBinding_InspectNotFoundIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "inspect", "api_key/missing/x"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit code")
	}
}

func TestRunBinding_SetupSendsAPIKeyBody(t *testing.T) {
	var got struct {
		ConnectorFQN string `json:"connector_fqn"`
		Bindings     []struct {
			Identity string `json:"identity"`
			Source   struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			} `json:"source"`
		} `json:"bindings"`
	}
	// CLI now probes oauth2/init first; the connector under test is
	// api_key, so the server returns 422 not_oauth2 and the CLI falls
	// through to the api_key flow.
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/setup/oauth2/init":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"not_oauth2","message":"connector declares api_key"}}`)
		case "/bindings/setup":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"created":[{"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team","connector_fqn":"github://aileron/linear","created_at":"2024-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	stdin := strings.NewReader("team\nlin-secret\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/linear"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr = %s", code, stderr.String())
	}
	if got.ConnectorFQN != "github://aileron/linear" {
		t.Errorf("connector_fqn = %q", got.ConnectorFQN)
	}
	if len(got.Bindings) != 1 {
		t.Fatalf("len(bindings) = %d", len(got.Bindings))
	}
	if got.Bindings[0].Identity != "team" || got.Bindings[0].Source.Kind != "api_key" ||
		got.Bindings[0].Source.Value != "lin-secret" {
		t.Errorf("body = %+v", got)
	}
	if !strings.Contains(stdout.String(), "Created: api_key/linear/team") {
		t.Errorf("stdout missing created line: %s", stdout.String())
	}
}

func TestRunBinding_SetupRejectsEmptyValue(t *testing.T) {
	// init returns 422 not_oauth2 → CLI falls through and rejects
	// blank api_key value before hitting /bindings/setup.
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bindings/setup/oauth2/init" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"not_oauth2"}}`)
			return
		}
		t.Error("/bindings/setup should not be hit when value is empty")
	})
	stdin := strings.NewReader("team\n\n") // identity provided, value empty
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/linear"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when value is blank")
	}
}

func TestRunBinding_RebindPostsValue(t *testing.T) {
	var seenBody []byte
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bindings/api_key/linear/team/rebind" {
			t.Errorf("path = %s", r.URL.Path)
		}
		seenBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team","connector_fqn":"github://aileron/linear","created_at":"2024-01-01T00:00:00Z"}`)
	})
	stdin := strings.NewReader("new-secret\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"rebind", "api_key/linear/team"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(string(seenBody), `"value":"new-secret"`) {
		t.Errorf("body = %s", seenBody)
	}
	if !strings.Contains(stdout.String(), "Rebound: api_key/linear/team") {
		t.Errorf("stdout: %s", stdout.String())
	}
}

func TestRunBinding_RevokeRequiresConfirmation(t *testing.T) {
	hits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	})
	// Cancel.
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"revoke", "api_key/linear/team"}, strings.NewReader("n\n"), &stdout, &stderr)
	if code != 0 {
		t.Errorf("cancel exit = %d", code)
	}
	if hits != 0 {
		t.Errorf("server called %d times on cancel", hits)
	}
	if !strings.Contains(stdout.String(), "cancelled") {
		t.Errorf("missing cancel message: %s", stdout.String())
	}

	// Confirm.
	stdout.Reset()
	stderr.Reset()
	code = runBinding([]string{"revoke", "api_key/linear/team"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Errorf("confirm exit = %d; stderr = %s", code, stderr.String())
	}
	if hits != 1 {
		t.Errorf("server called %d times on confirm", hits)
	}
	if !strings.Contains(stdout.String(), "Revoked: api_key/linear/team") {
		t.Errorf("missing revoked line: %s", stdout.String())
	}
}

func TestRunBinding_ListServerErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"oops"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stdout = %s", stdout.String())
	}
}

func TestRunBinding_SetupServerErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"oauth_setup_not_yet_supported"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/x"},
		strings.NewReader("work\nval\n"), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr = %s", stderr.String())
	}
}

func TestRunBinding_RebindNotFoundIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"rebind", "api_key/x/y"},
		strings.NewReader("v\n"), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr = %s", stderr.String())
	}
}

func TestRunBinding_RevokeServerErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"revoke", "api_key/x/y"},
		strings.NewReader("y\n"), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr = %s", stderr.String())
	}
}

func TestRunBinding_RevokeNotFoundIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"revoke", "api_key/x/y"},
		strings.NewReader("y\n"), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 404")
	}
}

func TestRunBinding_SetupRequiresIdentity(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be hit when CLI rejects empty identity")
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/x"},
		strings.NewReader("\n"), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for empty identity")
	}
}

func TestRunBinding_RebindRequiresValue(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be hit when CLI rejects empty value")
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"rebind", "api_key/x/y"},
		strings.NewReader("\n"), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for empty value")
	}
}

func TestRunBinding_InspectRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"inspect"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("inspect with no name accepted")
	}
}

func TestRunBinding_SetupRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("setup with no FQN accepted")
	}
}

func TestRunBinding_RebindRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"rebind"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("rebind with no name accepted")
	}
}

func TestRunBinding_RevokeRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"revoke"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("revoke with no name accepted")
	}
}

func TestRunBinding_TransportErrorIsExit1(t *testing.T) {
	// Point at a closed listener so the HTTP request fails at dial.
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:1/v1")
	for _, args := range [][]string{
		{"binding", "list"},
		{"binding", "inspect", "x"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, newTestRegistry(), &stdout, &stderr)
		if code == 0 {
			t.Errorf("%v: expected nonzero exit on transport error", args)
		}
	}
}

func TestRunBinding_ListBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"list", "--bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on bad flag")
	}
}

func TestRunBinding_InvalidJSONFromServerIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	})
	for _, args := range [][]string{
		{"binding", "list"},
		{"binding", "inspect", "x"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, newTestRegistry(), &stdout, &stderr)
		if code == 0 {
			t.Errorf("%v: expected nonzero exit on bad JSON", args)
		}
	}
}

func TestRunBinding_SetupParsesPartialResponse(t *testing.T) {
	// Server returns 201 but with garbage body — CLI should exit 1
	// rather than crash trying to render the response.
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "not json")
	})
	stdin := strings.NewReader("team\nval\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/x"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on bad response body")
	}
}

func TestBindingAPIBaseURL_DefaultAndOverride(t *testing.T) {
	t.Setenv("AILERON_API_URL", "")
	if got := bindingAPIBaseURL(); got != "http://localhost:8721/v1" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("AILERON_API_URL", "https://example.com/v1/")
	if got := bindingAPIBaseURL(); got != "https://example.com/v1" {
		t.Errorf("override = %q", got)
	}
}

func TestPromptLine_ReusesBufferedReader(t *testing.T) {
	// promptLine must accept an existing *bufio.Reader without
	// double-buffering, so two consecutive prompts read distinct
	// lines from the same source.
	r := bufio.NewReader(strings.NewReader("first\nsecond\n"))
	var out bytes.Buffer
	if got := promptLine(r, &out, "a: "); got != "first" {
		t.Errorf("first = %q", got)
	}
	if got := promptLine(r, &out, "b: "); got != "second" {
		t.Errorf("second = %q", got)
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
	code := run([]string{"binding", "wibble"}, newTestRegistry(), &stdout, &stderr)
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

// --- Connector + action install tests (#366) ---

// previewBody returns a canonical preview JSON payload used by tests
// that don't care about the preview-specific contents (capabilities,
// already_installed). For tests that DO care, write the payload
// inline.
func previewBody(fqn, version, hash string) string {
	return `{"fqn":"` + fqn + `","version":"` + version + `","hash":"` + hash + `","publisher":"test","signature_status":"verified","already_installed":false,"capabilities":{}}`
}

// connectorInstallServer routes preview + install requests to two
// separate handler functions. Mirrors the two-endpoint consent flow
// the CLI now drives. Either handler may be nil to default to a
// simple OK response.
func connectorInstallServer(t *testing.T, onPreview, onInstall http.HandlerFunc) *httptest.Server {
	return fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connectors/preview":
			if onPreview != nil {
				onPreview(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, previewBody("github://acme/x", "1.0.0", "sha256:abc"))
		case "/connectors/install":
			if onInstall != nil {
				onInstall(w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestRunConnector_InstallHappyPath(t *testing.T) {
	var seenInstallBody []byte
	var seenInstallPath string
	connectorInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		seenInstallPath = r.URL.Path
		seenInstallBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{
			"fqn":"github://acme/x",
			"version":"1.0.0",
			"hash":"sha256:abc",
			"entry_dir":"/path/to/entry"
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x", "--version=1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, body=%s", code, stderr.String())
	}
	if seenInstallPath != "/connectors/install" {
		t.Errorf("path = %q", seenInstallPath)
	}
	if !strings.Contains(string(seenInstallBody), `"fqn":"github://acme/x"`) ||
		!strings.Contains(string(seenInstallBody), `"version":"1.0.0"`) {
		t.Errorf("body = %s", seenInstallBody)
	}
	for _, want := range []string{"Connector install preview", "Installed:", "github://acme/x@1.0.0", "sha256:abc", "/path/to/entry"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunConnector_InstallAcceptsAtVersionInFQN(t *testing.T) {
	var seenInstallBody []byte
	connectorInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		seenInstallBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"x","entry_dir":"y"}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(string(seenInstallBody), `"version":"1.0.0"`) {
		t.Errorf("body = %s", seenInstallBody)
	}
}

// TestRunConnector_InstallAlreadyInstalledShortCircuits asserts the
// preview-driven short-circuit: when the preview reports
// already_installed=true, the CLI skips the prompt AND the install
// endpoint, rendering "Already installed" directly. The install
// handler must not be hit.
func TestRunConnector_InstallAlreadyInstalledShortCircuits(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","publisher":"test","signature_status":"verified","already_installed":true,"capabilities":{}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusOK)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called even though preview reported already_installed=true")
	}
	if !strings.Contains(stdout.String(), "Already installed") {
		t.Errorf("expected 'Already installed' in output: %s", stdout.String())
	}
}

func TestRunConnector_InstallMissingVersionRejected(t *testing.T) {
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when version is missing")
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when version is missing")
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when version is missing")
	}
}

func TestRunConnector_InstallVersionConflictRejected(t *testing.T) {
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when version conflicts")
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when version conflicts")
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0", "--version=2.0.0"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when --version conflicts with @<v> in FQN")
	}
}

// TestRunConnector_InstallSignatureFailureExits1 asserts ADR-0007's
// "signature failure is a hard fail" contract from the CLI side: the
// preview endpoint returns 422, the CLI exits 1 BEFORE prompting,
// and the install endpoint is never called.
func TestRunConnector_InstallSignatureFailureExits1(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"signature_failure","message":"signature_failure"}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr=%s", stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called after preview signature_failure; --yes must NOT bypass")
	}
}

func TestRunConnector_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit with no subcommand")
	}
}

func TestRunConnector_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "wibble"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on unknown subcommand")
	}
}

// TestRunConnector_InstallExpectedHashPropagated asserts the
// --hash flag flows through both the preview and install request
// bodies. The preview path matters because that's where signature
// verification happens — the hash check needs to fire there too.
func TestRunConnector_InstallExpectedHashPropagated(t *testing.T) {
	var previewBody, installBody []byte
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		previewBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abcd","publisher":"test","signature_status":"verified","already_installed":false,"capabilities":{}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fqn":"x","version":"1.0.0","hash":"h","entry_dir":"d"}`)
	})
	var stdout, stderr bytes.Buffer
	run([]string{"connector", "install", "github://acme/x@1.0.0", "--hash=sha256:abcd", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	for _, body := range [][]byte{previewBody, installBody} {
		if !strings.Contains(string(body), `"expected_hash":"sha256:abcd"`) {
			t.Errorf("body should include expected_hash: %s", body)
		}
	}
}

// TestRunConnector_InstallPromptYesProceeds asserts the operator's
// happy-path: preview shown, "y\n" piped, install called. This is
// the canonical interactive flow per ADR-0007.
func TestRunConnector_InstallPromptYesProceeds(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
	})

	r := newTestRegistry()
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("y\n")

	// We can't drive run() with a custom stdin because run() takes
	// os.Stdin via `runConnector`. Reach into runConnectorInstall
	// directly with our stdin reader.
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !installCalled {
		t.Error("install endpoint not called after 'y' confirmation")
	}
	if !strings.Contains(stdout.String(), "Install? [y/N]:") {
		t.Errorf("expected prompt in stdout; got: %s", stdout.String())
	}
	_ = r // silence unused if compiler complains
}

// TestRunConnector_InstallPromptNoCancels asserts the cancel path:
// when the operator types anything other than y/yes, the install
// endpoint is not called and the CLI prints "Cancelled."
func TestRunConnector_InstallPromptNoCancels(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
	})

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("n\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called after operator typed 'n'")
	}
	if !strings.Contains(stdout.String(), "Cancelled.") {
		t.Errorf("expected 'Cancelled.' in output; got: %s", stdout.String())
	}
}

// TestRunConnector_InstallPreviewRendersCapabilities asserts the
// consent prompt renders network hosts and credential kind/scope
// from the preview response. This is the load-bearing UX assertion
// — operators decide whether to install based on what's rendered
// here.
func TestRunConnector_InstallPreviewRendersCapabilities(t *testing.T) {
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"fqn":"github://aileron/slack",
			"version":"1.2.0",
			"hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"publisher":"Aileron Engineering",
			"signature_status":"verified",
			"already_installed":false,
			"capabilities":{
				"network_hosts":["slack.com:443"],
				"credential":{"kind":"api_key","scope":"Send Slack messages"}
			}
		}`)
	}, nil)

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("n\n") // cancel — we just want the rendering
	runConnectorInstall(
		[]string{"github://aileron/slack@1.2.0"},
		stdin, &stdout, &stderr,
	)
	out := stdout.String()
	for _, want := range []string{
		"Connector install preview",
		"github://aileron/slack",
		"1.2.0",
		"sha256:0123456789ab",
		"Aileron Engineering",
		"verified",
		"Network access:",
		"slack.com:443",
		"Credential required:",
		"api_key",
		"Send Slack messages",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview output missing %q:\n%s", want, out)
		}
	}
}

// TestRunConnector_InstallPreviewServerErrorExits1 asserts that a
// preview-side failure (e.g. fetch_failed because the source is
// down) exits 1 with the error visible. The install endpoint must
// not be reached.
func TestRunConnector_InstallPreviewServerErrorExits1(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"fetch_failed"}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when preview fails")
	}
	if installCalled {
		t.Error("install endpoint was called after preview failed")
	}
}

// TestRunConnectorCheck_Empty asserts that with nothing installed the
// CLI prints a useful "no connectors installed" line and exits 0 —
// the bare-server path operators see when they run check before any
// install.
func TestRunConnectorCheck_Empty(t *testing.T) {
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No connectors installed") {
		t.Errorf("expected 'No connectors installed' in output; got: %s", stdout.String())
	}
}

// TestRunConnectorCheck_UpdateAvailable asserts the primary signal:
// when the daemon reports update_available=true, the CLI renders the
// row with the latest version and a summary line counting updates.
func TestRunConnectorCheck_UpdateAvailable(t *testing.T) {
	latest := "0.0.6"
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{
			{
				Fqn:               "github://ALRubinger/aileron-connector-google",
				CurrentVersion:    "0.0.5",
				UpdateAvailable:   true,
				LatestVersion:     &latest,
				AvailableVersions: []string{"0.0.6"},
			},
		}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"github://ALRubinger/aileron-connector-google", "0.0.5", "0.0.6", "update available", "1 update(s) available"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got: %s", want, out)
		}
	}
}

// TestRunConnectorCheck_UpToDate asserts the "all good" path: every
// connector is on its latest version, summary says all up to date.
func TestRunConnectorCheck_UpToDate(t *testing.T) {
	latest := "1.0.0"
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{
			{Fqn: "github://acme/x", CurrentVersion: "1.0.0", UpdateAvailable: false, LatestVersion: &latest},
		}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "up to date") {
		t.Errorf("expected 'up to date' in output; got: %s", out)
	}
	if !strings.Contains(out, "All 1 connector(s) are up to date") {
		t.Errorf("expected summary line; got: %s", out)
	}
}

// TestRunConnectorCheck_PerConnectorError asserts the "offline-failing
// per connector" surface: a row with an error renders a "check failed"
// line, but other rows still render and the command exits 0.
func TestRunConnectorCheck_PerConnectorError(t *testing.T) {
	errMsg := "dial tcp: connection refused"
	latest := "1.1.0"
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{
			{Fqn: "github://acme/down", CurrentVersion: "1.0.0", Error: &errMsg},
			{Fqn: "github://acme/up", CurrentVersion: "1.0.0", UpdateAvailable: true, LatestVersion: &latest},
		}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (per-connector errors don't fail the command)", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "check failed") {
		t.Errorf("expected 'check failed' for the down connector; got: %s", out)
	}
	if !strings.Contains(out, "github://acme/up") {
		t.Errorf("expected the working connector to still render; got: %s", out)
	}
}

// TestRunConnectorCheck_IncludePrereleaseFlag asserts the
// --include-prerelease flag flows through to the fetcher (and thence
// to the daemon's query parameter). Without this propagation the flag
// would be silently dropped.
func TestRunConnectorCheck_IncludePrereleaseFlag(t *testing.T) {
	var sawInclude bool
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		sawInclude = includePrerelease
		return &connectorsCheckResponse{}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	run([]string{"connector", "check", "--include-prerelease"}, newTestRegistry(), &stdout, &stderr)
	if !sawInclude {
		t.Error("expected fetcher to be called with includePrerelease=true")
	}
}

// TestRunConnectorCheck_FetcherError asserts that a daemon error
// (network, 500, etc.) exits with a non-zero status and prints to
// stderr. Operators can wire the exit code into shell scripts.
func TestRunConnectorCheck_FetcherError(t *testing.T) {
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return nil, fmt.Errorf("connection refused")
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when fetcher fails")
	}
	if !strings.Contains(stderr.String(), "connection refused") {
		t.Errorf("expected error message in stderr; got: %s", stderr.String())
	}
}

// TestRunConnectorCheck_FetcherSendsQueryParam asserts the production
// fetcher (not the stub) builds the right URL when include-prerelease
// is requested. This is the only test that touches the actual HTTP
// call path.
func TestRunConnectorCheck_FetcherSendsQueryParam(t *testing.T) {
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	if _, err := fetchConnectorCheck(true); err != nil {
		t.Fatalf("fetchConnectorCheck: %v", err)
	}
	if sawQuery != "include_prerelease=true" {
		t.Errorf("query = %q, want include_prerelease=true", sawQuery)
	}
}

// TestRunSync_Empty asserts the empty-state path: no actions, no
// requireds, no installs. The CLI prints a "no dependencies" line
// and exits 0 — what an operator running sync on a fresh project
// sees before installing anything.
func TestRunSync_Empty(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No connector dependencies") {
		t.Errorf("expected empty-state message; got: %s", stdout.String())
	}
}

// TestRunSync_InstallsAndUnbound asserts the canonical happy path:
// one action declares a connector, sync installs it, and reports an
// unbound capability so the operator knows to run `aileron binding
// setup <FQN>` next.
func TestRunSync_InstallsAndUnbound(t *testing.T) {
	scope := "test scope"
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			ActionsSeen: 1,
			Required:    []connectorRefWire{{Fqn: "github://acme/widget", Version: "1.0.0"}},
			Installed: []installedConnectorWire{
				{Fqn: "github://acme/widget", Version: "1.0.0", Hash: "sha256:abc"},
			},
			Unbound: []unboundCapabilityWire{
				{ConnectorFqn: "github://acme/widget", Kind: "api_key", Scope: &scope},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Walked 1 action", "github://acme/widget", "Installed 1", "1 unbound", "api_key", "test scope", "aileron binding setup"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got: %s", want, out)
		}
	}
}

// TestRunSync_AlreadyInstalledIdempotent asserts the second-run path:
// every connector already present, no failures, no unbound. A clean
// idempotent re-run should print "already installed" and exit 0.
func TestRunSync_AlreadyInstalledIdempotent(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			ActionsSeen:      1,
			Required:         []connectorRefWire{{Fqn: "github://acme/widget", Version: "1.0.0"}},
			AlreadyInstalled: []connectorRefWire{{Fqn: "github://acme/widget", Version: "1.0.0"}},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "already installed") {
		t.Errorf("expected 'already installed'; got: %s", out)
	}
	if strings.Contains(out, "Installed 1") {
		t.Errorf("did not expect 'Installed 1' line on idempotent re-run; got: %s", out)
	}
}

// TestRunSync_InstallFailureExits1 asserts the script-friendly path:
// any install failure exits 1 so operators wiring sync into CI / Make
// targets can branch on the exit code.
func TestRunSync_InstallFailureExits1(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			ActionsSeen: 1,
			Required:    []connectorRefWire{{Fqn: "github://acme/widget", Version: "1.0.0"}},
			InstallFailures: []connectorFailureWire{
				{Fqn: "github://acme/widget", Version: "1.0.0", Error: "connection refused"},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 on install failure", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "1 install failure") {
		t.Errorf("expected '1 install failure'; got: %s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("expected error message in output; got: %s", out)
	}
}

// TestRunSync_BindAllWarns asserts that --bind-all prints a clear
// "not yet implemented" warning so operators learn the limitation
// without filing a "broken" bug. When --bind-all lands as a real
// feature, this test will be replaced.
func TestRunSync_BindAllWarns(t *testing.T) {
	scope := "test"
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			Unbound: []unboundCapabilityWire{
				{ConnectorFqn: "github://acme/widget", Kind: "api_key", Scope: &scope},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	run([]string{"sync", "--bind-all"}, newTestRegistry(), &stdout, &stderr)
	if !strings.Contains(stderr.String(), "not yet implemented") {
		t.Errorf("expected '--bind-all not yet implemented' warning; got stderr: %s", stderr.String())
	}
}

// TestRunSync_YesFlagPropagated asserts the --yes flag flows through
// to the fetcher (and thence to the daemon's auto_install body
// field). Without this propagation the flag would be silently dropped.
func TestRunSync_YesFlagPropagated(t *testing.T) {
	var sawAutoInstall bool
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		sawAutoInstall = autoInstall
		return &syncResult{}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	run([]string{"sync", "--yes"}, newTestRegistry(), &stdout, &stderr)
	if !sawAutoInstall {
		t.Error("expected fetcher to be called with autoInstall=true")
	}
}

// TestRunSync_FetcherError asserts the daemon-down path: a network
// or 5xx error from the daemon exits 1 and prints the error to stderr.
func TestRunSync_FetcherError(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return nil, fmt.Errorf("connection refused")
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on fetcher error")
	}
	if !strings.Contains(stderr.String(), "connection refused") {
		t.Errorf("expected error in stderr; got: %s", stderr.String())
	}
}

// TestPostSyncRequest_HappyPath asserts the actual HTTP fetcher (not
// the stub) hits the right URL, sends the right method + headers, and
// decodes the daemon's response into syncResult correctly. The other
// runSync tests stub the fetcher; this is the only path that
// exercises the real network code.
func TestPostSyncRequest_HappyPath(t *testing.T) {
	var sawMethod, sawPath, sawContentType string
	var sawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod = r.Method
		sawPath = r.URL.Path
		sawContentType = r.Header.Get("Content-Type")
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"actions_seen": 2,
			"required": [{"fqn":"github://acme/x","version":"1.0.0"}],
			"installed": [],
			"already_installed": [{"fqn":"github://acme/x","version":"1.0.0"}],
			"install_failures": [],
			"unbound": []
		}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	got, err := postSyncRequest(false)
	if err != nil {
		t.Fatalf("postSyncRequest: %v", err)
	}
	if sawMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", sawMethod)
	}
	if sawPath != "/v1/sync" {
		t.Errorf("path = %q, want /v1/sync", sawPath)
	}
	if sawContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", sawContentType)
	}
	if !strings.Contains(string(sawBody), `"auto_install":false`) {
		t.Errorf("body should contain auto_install=false; got: %s", sawBody)
	}
	if got.ActionsSeen != 2 {
		t.Errorf("ActionsSeen = %d, want 2", got.ActionsSeen)
	}
	if len(got.Required) != 1 || got.Required[0].Fqn != "github://acme/x" {
		t.Errorf("Required = %v, want [github://acme/x]", got.Required)
	}
	if len(got.AlreadyInstalled) != 1 {
		t.Errorf("AlreadyInstalled = %v, want 1", got.AlreadyInstalled)
	}
}

// TestPostSyncRequest_AutoInstallTrueInBody asserts the autoInstall
// argument flows into the JSON body. Mirrors --yes propagation
// from the runSync test, but checks the wire shape directly.
func TestPostSyncRequest_AutoInstallTrueInBody(t *testing.T) {
	var sawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"actions_seen":0,"required":[],"installed":[],"already_installed":[],"install_failures":[],"unbound":[]}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	if _, err := postSyncRequest(true); err != nil {
		t.Fatalf("postSyncRequest: %v", err)
	}
	if !strings.Contains(string(sawBody), `"auto_install":true`) {
		t.Errorf("body should contain auto_install=true; got: %s", sawBody)
	}
}

// TestPostSyncRequest_Non200ReturnsError asserts daemon-side errors
// (e.g. 401 unauthorized, 500 internal) surface as a Go error with
// the status code embedded in the message. Operators wiring sync
// into shell scripts can grep for the code if needed.
func TestPostSyncRequest_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"internal_error","message":"oops"}}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	_, err := postSyncRequest(false)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500; got: %v", err)
	}
	if !strings.Contains(err.Error(), "oops") {
		t.Errorf("error should include response body; got: %v", err)
	}
}

// TestPostSyncRequest_MalformedJSONReturnsError asserts that a
// truncated or otherwise unparseable response surfaces as a wrapped
// "decoding response" error. Without this, a daemon bug producing
// bad JSON would crash the CLI rather than reporting cleanly.
func TestPostSyncRequest_MalformedJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"actions_seen": not_a_number}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	_, err := postSyncRequest(false)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if !strings.Contains(err.Error(), "decoding response") {
		t.Errorf("error should mention 'decoding response'; got: %v", err)
	}
}

// TestPostSyncRequest_NetworkErrorReturnsError asserts a closed
// listener (server already shut down) surfaces as a Go error rather
// than a panic. Same robustness contract as fetchRuntimeStatus and
// fetchConnectorCheck.
func TestPostSyncRequest_NetworkErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // shut down listener immediately
	t.Setenv("AILERON_API_URL", addr+"/v1")

	_, err := postSyncRequest(false)
	if err == nil {
		t.Fatal("expected error on closed listener")
	}
}

func TestRunAction_AddHappyPath(t *testing.T) {
	var seenBody []byte
	var seenPath string
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{
			"name":"list-recent-prs",
			"fqn":"github://acme/x/actions/list-recent-prs",
			"version":"0.1.0",
			"source":"github://acme/x/actions/list-recent-prs@0.1.0",
			"path":"/path/list-recent-prs.md"
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://acme/x/actions/list-recent-prs", "--version=0.1.0"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if seenPath != "/actions/install" {
		t.Errorf("path = %q", seenPath)
	}
	if !strings.Contains(string(seenBody), `"version":"0.1.0"`) {
		t.Errorf("body = %s", seenBody)
	}
	for _, want := range []string{"Added:", "list-recent-prs", "/path/list-recent-prs.md"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunAction_AddForceFlagPropagated(t *testing.T) {
	var seenBody []byte
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"x","fqn":"y","version":"1.0.0","source":"y","path":"z"}`)
	})
	var stdout, stderr bytes.Buffer
	run([]string{"action", "add", "github://acme/x/actions/y@0.1.0", "--force"},
		newTestRegistry(), &stdout, &stderr)
	if !strings.Contains(string(seenBody), `"force":true`) {
		t.Errorf("force flag not propagated: %s", seenBody)
	}
}

func TestRunAction_AddAlreadyInstalledIs200(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"name":"x","fqn":"y","version":"1","source":"y","path":"z","already_installed":true
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://acme/x/actions/y@0.1.0"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "Already installed") {
		t.Errorf("output: %s", stdout.String())
	}
}

func TestRunAction_AddMissingVersionRejected(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://acme/x/actions/y"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when version missing")
	}
}

func TestRunAction_AddServerErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://acme/x/actions/y@0.1.0"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit")
	}
}

func TestRunAction_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"action"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit")
	}
}

func TestRunAction_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "wibble"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit")
	}
}

func TestSplitFQNVersion(t *testing.T) {
	cases := []struct {
		fqn         string
		flag        string
		wantFQN     string
		wantVersion string
		wantErr     bool
	}{
		{"github://x/y", "1.0.0", "github://x/y", "1.0.0", false},
		{"github://x/y@1.0.0", "", "github://x/y", "1.0.0", false},
		{"github://x/y@1.0.0", "1.0.0", "github://x/y", "1.0.0", false},
		{"github://x/y@1.0.0", "2.0.0", "", "", true},
		{"github://x/y", "", "", "", true},
		{"github://x/y@", "", "", "", true},
	}
	for _, c := range cases {
		fqn, ver, err := splitFQNVersion(c.fqn, c.flag)
		if c.wantErr {
			if err == nil {
				t.Errorf("splitFQNVersion(%q, %q) = no error; want error", c.fqn, c.flag)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitFQNVersion(%q, %q): %v", c.fqn, c.flag, err)
			continue
		}
		if fqn != c.wantFQN || ver != c.wantVersion {
			t.Errorf("splitFQNVersion(%q, %q) = (%q, %q); want (%q, %q)",
				c.fqn, c.flag, fqn, ver, c.wantFQN, c.wantVersion)
		}
	}
}

// fakeBrowser captures URLs the CLI would have asked the system to
// open, without actually spawning a process. Tests that exercise the
// OAuth dance swap bindingBrowser to one of these so `go test`
// doesn't pop real browser windows.
type fakeBrowser struct{ opened []string }

func (f *fakeBrowser) Open(url string) error { f.opened = append(f.opened, url); return nil }

// withFakeBrowser swaps bindingBrowser to a fake for the duration of
// a test, restoring the default on cleanup.
func withFakeBrowser(t *testing.T) *fakeBrowser {
	t.Helper()
	prev := bindingBrowser
	fb := &fakeBrowser{}
	bindingBrowser = fb
	t.Cleanup(func() { bindingBrowser = prev })
	return fb
}

// TestRunBinding_SetupOAuth2DriveDance exercises the full
// server-driven OAuth dance from the CLI's perspective: init returns
// an authorize URL pointing at a fake provider, CLI spins up its
// loopback listener, a goroutine simulates the user's browser
// landing on the redirect URI with `code` + `state`, CLI POSTs
// finish, prints the bound name.
func TestRunBinding_SetupOAuth2DriveDance(t *testing.T) {
	fb := withFakeBrowser(t)
	_ = fb // verified at the end
	var seenInitBody []byte
	var seenFinishBody []byte
	var redirectURI string
	var sessionID = "test-session-id"
	var state = "test-state-abc"

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/setup/oauth2/init":
			seenInitBody, _ = io.ReadAll(r.Body)
			// Pick a free port for the redirect_uri the CLI will use.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("port pick: %v", err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
			authorizeURL := fmt.Sprintf("http://provider.test/authorize?state=%s&redirect_uri=%s", state, redirectURI)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"session_id":%q,"authorize_url":%q,"redirect_uri":%q}`,
				sessionID, authorizeURL, redirectURI))

			// Simulate the user clicking through the OAuth screen
			// and the provider redirecting to the loopback callback.
			// Small delay so the CLI has time to bind its listener.
			go func() {
				time.Sleep(100 * time.Millisecond)
				_, _ = http.Get(redirectURI + "?code=auth-code&state=" + state)
			}()
		case "/bindings/setup/oauth2/finish":
			seenFinishBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/aileron-connector-google","created_at":"2024-01-01T00:00:00Z"
			}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	// Stub the browser-open so tests don't pop a real browser.
	// (The CLI calls SystemBrowser{}.Open which would invoke `open`/`xdg-open`/`start`;
	// we tolerate the failure since the simulated callback fires regardless.)

	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://acme/aileron-connector-google"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(string(seenInitBody), `"connector_fqn":"github://acme/aileron-connector-google"`) {
		t.Errorf("init body missing connector_fqn: %s", seenInitBody)
	}
	if !strings.Contains(string(seenInitBody), `"identity":"work"`) {
		t.Errorf("init body missing identity: %s", seenInitBody)
	}
	if !strings.Contains(string(seenFinishBody), `"session_id":"`+sessionID+`"`) {
		t.Errorf("finish body missing session_id: %s", seenFinishBody)
	}
	if !strings.Contains(string(seenFinishBody), `"code":"auth-code"`) {
		t.Errorf("finish body missing code: %s", seenFinishBody)
	}
	if !strings.Contains(string(seenFinishBody), `"state":"`+state+`"`) {
		t.Errorf("finish body missing state: %s", seenFinishBody)
	}
	if !strings.Contains(stdout.String(), "Bound: oauth2/google/work") {
		t.Errorf("stdout missing bound line: %s", stdout.String())
	}
	// Confirm the CLI asked the (fake) browser to open the authorize URL.
	if len(fb.opened) != 1 {
		t.Errorf("fake browser open count = %d, want 1", len(fb.opened))
	}
}

func TestRunBinding_SetupOAuth2_InitErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"oops"}`)
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://acme/x"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr = %s", stderr.String())
	}
}

func TestPortFromRedirectURI(t *testing.T) {
	cases := map[string]int{
		"http://127.0.0.1:54321/callback":   54321,
		"http://localhost:8080/x":           8080,
		"http://127.0.0.1/callback":         0, // no port
		"https://accounts.google.com/auth":  0, // standard port omitted
		"":                                  0,
		"not a url":                         0,
	}
	for in, want := range cases {
		if got := portFromRedirectURI(in); got != want {
			t.Errorf("portFromRedirectURI(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestRunAction_AddAutoPromptsForUnboundCapabilities(t *testing.T) {
	// After install, the server returned `unbound_capabilities`; the
	// CLI prompts the user, drops into binding setup for each yes,
	// and runs the api_key flow when the connector declares api_key.
	var initHits, setupHits int
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"echo","fqn":"github://x/y/actions/echo",
				"version":"1.0.0","source":"x","path":"/p",
				"unbound_capabilities":[{"connector_fqn":"github://x/conn-a","kind":"api_key","scope":"read"}]
			}`)
		case "/bindings/setup/oauth2/init":
			initHits++
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"not_oauth2"}}`)
		case "/bindings/setup":
			setupHits++
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"created":[{"name":"api_key/conn-a/work","kind":"api_key","service":"conn-a","identity":"work","connector_fqn":"github://x/conn-a","created_at":"2024-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	// Yes-to-prompt, identity "work", api_key value "k".
	stdin := strings.NewReader("y\nwork\nk\n")
	var stdout, stderr bytes.Buffer
	code := runAction([]string{"add", "github://x/y/actions/echo@1.0.0"},
		stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if initHits != 1 {
		t.Errorf("oauth2/init hits = %d, want 1", initHits)
	}
	if setupHits != 1 {
		t.Errorf("bindings/setup hits = %d, want 1", setupHits)
	}
	if !strings.Contains(stdout.String(), "github://x/conn-a") {
		t.Errorf("stdout should mention the connector FQN: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Created: api_key/conn-a/work") {
		t.Errorf("stdout missing binding-created line: %s", stdout.String())
	}
}

func TestRunAction_AddDeclineDoesNotBindButPrintsHint(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/actions/install" {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"echo","fqn":"x","version":"1.0.0","source":"x","path":"/p",
				"unbound_capabilities":[{"connector_fqn":"github://x/conn-a","kind":"oauth2"}]
			}`)
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	})
	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := runAction([]string{"add", "github://x/y/actions/echo@1.0.0"},
		stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Skipped") {
		t.Errorf("stdout should mention skipped: %s", stdout.String())
	}
}

func TestRunAction_AddNoBindFlagSkipsPrompt(t *testing.T) {
	hits := map[string]int{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		if r.URL.Path == "/actions/install" {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"echo","fqn":"x","version":"1.0.0","source":"x","path":"/p",
				"unbound_capabilities":[{"connector_fqn":"github://x/conn-a","kind":"api_key"}]
			}`)
			return
		}
		t.Errorf("unexpected path with --no-bind: %s", r.URL.Path)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://x/y/actions/echo@1.0.0", "--no-bind"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if hits["/bindings/setup/oauth2/init"] != 0 {
		t.Errorf("--no-bind should not call binding setup")
	}
	if !strings.Contains(stdout.String(), "Run `aileron binding setup") {
		t.Errorf("stdout should print the manual-binding hint with --no-bind: %s", stdout.String())
	}
}

func TestRunBinding_SetupOAuth2_MalformedInitResponse(t *testing.T) {
	withFakeBrowser(t)
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Server claims 200 but body isn't valid JSON.
		_, _ = io.WriteString(w, `{not json`)
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://x/y"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit on malformed init response; stderr=%s", stderr.String())
	}
}

func TestRunBinding_SetupOAuth2_BadRedirectURIRejected(t *testing.T) {
	withFakeBrowser(t)
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"s","authorize_url":"http://x/auth","redirect_uri":"not-a-url"}`)
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://x/y"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit when redirect_uri has no port; stderr=%s", stderr.String())
	}
}

func TestRunBinding_SetupOAuth2_FinishErrorIsExit1(t *testing.T) {
	withFakeBrowser(t)
	var redirectURI string
	state := "test-state"
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/setup/oauth2/init":
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
			authorizeURL := fmt.Sprintf("http://provider.test/auth?state=%s&redirect_uri=%s", state, redirectURI)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"session_id":"s","authorize_url":%q,"redirect_uri":%q}`,
				authorizeURL, redirectURI))
			go func() {
				time.Sleep(50 * time.Millisecond)
				_, _ = http.Get(redirectURI + "?code=c&state=" + state)
			}()
		case "/bindings/setup/oauth2/finish":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"token_exchange_failed"}}`)
		}
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://x/y"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit when finish fails; stderr=%s", stderr.String())
	}
}

func TestRunBinding_SetupOAuth2_PortBindFailsWhenSamePortInUse(t *testing.T) {
	withFakeBrowser(t)
	// Bind a port and HOLD it; the CLI's listener.NewListener call
	// for the same port will fail.
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	heldPort := holder.Addr().(*net.TCPAddr).Port
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Return the held port as the redirect URI; CLI's bind fails.
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"session_id":"s","authorize_url":"http://x/auth?state=s","redirect_uri":"http://127.0.0.1:%d/callback"}`,
			heldPort))
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://x/y"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when listener bind fails")
	}
}

// --- runActionAdd: auto-install missing connectors (issue #413) ---

func TestRunActionAdd_PromptsAndRetriesOnConnectorsMissing(t *testing.T) {
	// First POST returns 422 connectors_missing; user accepts at the
	// prompt; second POST is sent with auto_install_connectors=true
	// and the server replies 201.
	var calls []map[string]any
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/actions/install" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body)

		auto, _ := body["auto_install_connectors"].(bool)
		w.Header().Set("Content-Type", "application/json")
		if !auto {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"connectors_missing","message":"need deps","details":[
				{"name":"github://acme/conn","version":"1.0.0","hash":"sha256:abc"}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"github://acme/conn/actions/run@0.1.0","path":"/tmp/my-action.md"}`)
	})

	stdin := strings.NewReader("y\n")
	var stdout, stderr bytes.Buffer
	code := runActionAdd([]string{"github://acme/conn/actions/run@0.1.0"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 install calls, got %d", len(calls))
	}
	if got, _ := calls[0]["auto_install_connectors"].(bool); got {
		t.Error("first call must not set auto_install_connectors")
	}
	if got, _ := calls[1]["auto_install_connectors"].(bool); !got {
		t.Error("second call must set auto_install_connectors=true")
	}
	out := stdout.String()
	for _, want := range []string{
		"github://acme/conn@1.0.0",
		"sha256:abc",
		"Install the connector(s) before adding this action?",
		"Added: my-action",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunActionAdd_DeclinesPromptExitsCleanly(t *testing.T) {
	// User answers "n" — the CLI does not retry, prints a "Skipped"
	// hint, and exits 1 without dumping the raw error envelope.
	var callCount int
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"connectors_missing","message":"need deps","details":[
			{"name":"github://acme/conn","version":"1.0.0","hash":"sha256:abc"}
		]}}`)
	})
	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := runActionAdd([]string{"github://acme/conn/actions/run@0.1.0"}, stdin, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1; stderr = %s", code, stderr.String())
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 install call, got %d", callCount)
	}
	if !strings.Contains(stdout.String(), "Skipped.") {
		t.Errorf("output should include 'Skipped.', got: %s", stdout.String())
	}
	if strings.Contains(stderr.String(), "server returned") {
		t.Errorf("must not dump raw envelope on user decline, stderr: %s", stderr.String())
	}
}

func TestRunActionAdd_NoAutoInstallFlagSkipsPrompt(t *testing.T) {
	// --no-auto-install → CLI does not prompt; the original 422 is
	// surfaced verbatim. Lets scripts opt out of the interactive
	// retry while keeping the structural pre-check intact.
	var callCount int
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"connectors_missing","message":"need deps","details":[
			{"name":"github://acme/conn","version":"1.0.0","hash":"sha256:abc"}
		]}}`)
	})
	// Stdin is empty — if the prompt fired we'd hit EOF. The flag
	// suppresses the prompt entirely.
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0", "--no-auto-install"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 install call, got %d", callCount)
	}
	if !strings.Contains(stderr.String(), "connectors_missing") {
		t.Errorf("stderr should contain raw envelope, got: %s", stderr.String())
	}
}

// --- aileron audit ---

// TestRunAudit_EmptyPrintsNoEntries: with no events the CLI prints a
// useful message and exits 0. This is the bare-server path operators
// see before any audit-emitting actions have run.
func TestRunAudit_EmptyPrintsNoEntries(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return &auditListWire{Events: []auditEventWire{}}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No audit events") {
		t.Errorf("expected 'No audit events'; got: %s", stdout.String())
	}
}

// TestRunAudit_TabularRendersTimestampIDTypeAndSummary: the default
// rendering is a one-line-per-event table whose columns are stable
// for shell pipelines.
func TestRunAudit_TabularRendersTimestampIDTypeAndSummary(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return &auditListWire{Events: []auditEventWire{{
			AuditID:   "audit-xyz",
			EventType: "execution.failed",
			Timestamp: time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC),
			Payload: map[string]any{
				"class":   "binding_required",
				"details": map[string]any{"connector": "github://aileron/slack"},
			},
		}}}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"audit-xyz",
		"execution.failed",
		"class=binding_required",
		"connector=github://aileron/slack",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got: %s", want, out)
		}
	}
}

// TestRunAudit_FlagsForwardedToFetcher: every documented filter flag
// ends up on the auditListQuery sent to the daemon. Drift between
// the CLI flags and the wire query is the most likely regression.
func TestRunAudit_FlagsForwardedToFetcher(t *testing.T) {
	var seen auditListQuery
	prev := auditListFetcher
	auditListFetcher = func(q auditListQuery) (*auditListWire, error) {
		seen = q
		return &auditListWire{}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	args := []string{
		"audit", "list",
		"--since", "2026-05-01T00:00:00Z",
		"--audit-id", "audit-xyz",
		"--connector", "github://aileron/slack",
		"--class", "binding_required",
		"--limit", "50",
	}
	code := run(args, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if seen.since != "2026-05-01T00:00:00Z" ||
		seen.auditID != "audit-xyz" ||
		seen.connector != "github://aileron/slack" ||
		seen.class != "binding_required" ||
		seen.limit != 50 {
		t.Errorf("query = %+v; some flags didn't reach fetcher", seen)
	}
}

// TestRunAudit_JSONRendersOnePerLine: --json switches to one
// JSON-encoded event per line. Round-trips back through json.Decode.
func TestRunAudit_JSONRendersOnePerLine(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return &auditListWire{Events: []auditEventWire{
			{AuditID: "a1", EventType: "action.installed", Timestamp: time.Now()},
			{AuditID: "a2", EventType: "binding.created", Timestamp: time.Now()},
		}}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), stdout.String())
	}
	for _, line := range lines {
		var ev auditEventWire
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %q is not JSON: %v", line, err)
		}
	}
}

// TestRunAudit_FetcherErrorExitsNonZero: a daemon error (network,
// 500, etc.) exits non-zero with a useful stderr line. Operators can
// rely on the exit code in scripts.
func TestRunAudit_FetcherErrorExitsNonZero(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return nil, fmt.Errorf("connection refused")
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "list"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit on fetcher error")
	}
	if !strings.Contains(stderr.String(), "connection refused") {
		t.Errorf("expected error in stderr; got: %s", stderr.String())
	}
}

// TestRunAuditShow_HappyPath: `aileron audit show <id>` prints the
// event as pretty JSON.
func TestRunAuditShow_HappyPath(t *testing.T) {
	prev := auditGetFetcher
	auditGetFetcher = func(id string) (*auditEventWire, int, error) {
		if id != "audit-xyz" {
			t.Errorf("fetcher saw id=%q", id)
		}
		return &auditEventWire{
			AuditID:   "audit-xyz",
			EventType: "action.installed",
			Timestamp: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			Payload:   map[string]any{"name": "ship-update"},
		}, http.StatusOK, nil
	}
	defer func() { auditGetFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "show", "audit-xyz"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	var got auditEventWire
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; out=%s", err, stdout.String())
	}
	if got.AuditID != "audit-xyz" || got.EventType != "action.installed" {
		t.Errorf("got = %+v", got)
	}
}

// TestRunAuditShow_NotFoundExitsNonZero: 404 from the daemon translates
// to a non-zero exit and a stderr line naming the missing id.
func TestRunAuditShow_NotFoundExitsNonZero(t *testing.T) {
	prev := auditGetFetcher
	auditGetFetcher = func(_ string) (*auditEventWire, int, error) {
		return nil, http.StatusNotFound, nil
	}
	defer func() { auditGetFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "show", "missing"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit on 404")
	}
	if !strings.Contains(stderr.String(), "missing") {
		t.Errorf("stderr should name the id; got: %s", stderr.String())
	}
}

// TestRunAuditShow_RequiresAuditID: with no positional argument the
// CLI prints usage and exits non-zero; the fetcher is not called.
func TestRunAuditShow_RequiresAuditID(t *testing.T) {
	called := false
	prev := auditGetFetcher
	auditGetFetcher = func(_ string) (*auditEventWire, int, error) {
		called = true
		return nil, http.StatusOK, nil
	}
	defer func() { auditGetFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "show"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit when audit-id is missing")
	}
	if called {
		t.Error("fetcher should not be called without an audit-id")
	}
	if !strings.Contains(stderr.String(), "audit-id") {
		t.Errorf("stderr should mention audit-id; got: %s", stderr.String())
	}
}

// TestRunAudit_UnknownSubcommand: unrecognized subcommand prints
// usage and exits non-zero.
func TestRunAudit_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit on unknown subcommand")
	}
	if !strings.Contains(stderr.String(), "unknown audit command") {
		t.Errorf("stderr should explain the failure; got: %s", stderr.String())
	}
}

// TestFetchAuditGet_HitsCorrectURL: the production fetcher hits
// `/v1/audit/<id>` (with PathEscape), decodes the wire shape on 200,
// and returns http.StatusNotFound without an error on 404 so the
// caller can branch on status without parsing the error string.
func TestFetchAuditGet_HitsCorrectURL(t *testing.T) {
	var sawPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audit/", func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		if r.URL.Path == "/v1/audit/audit-known" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"audit_id":"audit-known","event_type":"action.installed","timestamp":"2026-05-01T00:00:00Z","actor":{"type":"human","id":"user"},"payload":{"name":"ship-update"}}`)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	got, status, err := fetchAuditGet("audit-known")
	if err != nil || status != http.StatusOK {
		t.Fatalf("known: status=%d err=%v", status, err)
	}
	if got.AuditID != "audit-known" || got.EventType != "action.installed" {
		t.Errorf("got = %+v", got)
	}
	if sawPath != "/v1/audit/audit-known" {
		t.Errorf("path = %q", sawPath)
	}

	_, status, err = fetchAuditGet("missing")
	if err != nil {
		t.Errorf("404 should not surface as error; got %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestAuditPayloadSummary_PerEventShape exercises the renderer
// branches the tabular tests don't reach: action-installed (name
// + fqn), binding event (connector_fqn only), and an event with
// no useful keys.
func TestAuditPayloadSummary_PerEventShape(t *testing.T) {
	cases := []struct {
		name string
		in   auditEventWire
		want string
	}{
		{
			name: "action-installed-with-fqn",
			in: auditEventWire{
				EventType: "action.installed",
				Payload: map[string]any{
					"name": "ship-update",
					"fqn":  "github://aileron/ship-update",
				},
			},
			want: "name=ship-update connector=github://aileron/ship-update",
		},
		{
			name: "binding-created",
			in: auditEventWire{
				EventType: "binding.created",
				Payload:   map[string]any{"connector_fqn": "github://aileron/slack"},
			},
			want: "connector=github://aileron/slack",
		},
		{
			name: "empty-payload",
			in:   auditEventWire{EventType: "unknown", Payload: map[string]any{}},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := auditPayloadSummary(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestFetchAuditList_BuildsURL: the production fetcher (not the stub)
// builds the right URL with all filter params and decodes the wire
// shape. Single test that exercises the actual HTTP path.
func TestFetchAuditList_BuildsURL(t *testing.T) {
	var sawQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	_, err := fetchAuditList(auditListQuery{
		since:     "2026-05-01T00:00:00Z",
		auditID:   "audit-x",
		connector: "github://aileron/slack",
		class:     "binding_required",
		limit:     25,
	})
	if err != nil {
		t.Fatalf("fetchAuditList: %v", err)
	}
	for k, want := range map[string]string{
		"since":         "2026-05-01T00:00:00Z",
		"audit_id":      "audit-x",
		"connector_fqn": "github://aileron/slack",
		"class":         "binding_required",
		"limit":         "25",
	} {
		if got := sawQuery.Get(k); got != want {
			t.Errorf("query[%s] = %q, want %q", k, got, want)
		}
	}
}
