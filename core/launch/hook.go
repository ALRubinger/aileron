package launch

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ALRubinger/aileron/core/model"
)

// HookInput is the JSON structure received from Claude Code's PreToolUse hook.
type HookInput struct {
	SessionID     string    `json:"session_id"`
	CWD           string    `json:"cwd"`
	HookEventName string    `json:"hook_event_name"`
	ToolName      string    `json:"tool_name"`
	ToolInput     ToolInput `json:"tool_input"`
}

// ToolInput holds the Bash tool parameters from Claude Code.
type ToolInput struct {
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	Timeout     int    `json:"timeout,omitempty"`
}

// HookOutput is the JSON structure returned to Claude Code.
type HookOutput struct {
	Decision string `json:"decision"`          // "approve", "deny", "ask"
	Reason   string `json:"reason,omitempty"`   // shown to user on deny
}

// RunHook reads a Claude Code PreToolUse hook request from stdin,
// evaluates the command against aileron.yaml policy, and writes the
// decision to stdout. Returns the exit code.
func RunHook(stdin io.Reader, stdout io.Writer) int {
	var input HookInput
	if err := json.NewDecoder(stdin).Decode(&input); err != nil {
		fmt.Fprintf(os.Stderr, "aileron-sh: failed to parse hook input: %v\n", err)
		return 1
	}

	// Only evaluate Bash tool calls.
	if input.ToolName != "Bash" {
		writeHookOutput(stdout, "approve", "")
		return 0
	}

	command := input.ToolInput.Command
	if command == "" {
		writeHookOutput(stdout, "approve", "")
		return 0
	}

	// Find policy file from the working directory.
	policyPath := FindPolicyFile(input.CWD)
	if policyPath == "" {
		// No policy file — don't interfere.
		writeHookOutput(stdout, "approve", "")
		return 0
	}

	result := EvaluateCommand(policyPath, command, input.CWD)

	switch result.Disposition {
	case model.DispositionAllow:
		writeHookOutput(stdout, "approve", "")
	case model.DispositionDeny:
		writeHookOutput(stdout, "deny", result.Reason)
	case model.DispositionRequireApproval:
		// "ask" defers to Claude Code's native approval prompt.
		writeHookOutput(stdout, "ask", result.Reason)
	default:
		writeHookOutput(stdout, "ask", "")
	}

	return 0
}

func writeHookOutput(w io.Writer, decision, reason string) {
	output := HookOutput{Decision: decision, Reason: reason}
	json.NewEncoder(w).Encode(output)
}

// WriteHookConfig writes a Claude Code settings file that registers
// aileron-sh as a PreToolUse hook for the Bash tool. Returns the path
// to the settings file.
func WriteHookConfig(shimPath, dir string) (string, error) {
	config := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []map[string]any{
				{
					"matcher": "Bash",
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": shimPath + " --hook",
						},
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling hook config: %w", err)
	}

	configDir := filepath.Join(dir, ".aileron")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", fmt.Errorf("creating config dir: %w", err)
	}

	configPath := filepath.Join(configDir, "claude-hooks.json")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return "", fmt.Errorf("writing hook config: %w", err)
	}

	return configPath, nil
}
