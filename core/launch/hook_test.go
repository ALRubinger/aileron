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
	assertHookDecision(t, stdout.String(), "approve")
}

func TestRunHook_DeniedCommand(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
deny:
  - command: "rm -rf *"
    description: "no recursive delete"
`), 0o644)

	input := hookInput("Bash", "rm -rf /important", dir)
	var stdout bytes.Buffer
	code := launch.RunHook(strings.NewReader(input), &stdout)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	assertHookDecision(t, stdout.String(), "deny")
}

func TestRunHook_AskCommand(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: ask
`), 0o644)

	input := hookInput("Bash", "git push origin main", dir)
	var stdout bytes.Buffer
	launch.RunHook(strings.NewReader(input), &stdout)

	assertHookDecision(t, stdout.String(), "ask")
}

func TestRunHook_NonBashTool(t *testing.T) {
	input := hookInput("Read", "/some/file", "/tmp")
	var stdout bytes.Buffer
	launch.RunHook(strings.NewReader(input), &stdout)

	assertHookDecision(t, stdout.String(), "approve")
}

func TestRunHook_NoPolicyFile(t *testing.T) {
	dir := t.TempDir() // no aileron.yaml

	input := hookInput("Bash", "rm -rf /", dir)
	var stdout bytes.Buffer
	launch.RunHook(strings.NewReader(input), &stdout)

	// No policy = don't interfere.
	assertHookDecision(t, stdout.String(), "approve")
}

func TestRunHook_EmptyCommand(t *testing.T) {
	input := hookInput("Bash", "", "/tmp")
	var stdout bytes.Buffer
	launch.RunHook(strings.NewReader(input), &stdout)

	assertHookDecision(t, stdout.String(), "approve")
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
	var result map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &result); err != nil {
		t.Fatalf("failed to parse hook output %q: %v", output, err)
	}
	if result["decision"] != expected {
		t.Errorf("decision = %q, want %q", result["decision"], expected)
	}
}
