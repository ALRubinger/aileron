// aileron-sh is a policy-enforced shell shim and hook for AI coding agents.
//
// Invocation modes:
//   - aileron-sh --hook           (Claude Code PreToolUse hook — reads JSON from stdin)
//   - aileron-sh -c "command"     (shell shim — policy-evaluated, for agents using $SHELL)
//   - aileron-sh script.sh        (passthrough)
//   - aileron-sh                  (passthrough — interactive shell)
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/core/audit"
	"github.com/ALRubinger/aileron/core/launch"
	"github.com/ALRubinger/aileron/core/launch/agents"
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
	command, shouldEval := agents.NormalizeCommand(os.Getenv("AILERON_AGENT"), rawCommand)
	policyPath := launch.FindPolicyFile(cwd)
	if hasCommand && shouldEval && policyPath != "" {
		result := launch.EvaluateCommand(policyPath, command, cwd)

		switch result.Disposition {
		case model.DispositionAllow:
			writeAuditEntry(command, "allow", result.RuleID)
		case model.DispositionDeny:
			writeAuditEntry(command, "deny", result.RuleID)
			writeDenyMessage(command, result.Reason, result.RuleID, policyPath)
			os.Exit(1)
		case model.DispositionRequireApproval:
			response := promptApproval(command, result.Reason)
			switch response {
			case responseDeny:
				writeAuditEntry(command, "ask_denied", result.RuleID)
				writeDenyByUserMessage(command)
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

// writeDenyMessage writes the deny message to stderr. The output
// copier's idle re-render will restore the status bar after.
func writeDenyMessage(command, reason, ruleID, policyPath string) {
	launch.WriteDeny(os.Stderr, command, reason, ruleID, policyPath)
}

// writeDenyByUserMessage writes the user-denied message to stderr.
func writeDenyByUserMessage(command string) {
	launch.WriteDenyByUser(os.Stderr, command)
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


// promptApproval requests user approval via the launcher's approval
// server. The launcher owns the real terminal and handles the prompt.
func promptApproval(command, reason string) approvalResponse {
	socketPath := os.Getenv("AILERON_APPROVAL_SOCKET")
	if socketPath == "" {
		return responseDeny
	}

	decision := launch.RequestApproval(socketPath, command, reason)

	switch decision {
	case "allow_once":
		return responseAllowOnce
	case "allow_project":
		return responseAllowProject
	case "allow_user":
		return responseAllowUser
	default:
		return responseDeny
	}
}
