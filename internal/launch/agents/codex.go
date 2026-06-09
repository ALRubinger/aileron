package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch"
)

// Codex is the agent definition for OpenAI Codex CLI.
//
// Codex CLI does not honour $SHELL — it resolves the user's shell via
// `getpwuid_r` and validates the binary name against a fixed allowlist
// (bash/zsh/sh/pwsh/powershell/cmd). The shell-shim interception model
// is impossible without an upstream patch. Per ADR-0015 the launcher
// no longer attempts shell interception for any agent; Codex's own
// sandbox + approval-policy machinery stays in charge of its local
// exec, and Aileron mediates only the actions the agent calls through
// aileron-mcp + the gateway.
//
// MCP wiring is written into `~/.codex/config.toml` under
// `[mcp_servers.aileron]` — Codex reads MCP servers from this file, not
// from a CLI flag, so ConfigureMCP returns no extra args.
type Codex struct{}

func (c Codex) Name() string          { return "codex" }
func (c Codex) BinaryNames() []string { return []string{"codex"} }

// Args returns no extra arguments. Approval policy + sandbox mode for
// Codex live in `~/.codex/config.toml` (e.g. `approval_policy = "never"`
// for fully autonomous runs); we do not override them at launch time.
// Users who want Codex's own approval prompt suppressed can set that in
// their config or pass `--dangerously-bypass-approvals-and-sandbox`
// themselves.
func (c Codex) Args() []string { return nil }

func (c Codex) Env() map[string]string { return nil }

// LLMEndpointEnv returns the env var Codex CLI reads to override the
// OpenAI API base URL. Routing Codex through Aileron's gateway only
// applies on the API-key auth path; sessions authenticated through
// ChatGPT login do not honour this var and run directly against
// OpenAI.
func (c Codex) LLMEndpointEnv() string { return "OPENAI_BASE_URL" }

// ConfigureMCP writes (or merges) a `[mcp_servers.aileron]` entry into
// `~/.codex/config.toml`. Codex reads MCP servers from config.toml at
// startup; passing them via CLI is not supported. Returns nil args.
//
// Mode branches the destination:
//   - ModeHost: writes the launcher's host `~/.codex/config.toml`,
//     preserving the rest of the file via mergeCodexMCPBlock.
//   - ModeSandbox: writes the generated [mcp_servers.aileron] block
//     to an os.MkdirTemp config.toml and returns a Volume bind-
//     mounting it into the container at /home/agent/.codex/config.toml.
//     The host `~/.codex/config.toml` is never touched. The in-
//     container Codex reads this file at startup. See ADR-0024.
//
// Sandbox mode emits ONLY the [mcp_servers.aileron] block — no merge
// against a host-side config — so any other [mcp_servers.foo] entries
// the user has on the host don't leak into the container. Users
// wanting extra MCP servers under Codex+sandbox configure them via
// their devcontainer or via a wrapper merge; the manual recipe at
// docs/development/sandbox-mcp-walkthrough.md documents the limitation.
func (c Codex) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string, mode launch.Mode) ([]string, []launch.MCPMount, error) {
	if mode == launch.ModeSandbox {
		return c.configureSandboxMCP(mcpBin, mcpEnv)
	}
	return c.configureHostMCP(mcpBin, mcpEnv)
}

func (c Codex) configureHostMCP(mcpBin string, mcpEnv map[string]string) ([]string, []launch.MCPMount, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("determining home directory: %w", err)
	}
	configDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating %s: %w", configDir, err)
	}
	path := filepath.Join(configDir, "config.toml")

	existing, _ := os.ReadFile(path)
	merged := mergeCodexMCPBlock(string(existing), mcpBin, mcpEnv)
	if err := os.WriteFile(path, []byte(merged), 0o600); err != nil {
		return nil, nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return nil, nil, nil
}

// codexSandboxConfigContainerPath is where Codex reads its config from
// inside the sandbox container.
const codexSandboxConfigContainerPath = "/home/agent/.codex/config.toml"

func (c Codex) configureSandboxMCP(mcpBin string, mcpEnv map[string]string) ([]string, []launch.MCPMount, error) {
	dir, err := os.MkdirTemp("", "aileron-codex-sandbox-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating codex sandbox config tempdir: %w", err)
	}
	path := filepath.Join(dir, "config.toml")
	// Sandbox mode emits only our [mcp_servers.aileron] block — no
	// merge with a host-side config. The empty-string baseline mirrors
	// what mergeCodexMCPBlock produces when called with no prior file.
	body := mergeCodexMCPBlock("", mcpBin, mcpEnv)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return nil, nil, fmt.Errorf("writing codex sandbox config: %w", err)
	}
	mount := launch.MCPMount{
		Source:   path,
		Target:   codexSandboxConfigContainerPath,
		ReadOnly: true,
	}
	return nil, []launch.MCPMount{mount}, nil
}

// mergeCodexMCPBlock replaces existing [mcp_servers.aileron] and
// [mcp_servers.aileron.env] blocks (or appends them) in the given
// config.toml content. The function does not fully parse the rest of
// the file — it scans line-by-line for the section headers, removes
// matching blocks, and emits the rewritten blocks at the end. Other
// sections and comments are preserved.
func mergeCodexMCPBlock(content, mcpBin string, mcpEnv map[string]string) string {
	prefix := "[mcp_servers." + launch.MCPServerName
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+8)
	skip := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		isHeader := strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]")
		// Start skipping any block whose header begins with our prefix
		// (covers both [mcp_servers.aileron] and [mcp_servers.aileron.env]).
		if isHeader && strings.HasPrefix(trim, prefix) {
			skip = true
			continue
		}
		if skip {
			if isHeader {
				skip = false
				out = append(out, line)
				continue
			}
			continue
		}
		out = append(out, line)
	}
	// Trim trailing blank lines so the appended block sits cleanly.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	out = append(out, "[mcp_servers."+launch.MCPServerName+"]")
	out = append(out, fmt.Sprintf("command = %q", mcpBin))
	if len(mcpEnv) > 0 {
		out = append(out, "")
		out = append(out, "[mcp_servers."+launch.MCPServerName+".env]")
		keys := make([]string, 0, len(mcpEnv))
		for k := range mcpEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, fmt.Sprintf("%s = %q", k, mcpEnv[k]))
		}
	}
	out = append(out, "")
	return strings.Join(out, "\n")
}
