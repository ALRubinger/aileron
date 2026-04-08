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
// Uses the hookSpecificOutput format so Claude Code displays custom messages.
type HookOutput struct {
	HookSpecificOutput *HookDecision `json:"hookSpecificOutput,omitempty"`
}

// HookDecision is the Claude Code PreToolUse decision format.
type HookDecision struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`                 // "approve", "deny", "ask"
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"` // shown to user
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

	// Only evaluate Bash tool calls. Non-Bash tools pass through.
	if input.ToolName != "Bash" {
		writeHookOutput(stdout, "allow", "")
		return 0
	}

	command := input.ToolInput.Command
	if command == "" {
		writeHookOutput(stdout, "allow", "")
		return 0
	}

	// Find policy file from the working directory.
	policyPath := FindPolicyFile(input.CWD)
	if policyPath == "" {
		// No policy file — don't interfere.
		writeHookOutput(stdout, "allow", "")
		return 0
	}

	result := EvaluateCommand(policyPath, command, input.CWD)

	switch result.Disposition {
	case model.DispositionAllow:
		// "allow" combined with --allowedTools "Bash(*)" auto-approves.
		writeHookOutput(stdout, "allow", "")
	case model.DispositionDeny:
		writeHookOutput(stdout, "deny", result.Reason)
	case model.DispositionRequireApproval:
		// "ask" takes precedence over --allowedTools (ask > allow in hook
		// precedence), so this will prompt the user despite auto-approve.
		writeHookOutput(stdout, "ask", result.Reason)
	default:
		writeHookOutput(stdout, "ask", "")
	}

	return 0
}

func writeHookOutput(w io.Writer, decision, reason string) {
	if reason == "" && decision == "deny" {
		reason = "aileron: blocked by policy"
	}
	if reason != "" {
		reason = "aileron: " + reason
	}
	output := HookOutput{
		HookSpecificOutput: &HookDecision{
			HookEventName:            "PreToolUse",
			PermissionDecision:       decision,
			PermissionDecisionReason: reason,
		},
	}
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
