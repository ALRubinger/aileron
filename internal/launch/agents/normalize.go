package agents

// NormalizeCommand looks up the agent by name and delegates to its
// NormalizeCommand method. Unknown agents pass the command through
// for policy evaluation unchanged.
//
// This function bridges the Agent interface to the aileron-sh binary,
// which only knows the agent name (from AILERON_AGENT env var) and
// cannot hold a reference to the Agent struct directly.
func NormalizeCommand(agent, command string) (string, bool) {
	if a, ok := registry[agent]; ok {
		return a.NormalizeCommand(command)
	}
	return command, true
}

// registry maps agent names to their instances for normalization lookup.
// Each agent registers itself here so aileron-sh can find it by name.
var registry = map[string]normalizer{
	"claude": Claude{},
	"pi":     Pi{},
}

// normalizer is the subset of Agent needed for command normalization.
type normalizer interface {
	NormalizeCommand(raw string) (string, bool)
}
