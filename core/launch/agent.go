// Package launch provides the launcher for AI coding agents under Aileron's
// policy-enforced shell.
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
	// process, beyond the standard SHELL/AILERON_REAL_SHELL manipulation.
	// Returns nil if no extra env is needed.
	Env() map[string]string
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
