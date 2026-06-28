package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	sandboxtoolchain "github.com/ALRubinger/aileron/internal/sandbox/toolchain"
	"github.com/ALRubinger/aileron/internal/version"
)

func runSandbox(args []string, registry *launch.Registry, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox <init|plan|build|check|warm|cache>")
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
	case "warm":
		return runSandboxWarm(args[1:], stdout, stderr)
	case "cache":
		return runSandboxCache(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown sandbox command: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron sandbox <init|plan|build|check|warm|cache>")
		return 1
	}
}

// runSandboxCache dispatches the manual cache-volume management subcommands.
// Operators evict Aileron-managed sandbox caches by hand (no max-age policy),
// so the only verb today is `clear`.
func runSandboxCache(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox cache <clear>")
		return 1
	}
	switch args[0] {
	case "clear":
		return runSandboxCacheClear(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown sandbox cache command: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron sandbox cache <clear>")
		return 1
	}
}

// runSandboxCacheClear removes every Aileron-managed sandbox cache volume
// (those whose name carries the CacheVolumePrefix). This is the manual
// garbage-collection path the operator chose over an automatic max-age policy.
// It is safe to run between launches; Docker recreates a volume on the next
// mount that needs it.
func runSandboxCacheClear(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox cache clear", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeName := flags.String("runtime", sandboxcontainer.DefaultRuntime, "Container runtime: auto or docker")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox cache clear [--runtime=auto|docker]")
		return 1
	}
	runtime, err := sandboxcontainer.ResolveRuntime(*runtimeName)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	names, err := sandboxCacheListFn(context.Background(), runtime)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(names) == 0 {
		fmt.Fprintln(stdout, "no Aileron-managed sandbox cache volumes to remove")
		return 0
	}
	if err := sandboxCacheRemoveFn(context.Background(), runtime, names); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	for _, name := range names {
		fmt.Fprintf(stdout, "removed %s\n", name)
	}
	fmt.Fprintf(stdout, "removed %d cache volume(s)\n", len(names))
	return 0
}

