// aileron-sh is a policy-enforced shell shim for AI coding agents.
//
// It is installed as the agent's SHELL, so every command the agent runs passes
// through here. Commands in -c mode are evaluated against aileron.yaml policy.
// Script and interactive modes pass through to the real shell.
//
// Invocation modes:
//   - aileron-sh -c "command"   (policy-evaluated — how agents run commands)
//   - aileron-sh script.sh      (passthrough)
//   - aileron-sh                (passthrough — interactive shell)
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
	realShell := os.Getenv("AILERON_REAL_SHELL")
	if realShell == "" {
		realShell = "/bin/sh"
	}

	binary, err := exec.LookPath(realShell)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aileron-sh: cannot find shell %q: %v\n", realShell, err)
		os.Exit(127)
	}

	args := os.Args[1:]

	// Only evaluate policy for -c mode (how agents invoke commands) and
	// only when an aileron.yaml exists. Without a policy file, the developer
	// hasn't opted into enforcement — passthrough to the real shell.
	cwd, _ := os.Getwd()
	if len(args) >= 2 && args[0] == "-c" && launch.FindPolicyFile(cwd) != "" {
		command := args[1]
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

// promptApproval writes a prompt to /dev/tty and reads the response.
// Returns true if the user approves. Falls back to deny if /dev/tty
// is not available (e.g., in CI).
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
