package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"
)

func runSandbox(args []string, registry *launch.Registry, stdout, stderr io.Writer) int {
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
	force := flags.Bool("force", false, "Overwrite an existing .devcontainer/devcontainer.json")
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
	plan, err := sandboxcomposition.Discover(cwd, version.Version, "")
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
	if len(plan.Features) > 0 {
		refs := make([]string, 0, len(plan.Features))
		for ref := range plan.Features {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		// On a Tier 2 BYO image the features are parsed for inspection but never
		// applied (the image is used as-is), so flag them as inert rather than
		// implying they compose into the sandbox.
		if plan.Tier == sandboxcomposition.TierBYOImage {
			fmt.Fprintf(stdout, "features (ignored — BYO image): %s\n", strings.Join(refs, ", "))
		} else {
			fmt.Fprintf(stdout, "features: %s\n", strings.Join(refs, ", "))
		}
	}
	return 0
}

func runSandboxBuild(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeName := flags.String("runtime", sandboxcontainer.DefaultRuntime, "Container runtime: auto or docker")
	tag := flags.String("tag", "", "Override the image tag to build")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox build [--runtime=auto|docker] [--tag=<image>]")
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	plan, err := sandboxcomposition.Discover(cwd, version.Version, "")
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
	runtimeName := flags.String("runtime", sandboxcontainer.DefaultRuntime, "Container runtime: auto or docker")
	buildPolicy := flags.String("build", sandboxcontainer.BuildPolicyAuto, "Build policy: auto, always, or never")
	agent := flags.String("agent", "", "Agent command to validate in the sandbox image")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() > 1 || (*agent != "" && flags.NArg() != 0) {
		fmt.Fprintln(stderr, "usage: aileron sandbox check [--runtime=auto|docker] [--build=auto|always|never] [--agent=<command>] [command]")
		return 1
	}
	command := *agent
	if command == "" && flags.NArg() == 1 {
		command = flags.Arg(0)
	}
	if command == "" {
		fmt.Fprintln(stderr, "usage: aileron sandbox check [--runtime=auto|docker] [--build=auto|always|never] [--agent=<command>] [command]")
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	// Pass the resolved agent command so `sandbox check` resolves the same
	// published per-agent image the launcher would (launch/check parity). With
	// no .devcontainer and a published agent, this plans TierPublished and the
	// build step pulls (or inspects) the per-agent image.
	plan, err := sandboxcomposition.Discover(cwd, version.Version, command)
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
	// A baked image (issue #957) carries aileron-mcp at the well-known path,
	// so require the in-image binary smoke check; an unbaked image has no
	// aileron-mcp mounted under sandbox check, so requiring it would fail.
	bakedVersion := sandboxCheckBakedVersionFn(context.Background(), resolvedRuntime, result.Image)
	if err := sandboxCheckValidateFn(context.Background(), resolvedRuntime, cwd, result.Image, command, bakedVersion != ""); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tier: %s\n", result.Tier)
	fmt.Fprintf(stdout, "runtime: %s\n", resolvedRuntime)
	fmt.Fprintf(stdout, "image: %s\n", result.Image)
	fmt.Fprintf(stdout, "agent: %s\n", command)
	if bakedVersion != "" {
		reportBakedMCPVersion(stdout, stderr, bakedVersion, version.Version)
	}
	fmt.Fprintln(stdout, "support: ok")
	return 0
}

// reportBakedMCPVersion surfaces version skew between a baked sandbox image's
// aileron-mcp and the host CLI. ADR-0024's host-mount lockstep does not hold
// for baked images (issue #957), so skew is an expected operational state for
// sealed runtimes rather than an error. A match prints an OK line; a mismatch
// prints a warning and never changes the exit code.
func reportBakedMCPVersion(stdout, stderr io.Writer, baked, host string) {
	if baked == host {
		fmt.Fprintf(stdout, "mcp: baked aileron-mcp %s (matches host CLI)\n", baked)
		return
	}
	fmt.Fprintf(stderr, "warning: baked aileron-mcp version %s differs from host CLI version %s. Baked images ship their own aileron-mcp under the managed-release model, so this skew is expected for sealed runtimes but worth investigating in the v4 default topology.\n", baked, host)
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

var sandboxCheckValidateFn = func(ctx context.Context, runtimeName, workDir, image, command string, requireMCPBinary bool) error {
	return sandboxcontainer.Builder{
		Runtime: runtimeName,
		Stdout:  io.Discard,
	}.Validate(ctx, sandboxcontainer.ValidateOptions{
		Runtime:           runtimeName,
		Image:             image,
		WorkDir:           workDir,
		Command:           []string{command},
		RequireProxyTrust: sandboxCheckRequiresProxyTrust(runtimeName),
		RequireMCPBinary:  requireMCPBinary,
	})
}

// sandboxCheckBakedVersionFn reports the aileron-mcp version baked into the
// resolved image (empty when unbaked or uninspectable). Swappable in tests.
var sandboxCheckBakedVersionFn = func(ctx context.Context, runtimeName, image string) string {
	return sandboxcontainer.BakedMCPVersion(ctx, sandboxcontainer.DefaultRunner(), runtimeName, image)
}

// sandboxCheckRequiresProxyTrust reports whether `sandbox check --agent` must
// validate the v4 HTTPS proxy contract for the given runtime. Docker runs
// the default-on proxy under aileron launch, so sandbox check exercises the
// same contract to surface BYO image gaps before launch would fail. The
// --sandbox-proxy=off opt-out applies to aileron launch only, not to sandbox
// check.
func sandboxCheckRequiresProxyTrust(runtimeName string) bool {
	switch strings.TrimSpace(runtimeName) {
	case "docker":
		return true
	default:
		return false
	}
}
