package agents

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch"
)

// Claude is the agent definition for Claude Code.
// Claude Code sends bash-specific commands (shopt, etc.), so the real
// shell must be bash regardless of the user's login shell.
type Claude struct{}

func (c Claude) Name() string          { return "claude" }
func (c Claude) BinaryNames() []string { return []string{"claude"} }

// Args tells Claude Code to auto-approve:
//   - Bash, so Aileron's shell-policy + approval layer is the single
//     arbiter of shell execution rather than Claude Code's per-command
//     prompt.
//   - The Aileron MCP server's tools (`mcp__<launch.MCPServerName>`),
//     so Claude Code does not double-prompt for tools whose execution
//     the daemon already mediates per ADR-0009/0010. Without this,
//     each unique mcp__aileron__* tool fires a "Yes / Yes don't ask
//     again / No" prompt the first time the agent calls it, doubling
//     the trust surface and leading the user to vest trust decisions
//     in the agent CLI rather than Aileron.
//
// `--allowedTools` accepts a single value with space-separated
// patterns; the bare `mcp__<server>` form whitelists every tool from
// that server (including ones registered later in the session).
func (c Claude) Args() []string {
	return []string{"--allowedTools", "Bash(*) mcp__" + launch.MCPServerName}
}

// Env returns Claude-specific environment variables. CLAUDE_CODE_SHELL
// is set to ~/.aileron/bash (installed by ConfigureShell) because Claude
// Code validates that the shell path contains "bash".
func (c Claude) Env() map[string]string {
	env := map[string]string{
		"AILERON_REAL_SHELL": "/bin/bash",
	}
	if home, err := os.UserHomeDir(); err == nil {
		env["CLAUDE_CODE_SHELL"] = filepath.Join(home, ".aileron", "bash")
	}
	return env
}

// LLMEndpointEnv returns the env var Claude Code reads to override the
// Anthropic API base URL. Setting this routes Claude's LLM calls through
// Aileron's embedded gateway when launch starts one.
func (c Claude) LLMEndpointEnv() string { return "ANTHROPIC_BASE_URL" }

// ConfigureShell installs a wrapper script at ~/.aileron/bash whose path
// contains "bash" so Claude Code accepts it as a valid shell. The wrapper
// delegates to aileron-sh for policy enforcement.
func (c Claude) ConfigureShell(shimPath, _ string) error {
	_, err := launch.InstallWrapper(shimPath)
	return err
}

// NormalizeCommand extracts the user command from Claude Code's eval
// wrapper. Claude Code sends commands as:
//
//	shopt -u extglob 2>/dev/null || true && eval 'actual command' < /dev/null && pwd -P >| /tmp/...
//
// Commands without the eval wrapper are Claude Code infrastructure
// (snapshot scripts, etc.) and should pass through without policy.
func (c Claude) NormalizeCommand(raw string) (string, bool) {
	inner := unwrapClaudeEval(raw)
	if inner == raw {
		return raw, false
	}
	return inner, true
}

// unwrapClaudeEval extracts the inner command from Claude Code's wrapper template.
func unwrapClaudeEval(command string) string {
	// Try quoted form first: eval '...'
	const quotedPrefix = "eval '"
	if idx := strings.Index(command, quotedPrefix); idx >= 0 {
		inner := command[idx+len(quotedPrefix):]

		var b strings.Builder
		for i := 0; i < len(inner); {
			if inner[i] == '\'' {
				if i+3 < len(inner) && inner[i:i+4] == `'\''` {
					b.WriteByte('\'')
					i += 4
					continue
				}
				return b.String()
			}
			b.WriteByte(inner[i])
			i++
		}
	}

	// Try unquoted form: eval <command> \< /dev/null
	const unquotedPrefix = "eval "
	if idx := strings.Index(command, unquotedPrefix); idx >= 0 {
		rest := command[idx+len(unquotedPrefix):]

		for _, sep := range []string{` \<`, ` \>`, " &&", " ||"} {
			if end := strings.Index(rest, sep); end >= 0 {
				return strings.TrimSpace(rest[:end])
			}
		}
		return strings.TrimSpace(rest)
	}

	return command
}
