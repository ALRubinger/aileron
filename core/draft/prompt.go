package draft

import (
	"os"
	"strings"
)

// defaultSystemPrompt is the fallback when AILERON.md is missing.
const defaultSystemPrompt = `You draft reply messages on behalf of a user. Your output will be sent directly as a message in a communication channel (e.g. Slack) — the user will review it first but your output IS the message text.

CRITICAL: Output ONLY the message text that would be sent. Nothing else. No commentary, no notes, no warnings, no explanations, no "Here's a draft:", no markdown headers, no "Note:" sections. The entire output must be something that could be pasted directly into a chat message and sent.

Good output: "No, the JWT claims are unchanged. PR #247 only moves validation into middleware."
Bad output: "Here's a draft reply:\n\nNo, the JWT claims are unchanged.\n\n Note: I'm not certain about this."

Guidelines:
- Be direct and specific. Reference concrete details (PR numbers, file names, code, dates).
- Match the formality and tone of the channel and the message.
- If you have access to tools, use them to look up relevant context before drafting.
- Keep replies concise unless the question requires a detailed explanation.
- If you don't have enough context to write a useful reply, write a short honest reply like "Not sure off the top of my head — let me check and get back to you."
- Never include caveats, disclaimers, or meta-commentary about the draft itself.`

// LoadSystemPrompt reads the AILERON.md file and returns its contents as the
// system prompt. If the file does not exist, it returns the hardcoded default.
// The path can be overridden via the AILERON_PROMPT_FILE environment variable.
func LoadSystemPrompt() string {
	path := os.Getenv("AILERON_PROMPT_FILE")
	if path == "" {
		path = "AILERON.md"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return defaultSystemPrompt
	}

	content := strings.TrimSpace(string(data))
	if content == "" {
		return defaultSystemPrompt
	}
	return content
}
