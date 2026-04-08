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
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/policy"
	"github.com/ALRubinger/aileron/core/policy/launch"
	"github.com/ALRubinger/aileron/core/store/mem"
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
	if len(args) >= 2 && args[0] == "-c" && findPolicyFile() != "" {
		command := args[1]
		disposition, reason := evaluatePolicy(command)

		switch disposition {
		case model.DispositionAllow:
			// Fall through to exec.
		case model.DispositionDeny:
			fmt.Fprintf(os.Stderr, "\033[31m  ✗ aileron: denied\033[0m %s\n", command)
			if reason != "" {
				fmt.Fprintf(os.Stderr, "    %s\n", reason)
			}
			os.Exit(1)
		case model.DispositionRequireApproval:
			if !promptApproval(command, reason) {
				fmt.Fprintf(os.Stderr, "\033[33m  ✗ aileron: denied by user\033[0m %s\n", command)
				os.Exit(1)
			}
			// Approved — fall through to exec.
		default:
			// Unknown disposition — ask to be safe.
			if !promptApproval(command, reason) {
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

// evaluatePolicy loads aileron.yaml and evaluates the command against it.
// Returns the disposition and an optional reason string.
func evaluatePolicy(command string) (model.Disposition, string) {
	pf := loadPolicyFile()
	rules := pf.ToEngineRules()

	store := mem.NewPolicyStore()
	ctx := context.Background()

	active := policy.ActiveStatus()
	if err := store.Create(ctx, policy.MakePolicy("launch", "default", rules, active)); err != nil {
		// If policy setup fails, default to ask.
		return model.DispositionRequireApproval, "policy load error"
	}

	engine := policy.NewRuleEngine(store)

	// Extract binary name (argv[0]) from the command.
	parts := strings.Fields(command)
	binaryName := ""
	argsStr := ""
	if len(parts) > 0 {
		binaryName = parts[0]
	}
	if len(parts) > 1 {
		argsStr = strings.Join(parts[1:], " ")
	}

	decision, err := engine.Evaluate(ctx, policy.EvaluationRequest{
		WorkspaceID: "default",
		Action: model.ActionIntent{
			Type:    "shell.exec",
			Summary: command,
			Metadata: map[string]any{
				"shell.command":     command,
				"shell.binary":      binaryName,
				"shell.args":        argsStr,
				"shell.working_dir": mustGetwd(),
			},
		},
	})
	if err != nil {
		return model.DispositionRequireApproval, "policy evaluation error"
	}

	return decision.Disposition, decision.DenialReason
}

// loadPolicyFile looks for aileron.yaml in the current directory, then
// walks up to the repo root. Returns an empty policy if not found.
func loadPolicyFile() *launch.PolicyFile {
	path := findPolicyFile()
	if path == "" {
		return &launch.PolicyFile{Version: 1}
	}
	pf, err := launch.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aileron-sh: warning: failed to load %s: %v\n", path, err)
		return &launch.PolicyFile{Version: 1}
	}
	return pf
}

// findPolicyFile searches for aileron.yaml in the current directory and
// parent directories (up to filesystem root).
func findPolicyFile() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
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

// promptApproval writes a prompt to /dev/tty and reads the response.
// Returns true if the user approves. Falls back to deny if /dev/tty
// is not available (e.g., in CI).
func promptApproval(command, reason string) bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// No terminal — default to deny for safety.
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

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}
