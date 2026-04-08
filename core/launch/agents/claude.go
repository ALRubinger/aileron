package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	// Translate allow rules to --allowedTools (each as a separate arg).
	var allowed []string
	for _, r := range pf.Allow {
		pattern := ruleToClaudePattern(r)
		if pattern != "" {
			allowed = append(allowed, "Bash("+pattern+")")
		}
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, allowed...)
	}

	// Translate deny rules to --disallowedTools (each as a separate arg).
	var denied []string
	for _, r := range pf.Deny {
		pattern := ruleToClaudePattern(r)
		if pattern != "" {
			denied = append(denied, "Bash("+pattern+")")
		}
	}
	if len(denied) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, denied...)
	}

	// Clear existing Bash permissions from settings.local.json so
	// accumulated allow-always rules don't bypass Aileron's policy.
	cleanup := clearBashPermissions(cwd)

	return args, cleanup, nil
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

// clearBashPermissions removes Bash(*) entries from .claude/settings.local.json
// so accumulated allow-always rules don't bypass Aileron's policy. Returns a
// cleanup function that restores the original file.
func clearBashPermissions(cwd string) func() {
	configPath := filepath.Join(cwd, ".claude", "settings.local.json")

	origData, hadOriginal := readFileIfExists(configPath)
	if !hadOriginal {
		return nil
	}

	var settings map[string]any
	if err := json.Unmarshal(origData, &settings); err != nil {
		return nil
	}

	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		return nil
	}

	allowList, ok := perms["allow"].([]any)
	if !ok {
		return nil
	}

	// Filter out Bash(*) entries.
	filtered := make([]any, 0, len(allowList))
	for _, item := range allowList {
		s, ok := item.(string)
		if !ok || !strings.HasPrefix(s, "Bash(") {
			filtered = append(filtered, item)
		}
	}

	perms["allow"] = filtered
	settings["permissions"] = perms

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil
	}

	os.WriteFile(configPath, data, 0o644)

	return func() {
		os.WriteFile(configPath, origData, 0o644)
	}
}

func readFileIfExists(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
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
