package agents

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/launch"
)

// Goose is the agent definition for Block's Goose CLI.
//
// Per ADR-0015, Aileron is not the trust surface for Goose's local
// shell commands; Goose's developer-extension permission model
// (GOOSE_MODE=auto for autonomous; smart_approve/approve for prompted)
// stays in charge.
//
// MCP wiring goes into `~/.config/goose/config.yaml`'s `extensions:`
// block. Goose discovers MCP servers from this file at startup;
// ConfigureMCP returns no extra args.
type Goose struct{}

func (g Goose) Name() string          { return "goose" }
func (g Goose) BinaryNames() []string { return []string{"goose"} }

func (g Goose) Args() []string { return nil }

// Env sets GOOSE_MODE=auto so Goose runs without per-tool approval
// prompts. Users who want Goose's own approval prompts can override
// this in their shell or use a wrapper script.
func (g Goose) Env() map[string]string {
	return map[string]string{"GOOSE_MODE": "auto"}
}

// LLMEndpointEnv returns "" — Goose configures its LLM provider base
// URL in `config.yaml` per provider, not via a single env var.
// Gateway routing for Goose is not available under launch today.
func (g Goose) LLMEndpointEnv() string { return "" }

// ConfigureMCP merges an `aileron` entry into the `extensions:` block
// of `~/.config/goose/config.yaml`. Existing extensions and other
// top-level keys are preserved. Goose's extension config is a YAML
// object keyed by extension name; we use the `stdio` transport with
// the aileron-mcp binary's absolute path.
//
// Returns nil args — Goose reads MCP servers from the config file, not
// from CLI flags.
func (g Goose) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string) ([]string, error) {
	configDir, err := gooseConfigDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", configDir, err)
	}
	path := filepath.Join(configDir, "config.yaml")

	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = yaml.Unmarshal(data, &root)
	}

	extensions, _ := root["extensions"].(map[string]any)
	if extensions == nil {
		extensions = map[string]any{}
	}
	extensions[launch.MCPServerName] = map[string]any{
		"type":    "stdio",
		"enabled": true,
		"name":    launch.MCPServerName,
		"cmd":     mcpBin,
		"envs":    mcpEnv,
	}
	root["extensions"] = extensions

	out, err := yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshaling goose config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return nil, nil
}

// gooseConfigDir returns Goose's per-user config directory:
//
//   - macOS / Linux: `~/.config/goose` (Goose uses XDG on macOS too,
//     not `~/Library/Application Support`).
//   - Windows: `%APPDATA%\Block\goose`.
//
// `os.UserConfigDir` returns the wrong macOS path so we resolve XDG
// manually on Unix-likes.
func gooseConfigDir() (string, error) {
	if runtimeIsWindows() {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("determining user config dir: %w", err)
		}
		return filepath.Join(cfg, "Block", "goose"), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "goose"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}
	return filepath.Join(home, ".config", "goose"), nil
}
