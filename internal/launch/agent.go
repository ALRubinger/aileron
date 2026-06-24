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

// Mode discriminates host launch from sandbox launch in the agent
// configuration contract. ADR-0024's sandbox MCP revival requires
// agents that write host-side config (Codex's ~/.codex/config.toml) to
// branch on this — for the in-container agent process, the host path
// is irrelevant and the launcher must bind-mount a generated config
// into the container filesystem instead.
type Mode string

const (
	// ModeHost is the direct host-launch path: the agent process runs
	// on the host, mcpBin is a host filesystem path, and config files
	// the agent reads live in the host's home directory.
	ModeHost Mode = "host"
	// ModeSandbox is the v4 container launch path: the agent process
	// runs inside the sandbox container, mcpBin is the container-side
	// path (typically /usr/local/bin/aileron-mcp), and config files the
	// agent reads must be reachable from inside the container — either
	// in the bind-mounted workspace dir or via an additional bind mount
	// returned by ConfigureMCP.
	ModeSandbox Mode = "sandbox"
)

// MCPMount is a host-to-container bind mount an agent's ConfigureMCP
// can request when running under ModeSandbox. The launcher converts
// this into the runtime's volume argument when starting the container.
// Agents in ModeHost should return no mounts.
type MCPMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// Agent describes a launchable AI coding agent.
type Agent interface {
	// Name returns the agent identifier used in CLI args (e.g. "claude").
	Name() string
	// BinaryNames returns candidate binary names to search on PATH, in
	// preference order. For example, ["claude"] or ["codex", "openai-codex"].
	BinaryNames() []string
	// Args returns CLI arguments the agent requires. These are prepended
	// before any user-supplied arguments.
	//
	// mode is host vs sandbox. Agents whose approval posture differs by
	// trust boundary branch on it: under ModeSandbox the container is the
	// trust boundary (ADR-0015), so an agent may pass a broader
	// auto-approve flag than under ModeHost. Agents whose flags are the
	// same in both modes ignore the parameter.
	Args(mode Mode) []string
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
	// mcpBin is the path to the aileron-mcp binary (host path under
	// ModeHost, container-side path under ModeSandbox).
	// mcpEnv is the environment the MCP server needs (AILERON_URL,
	// AILERON_SESSION_ID, etc.) — agents that write the config file
	// must persist these in the file's env block; agents that pass via
	// CLI must serialize them into their flag value.
	// dir is the launch working directory (project root for agents that
	// write per-project config like OpenCode); empty means "use cwd".
	// mode is host vs sandbox; agents that materialize config to a
	// host path under ModeHost must redirect that write to a temp host
	// path under ModeSandbox and return a Volume bind-mounting it into
	// the container at the path the in-container agent reads. See
	// ADR-0024 for the contract change.
	ConfigureMCP(mcpBin string, mcpEnv map[string]string, dir string, mode Mode) ([]string, []MCPMount, error)
	// AuthSpec returns the agent's declarative description of its
	// credential bindings. The launcher consumes the spec to render
	// vault entries into the container at launch (env vars, files in
	// a writable bind-mount) and to snapshot in-container rotations
	// back to the vault on clean exit. See [AuthSpec] for the
	// lifecycle and ADR-0025 for the design.
	//
	// Agents that have no vault-backed credentials return the zero
	// value AuthSpec{}; the launcher treats that as a no-op.
	AuthSpec() AuthSpec
	// SkillsPath returns the container-side directory the agent reads
	// installed Agent Skills from (e.g. "/home/agent/.claude/skills" for
	// Claude Code). At sandbox launch the launcher bind-mounts the
	// canonical host-side skill store read-only at this path, so a skill
	// installed once via `aileron skill install` is visible to the agent
	// wherever it launches (the install-once / launch-anywhere projection,
	// #1508). Per-agent path differences are absorbed here, not by
	// transpiling SKILL.md.
	//
	// An agent whose skills directory is not yet grounded returns "", and
	// the launcher skips the skills mount for it. Projection is a v4
	// container-model behavior: host launch (--local) lets the agent read
	// its own native skills directory and the launcher does not mount over
	// it.
	SkillsPath() string
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
