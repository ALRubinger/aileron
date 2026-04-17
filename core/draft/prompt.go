package draft

import (
	"fmt"
	"os"
	"strings"
)

// promptSource records where the system prompt was loaded from.
var promptSource string

// PromptSource returns the path the system prompt was loaded from.
func PromptSource() string {
	return promptSource
}

// LoadSystemPrompt reads the AILERON.md file and returns its contents as the
// system prompt. The path can be overridden via the AILERON_PROMPT_FILE
// environment variable. Panics if the file is missing or empty — the system
// prompt is required for draft generation and must not silently fall back to
// a hardcoded default.
func LoadSystemPrompt() string {
	path := os.Getenv("AILERON_PROMPT_FILE")
	if path == "" {
		path = "AILERON.md"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("AILERON.md not found at %q: %v — the system prompt is required for draft generation", path, err))
	}

	content := strings.TrimSpace(string(data))
	if content == "" {
		panic(fmt.Sprintf("AILERON.md at %q is empty — the system prompt is required for draft generation", path))
	}

	promptSource = path
	return content
}
