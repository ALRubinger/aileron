package launch_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestRunHook_AllowedCommand(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: deny
allow:
  - "echo *"
`), 0o644)

	input := hookInput("Bash", "echo hello", dir)
	var stdout bytes.Buffer
	code := launch.RunHook(strings.NewReader(input), &stdout)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	// Allow = no output (silent, defers to --allowedTools).
	if stdout.Len() != 0 {
		t.Errorf("expected no output for allow, got %q", stdout.String())
	}
}

func TestRunHook_DeniedCommand(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
deny:
  - command: "deploy --force *"
    description: "no force deploys"
`), 0o644)

	input := hookInput("Bash", "deploy --force production", dir)
	var stdout bytes.Buffer
	code := launch.RunHook(strings.NewReader(input), &stdout)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	assertHookDecision(t, stdout.String(), "deny")
	assertHookReason(t, stdout.String(), "aileron: no force deploys")
}

func TestRunHook_AskCommand(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: ask
`), 0o644)

	// Use a command not in any built-in allow or deny list.
	input := hookInput("Bash", "docker run myimage", dir)
	var stdout bytes.Buffer
	launch.RunHook(strings.NewReader(input), &stdout)

	assertHookDecision(t, stdout.String(), "ask")
}

func TestRunHook_NonBashTool(t *testing.T) {
	input := hookInput("Read", "/some/file", "/tmp")
	var stdout bytes.Buffer
	launch.RunHook(strings.NewReader(input), &stdout)

	if stdout.Len() != 0 {
		t.Errorf("expected no output for non-Bash tool, got %q", stdout.String())
	}
}

func TestRunHook_NoPolicyFile(t *testing.T) {
	dir := t.TempDir()

	input := hookInput("Bash", "rm -rf /", dir)
	var stdout bytes.Buffer
	launch.RunHook(strings.NewReader(input), &stdout)

	if stdout.Len() != 0 {
		t.Errorf("expected no output without policy, got %q", stdout.String())
	}
}

func TestRunHook_EmptyCommand(t *testing.T) {
	input := hookInput("Bash", "", "/tmp")
	var stdout bytes.Buffer
	launch.RunHook(strings.NewReader(input), &stdout)

	if stdout.Len() != 0 {
		t.Errorf("expected no output for empty command, got %q", stdout.String())
	}
}

func TestRunHook_InvalidJSON(t *testing.T) {
	var stdout bytes.Buffer
	code := launch.RunHook(strings.NewReader("not json"), &stdout)
	if code != 1 {
		t.Errorf("expected exit code 1 for invalid JSON, got %d", code)
	}
}

func hookInput(toolName, command, cwd string) string {
	input := map[string]any{
		"session_id":      "test",
		"cwd":             cwd,
		"hook_event_name": "PreToolUse",
		"tool_name":       toolName,
		"tool_input": map[string]any{
			"command": command,
		},
	}
	data, _ := json.Marshal(input)
	return string(data)
}

func assertHookDecision(t *testing.T, output, expected string) {
	t.Helper()
	var result struct {
		HookSpecificOutput struct {
			PermissionDecision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &result); err != nil {
		t.Fatalf("failed to parse hook output %q: %v", output, err)
	}
	if result.HookSpecificOutput.PermissionDecision != expected {
		t.Errorf("decision = %q, want %q", result.HookSpecificOutput.PermissionDecision, expected)
	}
}

func assertHookReason(t *testing.T, output, expected string) {
	t.Helper()
	var result struct {
		HookSpecificOutput struct {
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &result); err != nil {
		t.Fatalf("failed to parse hook output %q: %v", output, err)
	}
	if result.HookSpecificOutput.PermissionDecisionReason != expected {
		t.Errorf("reason = %q, want %q", result.HookSpecificOutput.PermissionDecisionReason, expected)
	}
}
