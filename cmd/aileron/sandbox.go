package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"
)

func runSandbox(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox <init|plan|build|check>")
		return 1
	}
	switch args[0] {
	case "init":
		return runSandboxInit(args[1:], stdout, stderr)
	case "plan":
		return runSandboxPlan(args[1:], stdout, stderr)
	case "build":
		return runSandboxBuild(args[1:], stdout, stderr)
	case "check":
		return runSandboxCheck(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown sandbox command: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron sandbox <init|plan|build|check>")
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

func runSandboxBuild(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeName := flags.String("runtime", sandboxcontainer.DefaultRuntime, "Container runtime: auto, docker, or podman")
	tag := flags.String("tag", "", "Override the image tag to build")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox build [--runtime=auto|docker|podman] [--tag=<image>]")
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
	result, err := sandboxBuildFn(context.Background(), *runtimeName, stdout, stderr, sandboxcontainer.BuildOptions{
		WorkDir: cwd,
		Plan:    plan,
		Tag:     *tag,
	})
	if errors.Is(err, sandboxcontainer.ErrNoBuildRequired) {
		fmt.Fprintf(stdout, "tier: %s\n", result.Tier)
		fmt.Fprintf(stdout, "image: %s\n", result.Image)
		fmt.Fprintln(stdout, "build: not required")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tier: %s\n", result.Tier)
	fmt.Fprintf(stdout, "runtime: %s\n", result.Runtime)
	fmt.Fprintf(stdout, "image: %s\n", result.Image)
	fmt.Fprintln(stdout, "build: complete")
	return 0
}

func runSandboxCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeName := flags.String("runtime", sandboxcontainer.DefaultRuntime, "Container runtime: auto, docker, or podman")
	buildPolicy := flags.String("build", sandboxcontainer.BuildPolicyAuto, "Build policy: auto, always, or never")
	agent := flags.String("agent", "", "Agent command to validate in the sandbox image")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() > 1 || (*agent != "" && flags.NArg() != 0) {
		fmt.Fprintln(stderr, "usage: aileron sandbox check [--runtime=auto|docker|podman] [--build=auto|always|never] [--agent=<command>] [command]")
		return 1
	}
	command := *agent
	if command == "" && flags.NArg() == 1 {
		command = flags.Arg(0)
	}
	if command == "" {
		fmt.Fprintln(stderr, "usage: aileron sandbox check [--runtime=auto|docker|podman] [--build=auto|always|never] [--agent=<command>] [command]")
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
	result, err := sandboxCheckBuildFn(context.Background(), *runtimeName, stdout, stderr, sandboxcontainer.BuildOptions{
		WorkDir: cwd,
		Plan:    plan,
		Policy:  *buildPolicy,
	})
	if err != nil && !errors.Is(err, sandboxcontainer.ErrNoBuildRequired) {
		fmt.Fprintf(stderr, "error: %s\n", sandboxCheckError(err))
		return 1
	}
	resolvedRuntime := result.Runtime
	if resolvedRuntime == "" {
		resolvedRuntime, err = sandboxcontainer.ResolveRuntime(*runtimeName)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}
	if err := sandboxCheckValidateFn(context.Background(), resolvedRuntime, cwd, result.Image, command); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tier: %s\n", result.Tier)
	fmt.Fprintf(stdout, "runtime: %s\n", resolvedRuntime)
	fmt.Fprintf(stdout, "image: %s\n", result.Image)
	fmt.Fprintf(stdout, "agent: %s\n", command)
	fmt.Fprintln(stdout, "support: ok")
	return 0
}

func sandboxCheckError(err error) string {
	return strings.ReplaceAll(err.Error(), "--sandbox-build", "--build")
}

var sandboxBuildFn = func(ctx context.Context, runtimeName string, stdout, stderr io.Writer, opts sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
	return sandboxcontainer.Builder{
		Runtime: runtimeName,
		Stdout:  stdout,
		Stderr:  stderr,
	}.Build(ctx, opts)
}

var sandboxCheckBuildFn = sandboxBuildFn

var sandboxCheckValidateFn = func(ctx context.Context, runtimeName, workDir, image, command string) error {
	return sandboxcontainer.Builder{
		Runtime: runtimeName,
		Stdout:  io.Discard,
	}.Validate(ctx, sandboxcontainer.ValidateOptions{
		Runtime: runtimeName,
		Image:   image,
		WorkDir: workDir,
		Command: []string{command},
	})
}
