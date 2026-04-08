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
	"strings"
	"syscall"

	"github.com/ALRubinger/aileron/core/launch"
	"github.com/ALRubinger/aileron/core/model"
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
	command, hasCommand := extractCommand(args)
	if hasCommand && launch.FindPolicyFile(cwd) != "" {
		policyPath := launch.FindPolicyFile(cwd)
		result := launch.EvaluateCommand(policyPath, command, cwd)

		switch result.Disposition {
		case model.DispositionAllow:
			// Fall through to exec.
		case model.DispositionDeny:
			launch.WriteDeny(os.Stderr, command, result.Reason)
			os.Exit(1)
		case model.DispositionRequireApproval:
			if !promptApproval(command, result.Reason) {
				launch.WriteDenyByUser(os.Stderr, command)
				os.Exit(1)
			}
		default:
			if !promptApproval(command, result.Reason) {
				os.Exit(1)
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

// promptApproval writes a prompt to /dev/tty and reads the response.
func promptApproval(command, reason string) bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer tty.Close()

	fmt.Fprintf(tty, "\n\033[33m  ⏸ aileron: agent wants to run\033[0m %s\n", command)
	if reason != "" {
		fmt.Fprintf(tty, "    %s\n", reason)
	}
	fmt.Fprintf(tty, "    \033[1m[y]\033[0m allow  \033[1m[n]\033[0m deny  ")

	reader := bufio.NewReader(tty)
	response, _ := reader.ReadString('\n')
	response = strings.TrimSpace(strings.ToLower(response))

	return response == "y" || response == "yes"
}
