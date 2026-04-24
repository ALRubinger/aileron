package draft

import (
	"fmt"
	"os"
	"strings"
)

// promptSource records where the prompts were loaded from.
var promptSource string

// PromptSource returns the path the prompts were loaded from.
func PromptSource() string {
	return promptSource
}

// Prompts holds the two prompts loaded from AILERON.md.
type Prompts struct {
	Research   string // Round 1: context gathering with tools
	Ghostwrite string // Round 2: writing the reply in the user's voice
}

// LoadPrompts reads AILERON.md and splits it into research and ghostwrite
// prompts. The file uses a "---" line separator between sections.
// Research prompt comes first, ghostwrite prompt second.
// Panics if the file is missing or empty.
func LoadPrompts() Prompts {
	path := os.Getenv("AILERON_PROMPT_FILE")
	if path == "" {
		path = "AILERON.md"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("AILERON.md not found at %q: %v — required for draft generation", path, err))
	}

	content := strings.TrimSpace(string(data))
	if content == "" {
		panic(fmt.Sprintf("AILERON.md at %q is empty — required for draft generation", path))
	}

	promptSource = path

	// Split on "---" separator between research and ghostwrite prompts.
	parts := strings.SplitN(content, "\n---\n", 2)
	if len(parts) != 2 {
		// Single section — use as ghostwrite prompt, no research prompt.
		return Prompts{Ghostwrite: content}
	}

	return Prompts{
		Research:   strings.TrimSpace(parts[0]),
		Ghostwrite: strings.TrimSpace(parts[1]),
	}
}

// LoadSystemPrompt is a backward-compatible wrapper that returns the
// ghostwrite prompt.
func LoadSystemPrompt() string {
	return LoadPrompts().Ghostwrite
}
