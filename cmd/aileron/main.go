package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALRubinger/aileron/core/launch"
	"github.com/ALRubinger/aileron/core/launch/agents"
	"github.com/ALRubinger/aileron/core/version"
)

func main() {
	registry := launch.NewRegistry()
	registry.Register(agents.Claude{})

	args := os.Args[1:]
	if len(args) == 0 {
		usage(registry)
		os.Exit(1)
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Printf("aileron %s (%s)\n", version.Version, version.Commit)
	case "launch":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: aileron launch <agent> [args...]")
			fmt.Fprintf(os.Stderr, "agents: %s\n", strings.Join(registry.Names(), ", "))
			os.Exit(1)
		}
		agentName := args[1]
		agent, ok := registry.Get(agentName)
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown agent: %q\n", agentName)
			fmt.Fprintf(os.Stderr, "available agents: %s\n", strings.Join(registry.Names(), ", "))
			os.Exit(1)
		}

		shimPath, err := resolveShim()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		result, err := launch.Launch(context.Background(), launch.LaunchConfig{
			Agent:     agent,
			ShellShim: shimPath,
			Args:      args[2:],
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(result.ExitCode)
	case "help", "--help", "-h":
		usage(registry)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n", args[0])
		usage(registry)
		os.Exit(1)
	}
}

func usage(registry *launch.Registry) {
	fmt.Println("aileron — the execution layer for AI coding agents")
	fmt.Println()
	fmt.Println("usage:")
	fmt.Println("  aileron launch <agent> [args...]   Launch an agent with policy-enforced shell")
	fmt.Println("  aileron version                    Print version information")
	fmt.Println("  aileron help                       Show this help")
	fmt.Println()
	fmt.Printf("agents: %s\n", strings.Join(registry.Names(), ", "))
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
