package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch/agents"
	"golang.org/x/term"
)

// isTTYFn reports whether the CLI is attached to an interactive terminal on
// stdin. It is a package-level seam so tests can force the prompt path
// (true) or the non-interactive default path (false) without a real TTY.
var isTTYFn = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// resolveClaudeAuthMode selects the Claude auth mode to thread onto the
// launched Claude agent (#1340). It only performs mode *selection*; the
// AuthSpec branching for the selected mode lives in agents.Claude.AuthSpec.
//
// Resolution rules:
//   - flagVal == "api-key"      -> ClaudeAuthModeAPIKey
//   - flagVal == "subscription" -> ClaudeAuthModeSubscription
//   - flagVal is any other non-empty value -> usage error (fail fast)
//   - flagVal == "" && isTTY    -> interactive first-run prompt (reads one
//     line from stdin, re-asking on unrecognized input)
//   - flagVal == "" && !isTTY   -> ClaudeAuthModeSubscription WITHOUT reading
//     stdin (the non-interactive default; stdin is left untouched so a piped
//     stdin destined for the agent is never consumed).
func resolveClaudeAuthMode(flagVal string, isTTY bool, stdin io.Reader, stdout io.Writer) (agents.ClaudeAuthMode, error) {
	if flagVal != "" {
		mode, ok := parseClaudeAuthMode(flagVal)
		if !ok {
			return 0, fmt.Errorf("invalid --claude-auth value %q: want %q or %q", flagVal, "subscription", "api-key")
		}
		return mode, nil
	}
	if !isTTY {
		// Non-interactive: default to subscription and DO NOT read stdin.
		return agents.ClaudeAuthModeSubscription, nil
	}
	return promptClaudeAuthMode(stdin, stdout)
}

// parseClaudeAuthMode maps an explicit flag value to a mode. It accepts the
// two canonical spellings plus a small set of obvious synonyms so a hurried
// invocation is not rejected. The canonical, documented values are
// "subscription" and "api-key".
func parseClaudeAuthMode(v string) (agents.ClaudeAuthMode, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "subscription", "sub", "s":
		return agents.ClaudeAuthModeSubscription, true
	case "api-key", "apikey", "api_key", "key", "k":
		return agents.ClaudeAuthModeAPIKey, true
	default:
		return 0, false
	}
}

// promptClaudeAuthMode runs the interactive first-run selection, writing the
// choices to stdout and reading one line from stdin per attempt. It re-asks
// on unrecognized input and returns an error only if stdin yields EOF before
// a valid choice (e.g. the input stream closes).
func promptClaudeAuthMode(stdin io.Reader, stdout io.Writer) (agents.ClaudeAuthMode, error) {
	br := bufio.NewReader(stdin)
	fmt.Fprintln(stdout, "How should Claude authenticate?")
	fmt.Fprintln(stdout, "  [1] subscription  Claude Pro/Max via OAuth login (default)")
	fmt.Fprintln(stdout, "  [2] api-key       Anthropic API key (ANTHROPIC_API_KEY)")
	for {
		fmt.Fprint(stdout, "Choose [subscription/api-key] (default subscription): ")
		line, err := br.ReadString('\n')
		choice := strings.TrimSpace(line)
		if choice == "" {
			if err != nil {
				// EOF with no input on this attempt: take the default rather
				// than spinning forever on a closed stream.
				return agents.ClaudeAuthModeSubscription, nil
			}
			// Bare Enter selects the default.
			return agents.ClaudeAuthModeSubscription, nil
		}
		switch strings.ToLower(choice) {
		case "1":
			return agents.ClaudeAuthModeSubscription, nil
		case "2":
			return agents.ClaudeAuthModeAPIKey, nil
		}
		if mode, ok := parseClaudeAuthMode(choice); ok {
			return mode, nil
		}
		fmt.Fprintf(stdout, "  unrecognized choice %q; please answer subscription or api-key\n", choice)
		if err != nil {
			// Stream closed mid-prompt with no valid choice; fall back to the
			// documented default rather than erroring the whole launch.
			return agents.ClaudeAuthModeSubscription, nil
		}
	}
}
