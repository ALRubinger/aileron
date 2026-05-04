package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Pi is the agent definition for the pi.dev coding agent.
// Pi ignores $SHELL and resolves its shell via its own settings file,
// so ConfigureShell writes .pi/settings.json with the shim path.
type Pi struct{}

func (p Pi) Name() string          { return "pi" }
func (p Pi) BinaryNames() []string { return []string{"pi"} }

// Args returns the flags needed to make Pi auto-approve shell commands
// so that Aileron is the single approval layer. Pi's --tools flag
// controls which built-in tools are enabled.
func (p Pi) Args() []string {
	return []string{"--tools", "bash,read,edit,write,grep,find,ls"}
}

func (p Pi) Env() map[string]string {
	return map[string]string{
		"AILERON_REAL_SHELL": "/bin/bash",
	}
}

// LLMEndpointEnv returns "" — Pi resolves its API endpoint via its
// settings file rather than an environment variable, so launch does not
// route Pi's LLM calls through the embedded gateway today. Adding gateway
// support for Pi would require extending ConfigureShell to write the
// gateway URL into .pi/settings.json.
func (p Pi) LLMEndpointEnv() string { return "" }

// NormalizeCommand returns the command as-is for policy evaluation.
// Pi does not wrap commands in a template like Claude Code does.
func (p Pi) NormalizeCommand(raw string) (string, bool) {
	return raw, true
}

// ConfigureShell writes a project-local .pi/settings.json that points
// Pi's shellPath at aileron-sh. Pi ignores $SHELL and resolves its
// shell from this settings file instead.
func (p Pi) ConfigureShell(shimPath, dir string) error {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("determining working directory: %w", err)
		}
	}

	piDir := filepath.Join(dir, ".pi")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return fmt.Errorf("creating .pi directory: %w", err)
	}

	settingsPath := filepath.Join(piDir, "settings.json")

	// Read existing settings to preserve other fields.
	existing := make(map[string]any)
	if data, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(data, &existing)
	}

	existing["shellPath"] = shimPath

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling Pi settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", settingsPath, err)
	}

	return nil
}
