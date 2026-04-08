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

// SetupHooks writes a Claude Code settings file that registers aileron-sh
// as a PreToolUse hook for the Bash tool. Returns --settings-file args
// so Claude Code loads the hook config.
func (c Claude) SetupHooks(shimPath string) ([]string, func(), error) {
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
		return nil, nil, fmt.Errorf("marshaling hook config: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "aileron-hooks-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating temp dir: %w", err)
	}

	configPath := filepath.Join(tmpDir, "settings.json")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return nil, nil, fmt.Errorf("writing hook config: %w", err)
	}

	cleanup := func() { os.RemoveAll(tmpDir) }
	return []string{"--settings", configPath}, cleanup, nil
}
