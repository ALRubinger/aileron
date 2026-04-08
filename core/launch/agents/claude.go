package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Claude is the agent definition for Claude Code.
type Claude struct{}

func (c Claude) Name() string           { return "claude" }
func (c Claude) BinaryNames() []string  { return []string{"claude"} }
func (c Claude) Env() map[string]string { return nil }

// SetupHooks writes Aileron's PreToolUse hook into the project's
// .claude/settings.local.json so Claude Code trusts it without prompting.
// The hook is removed on cleanup.
func (c Claude) SetupHooks(shimPath string) ([]string, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, fmt.Errorf("getting working directory: %w", err)
	}

	configDir := filepath.Join(cwd, ".claude")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating .claude dir: %w", err)
	}

	configPath := filepath.Join(configDir, "settings.local.json")

	// Read existing settings if present.
	existing := make(map[string]any)
	if data, err := os.ReadFile(configPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	// Save original for cleanup.
	origData, hadOriginal := readFileIfExists(configPath)

	// Merge our hook config into existing settings.
	existing["hooks"] = map[string]any{
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
	}

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling hook config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return nil, nil, fmt.Errorf("writing hook config: %w", err)
	}

	cleanup := func() {
		if hadOriginal {
			os.WriteFile(configPath, origData, 0o644)
		} else {
			os.Remove(configPath)
		}
	}

	return []string{"--allowedTools", "Bash(*)"}, cleanup, nil
}

func readFileIfExists(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}
