// Package launch provides the launcher for AI coding agents under
// Aileron's daemon. Per ADR-0015, the launcher's job is to:
//
//  1. Resolve the daemon and register a session.
//  2. Route the agent's LLM traffic through the daemon's gateway (when
//     the agent exposes an env-controllable base URL).
//  3. Register aileron-mcp with the agent so Aileron's tools are
//     callable as mcp__aileron__*.
//
// The launcher does not replace $SHELL, install wrapper scripts, write
// policy files, or audit shell commands the agent runs locally. The
// audit boundary is "actions Aileron executes" (ADR-0010 audit store),
// not "every command the agent runs."
package launch

import "sort"

// Agent describes a launchable AI coding agent.
type Agent interface {
	// Name returns the agent identifier used in CLI args (e.g. "claude").
	Name() string
	// BinaryNames returns candidate binary names to search on PATH, in
	// preference order. For example, ["claude"] or ["codex", "openai-codex"].
	BinaryNames() []string
	// Args returns CLI arguments the agent requires. These are prepended
	// before any user-supplied arguments.
	Args() []string
	// Env returns additional environment variables to set for the agent
	// process. Returns nil if no extra env is needed.
	Env() map[string]string
	// LLMEndpointEnv returns the name of the environment variable the
	// agent's LLM client honours to override its default API endpoint
	// (e.g. "ANTHROPIC_BASE_URL" for Claude Code, "OPENAI_BASE_URL" for
	// Codex CLI). Returns "" when the agent does not support endpoint
	// override via env (some agents resolve the endpoint from a settings
	// file; gateway routing is not available for those agents under launch).
	LLMEndpointEnv() string
	// ConfigureMCP arranges for the agent to discover Aileron's MCP
	// server. Agents that accept MCP wiring on the CLI (Claude Code's
	// --mcp-config, Goose's session --with-extension) return extra
	// args; agents that read MCP server configuration from a config
	// file (Codex's ~/.codex/config.toml, OpenCode's opencode.json)
	// write the file and return nil args.
	//
	// mcpBin is the absolute path to the aileron-mcp binary.
	// mcpEnv is the environment the MCP server needs (AILERON_URL,
	// AILERON_SESSION_ID, etc.) — agents that write the config file
	// must persist these in the file's env block; agents that pass via
	// CLI must serialize them into their flag value.
	// dir is the launch working directory (project root for agents that
	// write per-project config like OpenCode); empty means "use cwd".
	ConfigureMCP(mcpBin string, mcpEnv map[string]string, dir string) ([]string, error)
}

// Registry maps agent names to their definitions.
type Registry struct {
	agents map[string]Agent
}

// NewRegistry creates an empty agent registry.
func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]Agent)}
}

// Register adds an agent to the registry.
func (r *Registry) Register(a Agent) {
	r.agents[a.Name()] = a
}

// Get retrieves an agent by name.
func (r *Registry) Get(name string) (Agent, bool) {
	a, ok := r.agents[name]
	return a, ok
}

// Names returns all registered agent names in sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
