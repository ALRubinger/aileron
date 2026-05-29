package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	"github.com/ALRubinger/aileron/internal/version"
)

func runSandbox(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox <init|plan>")
		return 1
	}
	switch args[0] {
	case "init":
		return runSandboxInit(args[1:], stdout, stderr)
	case "plan":
		return runSandboxPlan(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown sandbox command: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron sandbox <init|plan>")
		return 1
	}
}

func runSandboxInit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	force := flags.Bool("force", false, "Overwrite existing .devcontainer files")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox init [--force]")
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	result, err := sandboxcomposition.Init(sandboxcomposition.InitOptions{
		WorkDir: cwd,
		Version: version.Version,
		Force:   *force,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "created %s\n", result.DevcontainerPath)
	fmt.Fprintf(stdout, "created %s\n", result.DockerfilePath)
	return 0
}

func runSandboxPlan(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox plan")
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	plan, err := sandboxcomposition.Discover(cwd, version.Version)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tier: %s\n", plan.Tier)
	fmt.Fprintf(stdout, "image: %s\n", plan.Image)
	if plan.DevcontainerPath != "" {
		fmt.Fprintf(stdout, "devcontainer: %s\n", plan.DevcontainerPath)
	}
	if plan.DockerfilePath != "" {
		fmt.Fprintf(stdout, "dockerfile: %s\n", plan.DockerfilePath)
	}
	return 0
}
