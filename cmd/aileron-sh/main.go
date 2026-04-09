// aileron-sh is a policy-enforced shell shim and hook for AI coding agents.
//
// Invocation modes:
//   - aileron-sh --hook           (Claude Code PreToolUse hook — reads JSON from stdin)
//   - aileron-sh -c "command"     (shell shim — policy-evaluated, for agents using $SHELL)
//   - aileron-sh script.sh        (passthrough)
//   - aileron-sh                  (passthrough — interactive shell)
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/core/audit"
	"github.com/ALRubinger/aileron/core/launch"
	"github.com/ALRubinger/aileron/core/model"
	launchpolicy "github.com/ALRubinger/aileron/core/policy/launch"
)

// approvalResponse represents the user's choice at the approval prompt.
type approvalResponse int

const (
	responseDeny approvalResponse = iota
	responseAllowOnce
	responseAllowProject
	responseAllowUser
)

func main() {
	args := os.Args[1:]

	// Hook mode: act as a Claude Code PreToolUse hook.
	// Reads JSON from stdin, evaluates policy, writes decision to stdout.
	if len(args) >= 1 && args[0] == "--hook" {
		os.Exit(launch.RunHook(os.Stdin, os.Stdout))
	}

	// Shell shim mode: intercept -c commands for agents that use $SHELL.
	realShell := os.Getenv("AILERON_REAL_SHELL")
	if realShell == "" {
		realShell = "/bin/sh"
	}

	binary, err := exec.LookPath(realShell)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aileron-sh: cannot find shell %q: %v\n", realShell, err)
		os.Exit(127)
	}

	// Only evaluate policy for -c mode and only when an aileron.yaml exists.
	// Claude Code spawns: shell -c -l "command" (login flag between -c and the
	// command string). We extract the command by finding -c and skipping any
	// intervening shell flags (-l, -i, etc.).
	cwd, _ := os.Getwd()
	rawCommand, hasCommand := extractCommand(args)
	command := normalizeCommand(os.Getenv("AILERON_AGENT"), rawCommand)
	policyPath := launch.FindPolicyFile(cwd)
	if hasCommand && policyPath != "" {
		result := launch.EvaluateCommand(policyPath, command, cwd)

		switch result.Disposition {
		case model.DispositionAllow:
			writeAuditEntry(command, "allow", result.RuleID)
		case model.DispositionDeny:
			writeAuditEntry(command, "deny", result.RuleID)
			launch.WriteDeny(os.Stderr, command, result.Reason)
			os.Exit(1)
		case model.DispositionRequireApproval:
			response := promptApproval(command, result.Reason)
			switch response {
			case responseDeny:
				writeAuditEntry(command, "ask_denied", result.RuleID)
				launch.WriteDenyByUser(os.Stderr, command)
				os.Exit(1)
			case responseAllowOnce:
				writeAuditEntry(command, "ask_approved", result.RuleID)
			case responseAllowProject:
				writeAuditEntry(command, "ask_approved", result.RuleID)
				persistAllowRule(policyPath, command, "project aileron.yaml")
			case responseAllowUser:
				writeAuditEntry(command, "ask_approved", result.RuleID)
				persistAllowRule(userSettingsPath(), command, "user settings")
			}
		}
	}

	// Exec the real shell with the original arguments.
	argv := append([]string{realShell}, args...)
	env := os.Environ()
	if err := syscall.Exec(binary, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "aileron-sh: exec %q failed: %v\n", binary, err)
		os.Exit(126)
	}
}

// writeAuditEntry appends an entry to the audit log if configured.
func writeAuditEntry(command, disposition, ruleID string) {
	path := os.Getenv("AILERON_AUDIT_LOG")
	if path == "" {
		return
	}
	audit.AppendShellEntry(path, audit.ShellEntry{
		Timestamp:   time.Now(),
		SessionID:   os.Getenv("AILERON_SESSION_ID"),
		Command:     command,
		Disposition: disposition,
		RuleID:      ruleID,
		Agent:       os.Getenv("AILERON_AGENT"),
	})
}