// sandboxCacheListFn lists the Aileron-managed sandbox cache volumes for the
// given runtime, filtered by CacheVolumePrefix. Swappable in tests.
var sandboxCacheListFn = func(ctx context.Context, runtime string) ([]string, error) {
	var out bytes.Buffer
	err := sandboxcontainer.DefaultRunner().Run(ctx, runtime,
		[]string{"volume", "ls", "--quiet", "--filter", "name=" + sandboxcomposition.CacheVolumePrefix},
		&out, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("list sandbox cache volumes: %w", err)
	}
	var names []string
	for _, line := range strings.Split(out.String(), "\n") {
		name := strings.TrimSpace(line)
		// Docker's name filter is a substring match; require the prefix so an
		// unrelated volume that merely contains the prefix mid-name is not
		// swept. Aileron names always start with the prefix.
		if strings.HasPrefix(name, sandboxcomposition.CacheVolumePrefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// sandboxCacheRemoveFn removes the named sandbox cache volumes. Swappable in
// tests.
var sandboxCacheRemoveFn = func(ctx context.Context, runtime string, names []string) error {
	args := append([]string{"volume", "rm"}, names...)
	if err := sandboxcontainer.DefaultRunner().Run(ctx, runtime, args, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("remove sandbox cache volumes: %w", err)
	}
	return nil
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

// rejectOfflineWithEscapeHatch fails fast when --offline is combined with the
// managed-toolchain escape hatch (--node/--devcontainer-cli). The two express
// conflicting intents: --offline asks to resolve the managed toolchain from the
// warm cache, while the escape hatch points the build at a pre-staged
// Node + CLI and skips the provisioner entirely (so Offline would be a silent
// no-op). Rather than dropping the offline intent, name both flags so the
// operator picks one. It returns a non-nil error when the combination is
// present (the message is already written to stderr).
func rejectOfflineWithEscapeHatch(offline bool, node, devcontainerCLI string, stderr io.Writer) error {
	if !offline || (node == "" && devcontainerCLI == "") {
		return nil
	}
	fmt.Fprintln(stderr, "error: --offline cannot be combined with the --node/--devcontainer-cli escape hatch; --offline resolves the managed toolchain from the warm cache, while --node/--devcontainer-cli skip the provisioner entirely. Pick one.")
	return errors.New("offline conflicts with escape hatch")
}

func runSandboxBuild(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeName := flags.String("runtime", sandboxcontainer.DefaultRuntime, "Container runtime: auto or docker")
	tag := flags.String("tag", "", "Override the image tag to build")
	toolchain := flags.String("toolchain", sandboxcontainer.ToolchainModeAuto, "Features-build toolchain: managed (default) or host-npx")
	node := flags.String("node", "", "Managed-toolchain escape hatch: path to a Node binary (with --devcontainer-cli)")
	devcontainerCLI := flags.String("devcontainer-cli", "", "Managed-toolchain escape hatch: path to the @devcontainers/cli entrypoint (with --node)")
	offline := flags.Bool("offline", false, "Resolve the managed toolchain from the warm cache with no network (run `aileron sandbox warm` first)")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox build [--runtime=auto|docker] [--tag=<image>] [--toolchain=managed|host-npx] [--offline] [--node=<path> --devcontainer-cli=<path>]")
		return 1
	}
	if err := rejectOfflineWithEscapeHatch(*offline, *node, *devcontainerCLI, stderr); err != nil {
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
	toolchainMode, nodeBinary, cliEntrypoint := sandboxcontainer.ResolveToolchainSelection(*toolchain, *node, *devcontainerCLI, os.Getenv)
	result, err := sandboxBuildFn(context.Background(), *runtimeName, stdout, stderr, sandboxcontainer.BuildOptions{
		WorkDir:                   cwd,
		Plan:                      plan,
		Tag:                       *tag,
		ToolchainMode:             toolchainMode,
		NodeBinary:                nodeBinary,
		DevcontainerCLIEntrypoint: cliEntrypoint,
		Offline:                   *offline,
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
	toolchain := flags.String("toolchain", sandboxcontainer.ToolchainModeAuto, "Features-build toolchain: managed (default) or host-npx")
	node := flags.String("node", "", "Managed-toolchain escape hatch: path to a Node binary (with --devcontainer-cli)")
	devcontainerCLI := flags.String("devcontainer-cli", "", "Managed-toolchain escape hatch: path to the @devcontainers/cli entrypoint (with --node)")
	offline := flags.Bool("offline", false, "Resolve the managed toolchain from the warm cache with no network (run `aileron sandbox warm` first)")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) > 1 || (*agent != "" && len(positionals) != 0) {
		fmt.Fprintln(stderr, "usage: aileron sandbox check [--runtime=auto|docker] [--build=auto|always|never] [--agent=<command>] [--toolchain=managed|host-npx] [--offline] [--node=<path> --devcontainer-cli=<path>] [command]")
		return 1
	}
	if err := rejectOfflineWithEscapeHatch(*offline, *node, *devcontainerCLI, stderr); err != nil {
		return 1
	}
	command := *agent
	if command == "" && len(positionals) == 1 {
		command = positionals[0]
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
	//
	// NOTE: `command` does double duty here. It is both the PATH binary validated
	// in-image AND the agent-id key Discover passes to PublishedAgentExists ->
	// recipeForAgent (agentRecipes is keyed by agent id: "claude", "codex"). That
	// only works because binary == id for every agent today. A future agent whose
	// PATH binary differs from its recipe id would not match agentRecipes, so
	// Discover would silently fall back to TierBase and lose launch/check
	// parity. If such an agent is added, thread the binary and the agent id
	// separately into Discover instead of relying on this conflation.
	plan, err := sandboxcomposition.Discover(cwd, version.Version, command)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	toolchainMode, nodeBinary, cliEntrypoint := sandboxcontainer.ResolveToolchainSelection(*toolchain, *node, *devcontainerCLI, os.Getenv)
	result, err := sandboxCheckBuildFn(context.Background(), *runtimeName, stdout, stderr, sandboxcontainer.BuildOptions{
		WorkDir:                   cwd,
		Plan:                      plan,
		Policy:                    *buildPolicy,
		ToolchainMode:             toolchainMode,
		NodeBinary:                nodeBinary,
		DevcontainerCLIEntrypoint: cliEntrypoint,
		Offline:                   *offline,
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
		err = sandboxcomposition.EnrichValidateError(err, result.Tier, command, version.Version, cwd, runtime.GOOS, sandboxcontainer.WorkspaceRelabelActive(resolvedRuntime))
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

// runSandboxWarm pre-stages the Aileron-managed toolchain (the pinned Node
// runtime plus @devcontainers/cli) into the content-addressed cache ahead of the
// first build, so a later `aileron sandbox build --offline` resolves it with no
// network. Warm is managed-only by definition: there is nothing to pre-fetch for
// the host-npx path (it uses the host's npx at build time), so an explicit
// `--toolchain=host-npx` is rejected with a clear message. Warm is idempotent: a
// warm cache short-circuits without re-downloading.
func runSandboxWarm(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox warm", flag.ContinueOnError)
	flags.SetOutput(stderr)
	toolchain := flags.String("toolchain", sandboxcontainer.ToolchainModeAuto, "Toolchain to warm: managed (default). host-npx has nothing to pre-fetch.")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron sandbox warm [--toolchain=managed]")
		return 1
	}
	if !sandboxcontainer.IsManagedToolchain(*toolchain) {
		fmt.Fprintln(stderr, "error: `aileron sandbox warm` only applies to the managed toolchain; host-npx has nothing to pre-fetch")
		return 1
	}
	managed, err := sandboxWarmFn(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "node: %s\n", managed.NodeBinary)
	fmt.Fprintf(stdout, "devcontainer-cli: %s\n", managed.CLIEntrypoint)
	fmt.Fprintln(stdout, "warmed: ok")
	return 0
}

// sandboxWarmFn provisions the managed toolchain with all production defaults
// (network on, Offline=false), populating the content-addressed cache.
// Swappable in tests so a unit test exercises the command surface without
// network.
var sandboxWarmFn = func(ctx context.Context) (sandboxcontainer.ManagedToolchain, error) {
	return sandboxtoolchain.Provision(ctx, sandboxtoolchain.Options{})
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
	builder := sandboxcontainer.Builder{
		Runtime: runtimeName,
		Stdout:  stdout,
		Stderr:  stderr,
	}
	// Attach the real managed-toolchain provisioner only when the build selects
	// the managed toolchain (the default), so the host-npx opt-out never carries a
	// provisioner (preserving its no-network/no-provision contract). The container
	// Builder consults the provisioner only on the managed branch and only when the
	// escape hatch is absent.
	if sandboxcontainer.IsManagedToolchain(opts.ToolchainMode) {
		builder.Provisioner = sandboxtoolchain.Provisioner{
			Options: sandboxtoolchain.Options{Offline: opts.Offline},
		}
	}
	return builder.Build(ctx, opts)
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
		RemapWorkspaceUID: sandboxcontainer.WorkspaceUIDRemapActive(runtimeName),
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
