// Command publishedagents prints the agents whose per-agent sandbox image is
// publishable (composition.PublishedAgents) as a JSON array, one source of truth
// for the sandbox-agents.yml CI build matrix.
//
// PublishedAgents excludes agents whose CLI license forbids redistribution (e.g.
// Claude Code, @anthropic-ai/claude-code, all-rights-reserved), so driving the
// matrix off this command guarantees CI never builds or publishes a
// non-redistributable per-agent image (#1451). A new publishable agent is added
// to the matrix automatically by gaining a publishable recipe; nothing in the
// workflow YAML hardcodes the agent list.
//
// Usage:
//
//	go run ./tools/publishedagents
//
// prints e.g. ["codex"] to stdout.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ALRubinger/aileron/internal/sandbox/composition"
)

func main() {
	agents := composition.PublishedAgents()
	if agents == nil {
		agents = []string{}
	}
	data, err := json.Marshal(agents)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal published agents: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}
