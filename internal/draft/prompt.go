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

// Prompts holds the prompts loaded from AILERON.md.
type Prompts struct {
	Research         string // Round 1: context gathering with tools
	IntentResolution string // Round 2: classify intent and extract parameters
	Ghostwrite       string // Round 3: writing the reply (informational only)
}

// LoadPrompts reads AILERON.md and splits it into research, intent resolution,
// and ghostwrite prompts. The file uses "---" line separators between sections.
// Order: research → intent resolution → ghostwrite.
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

	// Split on "---" separators. Supports 2 or 3 sections for backward compat.
	parts := strings.Split(content, "\n---\n")

	switch len(parts) {
	case 3:
		// Full 3-section format: research → intent resolution → ghostwrite
		return Prompts{
			Research:         strings.TrimSpace(parts[0]),
			IntentResolution: strings.TrimSpace(parts[1]),
			Ghostwrite:       strings.TrimSpace(parts[2]),
		}
	case 2:
		// Legacy 2-section format: research → ghostwrite (no intent resolution)
		return Prompts{
			Research:   strings.TrimSpace(parts[0]),
			Ghostwrite: strings.TrimSpace(parts[1]),
		}
	default:
		// Single section — use as ghostwrite prompt.
		return Prompts{Ghostwrite: content}
	}
}

// LoadSystemPrompt is a backward-compatible wrapper that returns the
// ghostwrite prompt.
func LoadSystemPrompt() string {
	return LoadPrompts().Ghostwrite
}
