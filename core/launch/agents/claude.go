package agents

import (
	"fmt"
	"os"
	"strings"

	launchpolicy "github.com/ALRubinger/aileron/core/policy/launch"
)

// Claude is the agent definition for Claude Code.
type Claude struct{}

func (c Claude) Name() string           { return "claude" }
func (c Claude) BinaryNames() []string  { return []string{"claude"} }
func (c Claude) Env() map[string]string { return nil }

// SetupHooks translates aileron.yaml rules into Claude Code's native
// --allowedTools and --disallowedTools CLI flags. No hooks or settings
// file modifications needed — Claude Code's permission system handles
// enforcement directly.
//
// Mapping:
//   - aileron.yaml allow rules → --allowedTools "Bash(pattern)"
//   - aileron.yaml deny rules  → --disallowedTools "Bash(pattern)"
//   - aileron.yaml ask rules   → default behavior (Claude Code prompts)
//   - default: ask             → unmatched commands prompt
func (c Claude) SetupHooks(shimPath string) ([]string, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, nil
	}

	policyPath := findPolicyFile(cwd)
	if policyPath == "" {
		return nil, nil, nil
	}

	pf, err := launchpolicy.Load(policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aileron: warning: failed to load %s: %v\n", policyPath, err)
		return nil, nil, nil
	}

	var args []string

	// Translate allow rules to --allowedTools.
	var allowed []string
	for _, r := range pf.Allow {
		pattern := ruleToClaudePattern(r)
		if pattern != "" {
			allowed = append(allowed, "Bash("+pattern+")")
		}
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, strings.Join(allowed, " "))
	}

	// Translate deny rules to --disallowedTools.
	var denied []string
	for _, r := range pf.Deny {
		pattern := ruleToClaudePattern(r)
		if pattern != "" {
			denied = append(denied, "Bash("+pattern+")")
		}
	}
	if len(denied) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, strings.Join(denied, " "))
	}

	return args, nil, nil
}

// ruleToClaudePattern converts an aileron.yaml rule to a Claude Code
// permission pattern. Claude Code uses "command:*" syntax where * is
// a glob. Aileron uses full command globs like "git push *".
func ruleToClaudePattern(r launchpolicy.Rule) string {
	cmd := r.Command
	if cmd == "" {
		return ""
	}

	// Claude Code patterns use ":" to separate command from args.
	// "git push *" → "git push:*"
	// "go test ./..." → "go test:*"
	// Simple commands without wildcards pass through as-is.
	//
	// For commands ending in " *", convert to "prefix:*"
	if strings.HasSuffix(cmd, " *") {
		prefix := strings.TrimSuffix(cmd, " *")
		return prefix + ":*"
	}

	// For exact commands, use as-is.
	return cmd
}

// findPolicyFile searches for aileron.yaml walking up from startDir.
func findPolicyFile(startDir string) string {
	dir := startDir
	for {
		candidate := dir + "/aileron.yaml"
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir || parent == "" {
			return ""
		}
		dir = parent
	}
}
