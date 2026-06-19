package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ALRubinger/aileron/internal/launch"
)

// OpenCode is the agent definition for sst/opencode.
//
// Per ADR-0015 the launcher does not own the shell. OpenCode has a
// first-class `shell` config key (and a `permission.bash` setting),
// but we leave both untouched — OpenCode's own permission system stays
// in charge of its local exec.
//
// MCP wiring goes into project-local `opencode.json` (preferred per
// OpenCode's documented merge order: project config wins over global
// for the keys we set). ConfigureMCP returns no extra args.
type OpenCode struct{}

func (o OpenCode) Name() string          { return "opencode" }
func (o OpenCode) BinaryNames() []string { return []string{"opencode"} }

func (o OpenCode) Args(_ launch.Mode) []string { return nil }
func (o OpenCode) Env() map[string]string      { return nil }

// LLMEndpointEnv returns "" — OpenCode configures the LLM provider
// base URL per provider in `opencode.json`, not via a single env var.
func (o OpenCode) LLMEndpointEnv() string { return "" }

// ConfigureMCP merges an `aileron` entry into the `mcp` block of the
// project-local `opencode.json`. Existing keys in the file are
// preserved; `mcp.aileron` is overwritten on each launch so credential
// env stays fresh.
//
// Returns nil args — OpenCode reads MCP servers from the config file.
// Mode is irrelevant for OpenCode: dir is the launch directory, which
// the sandbox launcher already bind-mounts as the container's
// workspace, so the in-container OpenCode reads the file at the same
// relative path under both modes.
func (o OpenCode) ConfigureMCP(mcpBin string, mcpEnv map[string]string, dir string, _ launch.Mode) ([]string, []launch.MCPMount, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, nil, fmt.Errorf("determining working directory: %w", err)
		}
		dir = cwd
	}
	path := filepath.Join(dir, "opencode.json")

	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &root)
	}

	mcp, _ := root["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	entry := map[string]any{
		"type":    "local",
		"command": []string{mcpBin},
		"enabled": true,
	}
	if len(mcpEnv) > 0 {
		entry["environment"] = mcpEnv
	}
	mcp[launch.MCPServerName] = entry
	root["mcp"] = mcp

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling opencode config: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return nil, nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return nil, nil, nil
}

// AuthSpec returns OpenCode's vault-backed credential descriptor.
// OpenCode persists its provider credentials in a standalone
// `auth.json` under the XDG data dir, written by `opencode auth login`
// and loaded on startup. That is a rotatable on-disk credential file,
// so the binding is a FileBinding with byte-identity Render/Capture.
// Required is false so an unseeded vault falls through to OpenCode's
// in-container `auth login` bootstrap, which Capture then snapshots on
// clean exit. MountAsFile is false (the default): OpenCode's
// ConfigureMCP writes a project-local `opencode.json` in the workspace
// mount, not under the data dir, so no sibling mount needs to coexist
// with auth.json.
func (o OpenCode) AuthSpec() launch.AuthSpec {
	return launch.AuthSpec{
		FileBindings: []launch.FileBinding{{
			VaultPath:     opencodeVaultPath,
			ContainerPath: opencodeAuthContainerPath,
			Mode:          0o600,
			Required:      false,
			Render:        opencodeRender,
			Capture:       opencodeCapture,
		}},
	}
}
