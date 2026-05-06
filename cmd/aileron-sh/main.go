// aileron-sh is a policy-enforced shell shim and hook for AI coding agents.
//
// Invocation modes:
//   - aileron-sh --hook           (Claude Code PreToolUse hook — reads JSON from stdin)
//   - aileron-sh -c "command"     (shell shim — policy-evaluated, for agents using $SHELL)
//   - aileron-sh script.sh        (passthrough)
//   - aileron-sh                  (passthrough — interactive shell)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/model"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
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

// writeAuditEntry appends an entry to today's daily-rotated audit log
// under the state dir advertised via AILERON_AUDIT_DIR (set by
// `aileron launch`). The file path is recomputed on every call so a
// shell invocation that happens after midnight lands in the new day's
// file. ADR-0012 specifies the user-scope, daily-rotated layout.
func writeAuditEntry(command, disposition, ruleID string) {
	stateDir := os.Getenv("AILERON_AUDIT_DIR")
	if stateDir == "" {
		return
	}
	audit.AppendShellEntry(audit.DailyPath(stateDir), audit.ShellEntry{
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


// promptApproval requests user approval via the daemon's
// /v1/sessions/{id}/approvals/shell endpoint, which long-polls until
// the user clicks Approve/Deny on the webapp /approvals page (or the
// 5-minute server-side timeout fires).
//
// AILERON_APPROVAL_URL is the daemon's base URL, set by `aileron
// launch` for the agent's child processes. AILERON_SESSION_ID
// identifies which launch session this approval belongs to so the
// daemon can attribute it on the webapp + audit log.
//
// Any failure path — env unset, daemon unreachable, malformed
// response, timeout — collapses to deny so the policy-enforced shell
// fails closed.
func promptApproval(command, reason string) approvalResponse {
	baseURL := os.Getenv("AILERON_APPROVAL_URL")
	sessionID := os.Getenv("AILERON_SESSION_ID")
	if baseURL == "" || sessionID == "" {
		return responseDeny
	}

	cwd, _ := os.Getwd()
	body, err := json.Marshal(map[string]string{
		"command": command,
		"reason":  reason,
		"cwd":     cwd,
	})
	if err != nil {
		return responseDeny
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/v1/sessions/" + sessionID + "/approvals/shell"
	// The daemon caps the wait at its action-approval TTL (5min by
	// default); cap our client-side dial+read at slightly longer so
	// the daemon's bounded response always wins over a transport
	// timeout. We can't tell the difference from here either way —
	// both surface as "deny" by design.
	client := &http.Client{Timeout: 6 * time.Minute}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return responseDeny
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseDeny
	}
	var decoded struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return responseDeny
	}

	switch decoded.Decision {
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