// persistAllowRule writes a command pattern to the given policy file
// and notifies the user via /dev/tty.
func persistAllowRule(path, command, label string) {
	if err := launchpolicy.AppendAllowRule(path, command); err != nil {
		fmt.Fprintf(os.Stderr, "aileron-sh: warning: could not save rule: %v\n", err)
		return
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer tty.Close()
	fmt.Fprintf(tty, "    \033[32m✓ saved to %s\033[0m\n", label)
}

// userSettingsPath returns the path to ~/.aileron/settings.yaml.
func userSettingsPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".aileron", "settings.yaml")
}

// extractCommand finds the command string in shell args of the form
// [-l] [-i] -c [-l] [-i] "command" [arg0 ...]. Returns the command
// string and true if -c mode is detected, or ("", false) otherwise.
//
// Shells accept flags in any order relative to -c, and Claude Code
// specifically passes [-c, -l, command]. We scan for -c, then take the
// first non-flag argument after it as the command string.
func extractCommand(args []string) (string, bool) {
	foundC := false
	for _, a := range args {
		if a == "-c" {
			foundC = true
			continue
		}
		if foundC {
			// Skip single-character flags that may appear between -c and the command.
			if len(a) > 0 && a[0] == '-' && len(a) == 2 {
				continue
			}
			return a, true
		}
	}
	return "", false
}

// normalizeCommand applies agent-specific command transformations before
// policy evaluation. Each agent may wrap commands differently; this function
// strips that wrapping so policy rules match the developer's intent.
func normalizeCommand(agent, command string) string {
	switch agent {
	case "claude":
		return unwrapClaudeEval(command)
	default:
		return command
	}
}

// unwrapClaudeEval extracts the inner command from Claude Code's wrapper template.
// Claude Code sends commands in the form:
//
//	shopt -u extglob 2>/dev/null || true && eval 'actual command' < /dev/null && pwd -P >| /tmp/...
//
// This function finds the eval '...' segment and returns the inner command.
// If the input doesn't match the wrapper pattern, it is returned unchanged.
func unwrapClaudeEval(command string) string {
	// Look for: eval '...'
	const evalPrefix = "eval '"
	idx := strings.Index(command, evalPrefix)
	if idx < 0 {
		return command
	}

	inner := command[idx+len(evalPrefix):]

	// Find the closing single quote. The inner command uses '\'' to escape
	// literal single quotes (shell idiom: end quote, escaped quote, start quote).
	var b strings.Builder
	for i := 0; i < len(inner); {
		if inner[i] == '\'' {
			// Check for '\'' escape sequence (end-quote, backslash-quote, start-quote).
			if i+3 < len(inner) && inner[i:i+4] == `'\''` {
				b.WriteByte('\'')
				i += 4
				continue
			}
			// Unescaped closing quote — we're done.
			return b.String()
		}
		b.WriteByte(inner[i])
		i++
	}

	// No closing quote found — return original command unchanged.
	return command
}

// promptApproval writes a prompt to /dev/tty and reads the response.
func promptApproval(command, reason string) approvalResponse {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return responseDeny
	}
	defer tty.Close()

	fmt.Fprintf(tty, "\n\033[33m  ⏸ aileron: agent wants to run\033[0m %s\n", command)
	if reason != "" {
		fmt.Fprintf(tty, "    %s\n", reason)
	}
	fmt.Fprintf(tty, "    \033[1m[y]\033[0m allow once  \033[1m[n]\033[0m deny  \033[1m[p]\033[0m always (project)  \033[1m[u]\033[0m always (user)  ")

	reader := bufio.NewReader(tty)
	response, _ := reader.ReadString('\n')
	response = strings.TrimSpace(strings.ToLower(response))

	switch response {
	case "y", "yes":
		return responseAllowOnce
	case "p":
		return responseAllowProject
	case "u":
		return responseAllowUser
	default:
		return responseDeny
	}
}
