package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALRubinger/aileron/core/audit"
	"github.com/ALRubinger/aileron/core/launch"
	"github.com/ALRubinger/aileron/core/launch/agents"
	"github.com/ALRubinger/aileron/core/version"
)

func main() {
	registry := launch.NewRegistry()
	registry.Register(agents.Claude{})
	os.Exit(run(os.Args[1:], registry, os.Stdout, os.Stderr))
}

// run executes the CLI and returns an exit code.
func run(args []string, registry *launch.Registry, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout, registry)
		return 1
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "aileron %s (%s)\n", version.Version, version.Commit)
		return 0
	case "launch":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: aileron launch <agent> [args...]")
			fmt.Fprintf(stderr, "agents: %s\n", strings.Join(registry.Names(), ", "))
			return 1
		}
		agentName := args[1]
		agent, ok := registry.Get(agentName)
		if !ok {
			fmt.Fprintf(stderr, "unknown agent: %q\n", agentName)
			fmt.Fprintf(stderr, "available agents: %s\n", strings.Join(registry.Names(), ", "))
			return 1
		}

		shimPath, err := resolveShim()
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}

		result, err := launch.Launch(context.Background(), launch.LaunchConfig{
			Agent:     agent,
			ShellShim: shimPath,
			Args:      args[2:],
		})
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return result.ExitCode
	case "log":
		return runLog(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usage(stdout, registry)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %q\n", args[0])
		usage(stderr, registry)
		return 1
	}
}

func usage(w io.Writer, registry *launch.Registry) {
	fmt.Fprintln(w, "aileron — the execution layer for AI coding agents")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  aileron launch <agent> [args...]   Launch an agent with policy-enforced shell")
	fmt.Fprintln(w, "  aileron log [flags]                View the audit trail")
	fmt.Fprintln(w, "  aileron version                    Print version information")
	fmt.Fprintln(w, "  aileron help                       Show this help")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "agents: %s\n", strings.Join(registry.Names(), ", "))
}

// runLog reads and displays the audit trail.
func runLog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "Filter by session ID")
	disposition := fs.String("disposition", "", "Filter by disposition (allow/deny/ask_approved/ask_denied)")
	command := fs.String("command", "", "Filter by command substring")
	path := fs.String("path", "", "Audit log file path (default: auto-detect)")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	logPath := *path
	if logPath == "" {
		logPath = launch.ResolveAuditLogFromCwd()
	}

	entries, err := audit.ReadShellEntriesFiltered(logPath, audit.ShellFilter{
		SessionID:      *session,
		Disposition:    *disposition,
		CommandPattern: *command,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error reading audit log %s: %v\n", logPath, err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Fprintln(stdout, "No audit entries found.")
		return 0
	}

	for _, e := range entries {
		ts := e.Timestamp.Format("15:04:05")
		fmt.Fprintf(stdout, "%s  %-13s  %-12s  %s\n", ts, e.Disposition, e.RuleID, e.Command)
	}
	return 0
}

// resolveShim finds the aileron-sh binary next to this executable, or on PATH.
func resolveShim() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving self path: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("resolving self symlinks: %w", err)
	}
	return launch.ResolveShim(self)
}
