// Package container builds and runs sandbox container images from composition plans.
package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/ALRubinger/aileron/internal/sandbox/composition"
)

// hostOS is a package-level indirection over runtime.GOOS so tests can
// drive the runtime-specific argv branches without depending on the
// test runner's OS.
var hostOS = func() string { return goruntime.GOOS }

// selinuxEnforcing reports whether the host is running SELinux in
// enforcing mode. It is a package-level indirection (mirroring the hostOS
// seam) so the SELinux-relabel argv branch stays unit-testable without
// depending on the test runner's host. The kernel exposes the current
// mode at /sys/fs/selinux/enforce: the file holds "1" when enforcing and
// "0" when permissive, and is absent when SELinux is disabled or the host
// is not Linux. Any read error (absent file, non-Linux host) means "not
// enforcing", so the relabel suffix is only ever added where it is needed.
var selinuxEnforcing = func() bool {
	data, err := os.ReadFile("/sys/fs/selinux/enforce")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

const DefaultRuntime = "auto"
const WorkspacePath = "/home/agent/workspace"
const AgentImagesDocsURL = "https://docs.withaileron.ai/development/sandbox-agent-images/"

// remapAgentUIDHelper is the in-image entrypoint helper (shipped by
// images/sandbox-base) the remap-enabled launch/validate path prepends as the
// container command so the agent uid is remapped to the workspace owner before
// the agent starts (issue #1461). Centralized here so the validate prepend and
// the stale-image diagnostic in Validate name the exact same binary.
const remapAgentUIDHelper = "aileron-remap-agent-uid"

// WorkspaceUIDRemapActive reports whether a launch/validate against runtimeName
// should route the container through the aileron-remap-agent-uid entrypoint to
// align the in-container agent uid/gid with the workspace owner. The DAC uid
// mismatch only reproduces on rootful Docker on Linux: a host bind mount keeps
// the host uid, and the non-root agent user is denied write access to the 0755
// workspace. macOS/Windows Docker Desktop translate uids at the file-sharing
// boundary, so the remap is scoped to Linux+Docker, mirroring the launcher's
// AuthSpec chown guard (issue #1461). It shares the hostOS() seam with the
// SELinux-relabel decision so both stay unit-testable together.
func WorkspaceUIDRemapActive(runtimeName string) bool {
	return runtimeName == "docker" && hostOS() == "linux"
}

// WorkspaceRelabelActive reports whether runArgs adds the `:z` SELinux relabel
// suffix to host bind mounts for runtimeName: only on rootful Docker on Linux
// with SELinux in enforcing mode (PR #1460). Exported so the validate-error
// enrichment can decide whether the SELinux relabel was actually applied (and
// thus whether SELinux guidance is relevant) versus the daemon lacking SELinux
// support, where a DAC uid mismatch is the real cause (issue #1461).
func WorkspaceRelabelActive(runtimeName string) bool {
	return runtimeName == "docker" && hostOS() == "linux" && selinuxEnforcing()
}

// DevcontainerCLIVersion pins the @devcontainers/cli version Aileron uses to
// build a Tier 1 devcontainer that declares `features`. Raw `docker build`
// cannot apply Features, so the Features build path routes through
// `devcontainer build` (ADR-0017). The version is exported so the Taskfile and
// CI reference the same pin (reproducible builds, no global install state).
const DevcontainerCLIVersion = "0.87.0"

// devcontainerCLI is the package-level indirection over the @devcontainers/cli
// invocation prefix: the executable plus the leading arguments that precede the
// `build` subcommand. Resolved through `npx --yes @devcontainers/cli@<pinned>`
// so the version is reproducible without a global install. It is a var (not a
// const slice) so unit tests can assert the assembled argv deterministically
// and CI/Taskfile can override the resolution if needed. The Runner seam
// (ADR-0014) is preserved: this flows through container.Runner.Run with name =
// devcontainerCLI[0]; the CLI itself shells out to Docker, adding no second
// runtime.
var devcontainerCLI = []string{"npx", "--yes", "@devcontainers/cli@" + DevcontainerCLIVersion}

var ErrNoBuildRequired = errors.New("sandbox image does not require a build")

const (
	BuildPolicyAlways = "always"
	BuildPolicyAuto   = "auto"
	BuildPolicyNever  = "never"
)

// Runner executes a container runtime command. It exists so build planning can
// be tested without Docker on the host. The interface keeps the runtime
// seam (the runtimeName parameter threaded through Build/Run/StopContainer)
// abstract so a second runtime can be re-added as a localized change.
type Runner interface {
	Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
}

// envRunner is an optional capability a Runner may implement: run a command with
// additional environment variables layered onto the inherited process
// environment. The production execRunner implements it. The build path uses it
// to scope a buildx builder selection (BUILDX_BUILDER) to a single multi-arch
// build invocation, and falls back to plain Run when the Runner does not
// implement it (issue #2054).
type envRunner interface {
	RunWithEnv(ctx context.Context, name string, args, env []string, stdout, stderr io.Writer) error
}

// DefaultRunner returns the production Runner that shells out to the
// container runtime executable (docker). Callers outside this
// package use it to drive runtime commands (e.g. image inspection)
// through the same code path the Builder uses internally.
func DefaultRunner() Runner { return execRunner{} }

type execRunner struct{}

func (r execRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	return r.run(ctx, name, args, nil, stdout, stderr)
}

// RunWithEnv runs the runtime command with extra environment variables (each
// "KEY=value") layered onto the inherited process environment. It exists so a
// build can select a scoped buildx builder via BUILDX_BUILDER for a single
// invocation without mutating the aileron process's global environment — a
// global change would leak the selection into the sibling single-arch
// daemon-load build, which must stay on the default `docker` driver to land the
// image in the local daemon. execRunner satisfies the optional envRunner
// interface so the build path can opt into it (issue #2054).
func (r execRunner) RunWithEnv(ctx context.Context, name string, args, env []string, stdout, stderr io.Writer) error {
	return r.run(ctx, name, args, env, stdout, stderr)
}

func (execRunner) run(ctx context.Context, name string, args, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	// Put the runtime child in its own process group so a terminal
	// Ctrl-C (SIGINT to the foreground process group) reaches only
	// aileron, not `docker` directly. This keeps aileron's
	// AuthSpec salvage handler the sole owner of teardown — an orderly
	// `docker stop --time` followed by Capture — instead of racing a
	// concurrent runtime-initiated kill. No-op on Windows. See ADR-0025
	// and issue #999.
	configureRuntimeChild(cmd, args, stdinIsTerminal())

	// Interactive `run -t`/`exec -t` on a real terminal: aileron owns the
	// PTY. It allocates a pseudo-terminal, puts the host terminal in raw
	// mode (restored on exit), and hands the runtime child a PTY slave it
	// controls — so the in-container agent gets a real raw TTY while
	// aileron stays in the host terminal's foreground process group as the
	// SIGINT/SIGTERM teardown owner and the #802 approval-TUI substrate.
	// runRuntimeChildPTY returns errRunPTYUnsupported when there is no PTY
	// to own (no terminal stdin, or Windows); callers fall through to the
	// plain stdio path below. See ADR-0025 and issue #1029.
	if stdinIsTerminal() && interactiveTTYRun(args) {
		err := runRuntimeChildPTY(cmd)
		if !errors.Is(err, errRunPTYUnsupported) {
			return err
		}
	}

	// Bind the host's stdin only for an interactive run (`-i` present).
	// A batch run reads no host stdin; leaving os.Stdin attached to the
	// docker CLI's stdin pump keeps stdin open with no EOF on a real
	// terminal, so the run never returns and the caller hangs (issue
	// #1889). Mirror the arg-inspection pattern interactiveTTYRun uses.
	if interactiveRun(args) {
		cmd.Stdin = os.Stdin
	}
	return cmd.Run()
}

// configureRuntimeChild sets the process-group attributes for a runtime
// child. It isolates the child into its own process group so a terminal
// Ctrl-C reaches aileron rather than docker directly (ADR-0025, issue
// #999). Aileron itself stays in the controlling terminal's foreground
// process group so it owns SIGINT/SIGTERM teardown and is the #802
// approval-TUI substrate. For an interactive `run -t`/`exec -t` the
// child no longer needs the terminal's foreground group: aileron
// allocates a PTY and hands the child a slave it controls (runtime.go
// PTY path), so the child's raw-mode tcsetattr targets that slave rather
// than the host terminal. The stdinTTY/args parameters are retained for
// the foreground-gating contract test and any future runtime that opts
// back into the foreground handoff. See issue #1029.
func configureRuntimeChild(cmd *exec.Cmd, _ []string, _ bool) {
	setRuntimeChildPgid(cmd)
}

// stdinIsTerminal reports whether the process's stdin is a real
// terminal. Package-level so tests can drive the foreground branch in
// execRunner.Run without a controlling TTY.
var stdinIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// interactiveTTYRun reports whether args describe an interactive
// container `run` or `exec` that allocates a pseudo-TTY (`-t`). Only
// such a command needs the runtime child to own the terminal's
// foreground process group — without it docker's raw-mode tcsetattr
// runs from a background group and fails with SIGTTOU/EINTR. Both the
// interactive `run -t` (an agent shell) and `exec -t` (the gh
// device-flow login in `aileron auth github`) need this. Image builds
// (`build -t <tag>`) do not — the verb gate keeps the `-t` build-tag
// flag from matching.
func interactiveTTYRun(args []string) bool {
	if len(args) == 0 || (args[0] != "run" && args[0] != "exec") {
		return false
	}
	for _, a := range args {
		if a == "-t" {
			return true
		}
	}
	return false
}

// interactiveRun reports whether args describe a container `run` or
// `exec` that keeps STDIN open (`-i`). Only such a command needs the
// host's os.Stdin bound to the runtime child; a batch run reads no host
// stdin, and binding it would leave stdin open with no EOF on a real
// terminal so the run never returns (issue #1889).
func interactiveRun(args []string) bool {
	if len(args) == 0 || (args[0] != "run" && args[0] != "exec") {
		return false
	}
	for _, a := range args {
		if a == "-i" {
			return true
		}
	}
	return false
}

// ManagedToolchain describes the resolved Aileron-managed Node + devcontainer
// CLI paths the managed build branch invokes. NodeBinary is the absolute path
// to the managed node executable; CLIEntrypoint is the absolute path to the
// @devcontainers/cli JS entrypoint the managed node runs.
type ManagedToolchain struct {
	NodeBinary    string
	CLIEntrypoint string
}

// toolchainProvisioner provisions (fetch + verify + cache) the managed Node and
// pinned @devcontainers/cli on demand and returns their resolved paths. It is
// the seam (ADR-0014 style) that keeps the container package Docker-free and
// network-free in unit tests: the host-npx opt-out path never calls it, and the
// managed path (the default) is exercised with a fake in tests. The real
// implementation lives in internal/sandbox/toolchain.
type toolchainProvisioner interface {
	Provision(ctx context.Context) (ManagedToolchain, error)
}

// Builder builds the image required by a sandbox composition plan.
type Builder struct {
	Runtime string
	Runner  Runner
	Stdout  io.Writer
	Stderr  io.Writer
	// Provisioner supplies the managed toolchain when a Features build selects
	// ToolchainModeManaged (the default) without the escape hatch. nil means "no
	// managed provisioning wired": the host-npx opt-out path never needs it, and a
	// managed request with nil Provisioner and no escape hatch fails fast rather
	// than silently falling back. Unit tests inject a fake to exercise the
	// managed branch with no network or Docker.
	Provisioner toolchainProvisioner
}

// Toolchain selection modes for a Features (Tier 1) devcontainer build. The
// build needs a Node runtime plus @devcontainers/cli; ToolchainModeManaged (the
// default) provisions an Aileron-managed Node + pinned CLI so Docker is the only
// host prerequisite (#1525), while ToolchainModeHostNPX resolves both through the
// host's `npx`. The default is managed (#1530); host-npx is the explicit opt-out.
const (
	// ToolchainModeAuto is the empty/default selection: it resolves to the
	// managed toolchain.
	ToolchainModeAuto = ""
	// ToolchainModeHostNPX explicitly selects the host-`npx` invocation (the
	// opt-out from the managed default).
	ToolchainModeHostNPX = "host-npx"
	// ToolchainModeManaged selects the Aileron-managed toolchain: a verified
	// managed Node binary running the pinned @devcontainers/cli entrypoint, so
	// the build needs no host Node/npx.
	ToolchainModeManaged = "managed"
)

// BuildOptions configures a sandbox image build.
type BuildOptions struct {
	WorkDir string
	Plan    composition.Plan
	Tag     string
	Policy  string
	// ToolchainMode selects how a Features (Tier 1) devcontainer build
	// resolves Node + @devcontainers/cli: ToolchainModeAuto/ToolchainModeManaged
	// (default) provision an Aileron-managed Node + pinned CLI; ToolchainModeHostNPX
	// uses the host `npx`. Ignored for every other tier.
	ToolchainMode string
	// NodeBinary and DevcontainerCLIEntrypoint are the managed-toolchain escape
	// hatch: when both are set, the managed branch runs exactly those paths
	// (`<NodeBinary> <DevcontainerCLIEntrypoint> build …`) and skips the
	// provisioner entirely, so a hermetic/offline host can point at a
	// pre-staged Node + CLI rather than fetching. Both must exist on disk.
	NodeBinary                string
	DevcontainerCLIEntrypoint string
	// Offline requests that a managed-toolchain build resolve Node +
	// @devcontainers/cli from the warm content-addressed cache with no network
	// access (#1531). The Builder itself never reads this field; the CLI layer
	// that constructs the toolchain Provisioner carries it into
	// toolchain.Options.Offline, so a build with --offline fails fast with a
	// `run aileron sandbox warm` hint on a cold cache instead of fetching. It is
	// ignored when ToolchainMode is host-npx; the CLI rejects combining --offline
	// with the --node/--devcontainer-cli escape hatch up front, so the escape
	// hatch never reaches a build with Offline set.
	Offline bool
	// Platforms requests a multi-architecture build. When non-empty the build
	// routes through `docker buildx build` (raw tiers) or `devcontainer build`
	// with `--platform <p1,p2,...>`, and OCILayoutDest must be set so the built
	// manifest list is written to a local OCI image layout rather than loaded
	// into the daemon. The freeze producer sets composition.MultiArchPlatforms so
	// the composed image is built for both linux/amd64 and linux/arm64 (ADR-0027,
	// issue #2036). Empty keeps the single-arch, daemon-loading build byte-for-byte
	// identical to before.
	Platforms []string
	// OCILayoutDest is the directory a multi-arch build (Platforms set) writes its
	// OCI image layout to, via `--output type=oci,dest=<dir>,tar=false`. The freeze
	// producer reads the per-arch config-digest set back from this layout and hands
	// it to publish (S3/S4 seam); it is not an ephemeral temp dir. Ignored when
	// Platforms is empty.
	OCILayoutDest string
	// BuildxBuilder, when set, selects a specific buildx builder instance for a
	// multi-arch build by exporting BUILDX_BUILDER=<name> in the build
	// subprocess environment (rather than `docker buildx use`, which would
	// repoint the operator's default builder). It is honored ONLY for a
	// multi-arch build (Platforms set): the single-arch daemon-load build must
	// run on the default `docker` driver so the composed image lands in the
	// local daemon, and a docker-container builder cannot load into the daemon.
	// The freeze producer sets it to the dedicated `aileron-freeze`
	// docker-container builder (issue #2054). Ignored when Platforms is empty.
	BuildxBuilder string
}

// BuildResult reports the image selected or built for launch.
type BuildResult struct {
	Runtime string
	Image   string
	Built   bool
	Tier    composition.Tier
	// OCILayoutDir is the directory a multi-arch build wrote its OCI image layout
	// to (echoing BuildOptions.OCILayoutDest). It is empty for a single-arch,
	// daemon-loading build. The freeze producer reads the per-arch config-digest
	// set back from it (issue #2036).
	OCILayoutDir string
}

// RunOptions configures one sandbox container execution.
type RunOptions struct {
	Runtime string
	Image   string
	WorkDir string
	Env     map[string]string
	Volumes []Volume
	Command []string
	User    string
	// Interactive maps to docker's `-i` (keep STDIN open even if not
	// attached). Set it for runs whose in-container process reads the
	// host's stdin (an agent shell). Leave it false for batch runs that
	// read no host stdin: a batch `docker run -i` on a real terminal
	// leaves stdin attached with no EOF, so the run never returns and the
	// caller hangs (issue #1889).
	Interactive bool
	TTY         bool
	// Name, when non-empty, is passed as `--name <Name>` so the
	// container is addressable for a later `stop` (the AuthSpec
	// graceful-shutdown salvage path; see ADR-0025). An anonymous
	// container cannot be stopped by name, so the launcher generates a
	// deterministic per-session name and reuses it for StopContainer.
	Name string
}

// Volume describes an additional mount for a sandbox container. By default
// Source is a host path bind-mounted at Target. When Named is set, Source is a
// Docker named volume instead: it is emitted verbatim as <name>:<target>
// (no host-path resolution), and Docker auto-creates the volume on first mount.
type Volume struct {
	Source   string
	Target   string
	ReadOnly bool
	// Named marks Source as a Docker named volume rather than a host path. The
	// runtime emits the mount without resolving Source to an absolute path
	// (filepath.Abs would mangle a volume name), letting Docker create and reuse
	// a persistent managed volume. Used for Aileron-managed sandbox caches.
	Named bool
}

// RunResult reports the selected runtime after a sandbox container exits.
type RunResult struct {
	Runtime string
}

// ValidateOptions configures a launch-time sandbox image validation run.
type ValidateOptions struct {
	Runtime           string
	Image             string
	WorkDir           string
	Env               map[string]string
	Volumes           []Volume
	Command           []string
	RequireProxyTrust bool
	// RequireMCPBinary asserts that aileron-mcp is present on the
	// container's PATH and runs (smoke-checked via `aileron-mcp
	// --version`). Set by the sandbox launcher whenever MCP wiring is
	// active. The two-step check catches both missing-binary and the
	// cross-arch ENOEXEC case where `command -v` succeeds but the
	// binary fails to exec inside the container. See ADR-0024.
	RequireMCPBinary bool
	// RemapWorkspaceUID routes the validation run through the
	// aileron-remap-agent-uid entrypoint exactly as the real launch does:
	// the container starts as root, the helper remaps the agent uid/gid to
	// the workspace owner, then drops to the agent user before the
	// writability probe runs. Without this the probe runs as the image's
	// default agent uid and would still report exit 3 on a DAC mismatch even
	// after the launch path is fixed, so validation must mirror the run. Set
	// by the launcher / `sandbox check` on Linux+Docker (issue #1461).
	RemapWorkspaceUID bool
}

// Build builds the image for plan. Tier 0 builds Aileron's local sandbox-base
// definition. Tier 1 builds a devcontainer Dockerfile when present. Tier 2 is a
// BYO image and intentionally does not perform runtime injection yet.
func (b Builder) Build(ctx context.Context, opts BuildOptions) (BuildResult, error) {
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = "."
	}
	runner := b.Runner
	if runner == nil {
		runner = execRunner{}
	}
	stdout := b.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := b.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	// Per-invocation environment for the build subprocess. It carries a scoped
	// BUILDX_BUILDER selection for a multi-arch build (issue #2054). It is nil for
	// a single-arch build, keeping that argv+env byte-identical to before.
	buildEnv := buildStepEnv(opts)

	// A multi-arch build and its OCI-layout output are one atomic request: the
	// `--output type=oci,dest=<dir>` token is only emitted when both are set, so a
	// half-configured pairing would silently drop the multi-arch intent or emit an
	// empty dest. Fail fast rather than deferring to a confusing buildx error.
	if len(opts.Platforms) > 0 && strings.TrimSpace(opts.OCILayoutDest) == "" {
		return BuildResult{}, fmt.Errorf("multi-arch build requested (platforms %s) without an OCILayoutDest", strings.Join(opts.Platforms, ","))
	}
	if strings.TrimSpace(opts.OCILayoutDest) != "" && len(opts.Platforms) == 0 {
		return BuildResult{}, fmt.Errorf("OCILayoutDest %q set without any Platforms; the OCI-layout output only applies to a multi-arch build", opts.OCILayoutDest)
	}

	image := opts.Tag
	if image == "" {
		image = opts.Plan.Image
	}
	result := BuildResult{Image: image, Tier: opts.Plan.Tier}
	policy, err := normalizeBuildPolicy(opts.Policy)
	if err != nil {
		return BuildResult{}, err
	}
	switch opts.Plan.Tier {
	case composition.TierBase:
		runtimeName, err := resolveRuntime(b.Runtime, b.Runner == nil)
		if err != nil {
			return BuildResult{}, err
		}
		result.Runtime = runtimeName
		shouldBuild, err := b.shouldBuild(ctx, runner, runtimeName, image, policy)
		if err != nil {
			return BuildResult{}, err
		}
		if !shouldBuild {
			return result, nil
		}
		args, err := baseBuildArgs(workDir, image, opts.Platforms, opts.OCILayoutDest)
		if err != nil {
			return BuildResult{}, err
		}
		if err := runBuildStep(ctx, runner, runtimeName, args, buildEnv, stdout, stderr); err != nil {
			return BuildResult{}, err
		}
		result.Built = true
		result.OCILayoutDir = multiArchLayoutDir(opts)
		return result, nil
	case composition.TierDevcontainer:
		useFeatures := len(opts.Plan.Features) > 0
		// A Tier 1 devcontainer with neither a Dockerfile nor features has
		// nothing to build (it FROMs a base image directly); treat it as
		// BYO-like. Features require a build even without a Dockerfile —
		// @devcontainers/cli resolves the base from the devcontainer config —
		// so a features-only plan must NOT short-circuit here.
		if opts.Plan.DockerfilePath == "" && !useFeatures {
			result.Image = opts.Plan.Image
			return result, ErrNoBuildRequired
		}
		runtimeName, err := resolveRuntime(b.Runtime, b.Runner == nil)
		if err != nil {
			return BuildResult{}, err
		}
		result.Runtime = runtimeName
		// A synthesized-devcontainer plan (Discover's no-.devcontainer local
		// agent build, #1451) carries its own deterministic local image tag in
		// Plan.Image and is NOT keyed off workDir: the build reads a managed
		// temp workspace folder, not workDir, so ProjectImageTag(workDir) would
		// be the wrong (and non-deterministic across launches) name. Honor
		// Plan.Image for that case; otherwise the project-local image tag.
		if opts.Tag == "" {
			if opts.Plan.SynthesizedDevcontainer != "" {
				result.Image = opts.Plan.Image
			} else {
				result.Image = ProjectImageTag(workDir)
			}
		}
		shouldBuild, err := b.shouldBuild(ctx, runner, runtimeName, result.Image, policy)
		if err != nil {
			return BuildResult{}, err
		}
		if !shouldBuild {
			return result, nil
		}
		// Features must be applied by @devcontainers/cli; raw `docker build`
		// cannot apply them. The CLI shells out to Docker under the hood, so
		// the Runner/runtimeName seam (ADR-0014) is preserved — runtimeName is
		// still resolved above so the Docker-only guard fires and the result
		// reports the runtime, but the CLI executable is what we exec.
		if useFeatures {
			prefix, err := b.resolveDevcontainerCLI(ctx, opts)
			if err != nil {
				return BuildResult{}, err
			}
			// For a synthesized-devcontainer plan there is no .devcontainer on
			// disk under workDir; materialize the composed config into a
			// managed temp workspace folder and build from there so the user's
			// workDir is never written to (#1451). The run path still mounts
			// the real workDir; only the build reads the temp folder.
			buildWorkDir := workDir
			if opts.Plan.SynthesizedDevcontainer != "" {
				tmp, cleanup, err := materializeSynthesizedWorkspace(opts.Plan.SynthesizedDevcontainer)
				if err != nil {
					return BuildResult{}, err
				}
				defer cleanup()
				buildWorkDir = tmp
			}
			name, args, err := devcontainerCLIBuildArgs(prefix, buildWorkDir, opts.Plan, result.Image, opts.Platforms, opts.OCILayoutDest)
			if err != nil {
				return BuildResult{}, err
			}
			if err := runBuildStep(ctx, runner, name, args, buildEnv, stdout, stderr); err != nil {
				return BuildResult{}, err
			}
			result.Built = true
			result.OCILayoutDir = multiArchLayoutDir(opts)
			return result, nil
		}
		args, err := devcontainerBuildArgs(workDir, opts.Plan, result.Image, opts.Platforms, opts.OCILayoutDest)
		if err != nil {
			return BuildResult{}, err
		}
		if err := runBuildStep(ctx, runner, runtimeName, args, buildEnv, stdout, stderr); err != nil {
			return BuildResult{}, err
		}
		result.Built = true
		result.OCILayoutDir = multiArchLayoutDir(opts)
		return result, nil
	case composition.TierPublished:
		// A published per-agent image is pulled, never built locally — that is
		// the whole point of the build-free default. The Runner/runtimeName seam
		// (ADR-0014) is preserved: the pull threads runtimeName exactly like the
		// build path. A pull is not a local build, so Built stays false; the
		// launcher keys image env wiring off Image, not Built.
		runtimeName, err := resolveRuntime(b.Runtime, b.Runner == nil)
		if err != nil {
			return BuildResult{}, err
		}
		result.Runtime = runtimeName
		result.Image = opts.Plan.Image
		switch policy {
		case BuildPolicyNever:
			if !b.imageExists(ctx, runner, runtimeName, result.Image) {
				return BuildResult{}, fmt.Errorf("sandbox image %s not found locally and sandbox build policy is never; run `%s pull %s` or use --sandbox-build=auto|always", result.Image, runtimeName, result.Image)
			}
			return result, nil
		case BuildPolicyAuto:
			// A floating tag (edge/latest) keeps its name while its upstream
			// digest moves, so a stale local copy would permanently satisfy the
			// existence check and `auto` would never re-pull. Only honor the
			// existence short-circuit for version-pinned tags; floating tags fall
			// through to the pull below so they re-resolve to the current digest
			// each launch (issue #1174).
			if !isFloatingTag(result.Image) && b.imageExists(ctx, runner, runtimeName, result.Image) {
				return result, nil
			}
		}
		// BuildPolicyAuto with the image absent, or BuildPolicyAlways: pull it.
		pullArgs := []string{"pull", result.Image}
		if err := runBuildStep(ctx, runner, runtimeName, pullArgs, nil, stdout, stderr); err != nil {
			return BuildResult{}, err
		}
		return result, nil
	case composition.TierBYOImage:
		result.Image = opts.Plan.Image
		return result, ErrNoBuildRequired
	default:
		return BuildResult{}, fmt.Errorf("unsupported sandbox composition tier %q", opts.Plan.Tier)
	}
}

// daemonUnreachableMessage is the friendly headline surfaced when a sandbox
// build/pull fails because the container daemon could not be reached (Docker
// Desktop not running, the npipe/socket absent). The underlying technical
// error is still chained via %w so the raw transport diagnostic remains
// available for deeper troubleshooting; it is just no longer the headline.
const daemonUnreachableMessage = "Docker isn't reachable; make sure Docker Desktop is running and available, then try again"

// runBuildStep runs a single container build/pull command and translates a
// daemon-unreachable failure into a friendly headline. It mirrors the
// tee-and-translate convention StopContainer and Validate already use: the
// runtime's stderr is teed into a buffer so the failure cause can be inspected
// without swallowing the live output the user sees. On a daemon-unreachable
// signature the returned error leads with daemonUnreachableMessage while still
// chaining the original `<runtime> <args>: <exit>` error via %w, so
// errors.Is/Unwrap continue to expose the underlying diagnostic. All other
// failures keep the established `%s %s: %w` framing verbatim.
//
// env carries extra "KEY=value" environment entries for this single invocation
// (e.g. BUILDX_BUILDER to scope a multi-arch build to a dedicated builder). It is
// applied only when the Runner implements envRunner; a plain Runner ignores it.
func runBuildStep(ctx context.Context, runner Runner, runtimeName string, args, env []string, stdout, stderr io.Writer) error {
	var stderrBuf bytes.Buffer
	teedStderr := io.MultiWriter(stderr, &stderrBuf)
	var err error
	if er, ok := runner.(envRunner); ok && len(env) > 0 {
		err = er.RunWithEnv(ctx, runtimeName, args, env, stdout, teedStderr)
	} else {
		err = runner.Run(ctx, runtimeName, args, stdout, teedStderr)
	}
	if err == nil {
		return nil
	}
	wrapped := fmt.Errorf("%s %s: %w", runtimeName, strings.Join(args, " "), err)
	if detectDaemonUnreachable(stderrBuf.String()) {
		return fmt.Errorf("%s: %w", daemonUnreachableMessage, wrapped)
	}
	return wrapped
}

// detectDaemonUnreachable reports whether a container build/pull error's stderr
// indicates the container daemon could not be reached. Matched
// case-insensitively against the signatures the Docker CLI emits on each
// platform: Windows npipe (`failed to connect to the docker API`, `the system
// cannot find the file specified`) and macOS/Linux (`cannot connect to the
// docker daemon`). See runBuildStep and issue #1496.
func detectDaemonUnreachable(stderr string) bool {
	lowered := strings.ToLower(stderr)
	for _, signature := range []string{
		"cannot connect to the docker daemon",
		"failed to connect to the docker api",
		"the system cannot find the file specified",
	} {
		if strings.Contains(lowered, signature) {
			return true
		}
	}
	return false
}

func normalizeBuildPolicy(policy string) (string, error) {
	normalized := strings.TrimSpace(policy)
	switch normalized {
	case "", BuildPolicyAlways:
		return BuildPolicyAlways, nil
	case BuildPolicyAuto:
		return BuildPolicyAuto, nil
	case BuildPolicyNever:
		return BuildPolicyNever, nil
	default:
		return "", fmt.Errorf("unsupported sandbox build policy %q (want auto, always, or never)", normalized)
	}
}

func (b Builder) shouldBuild(ctx context.Context, runner Runner, runtimeName, image, policy string) (bool, error) {
	switch policy {
	case BuildPolicyAlways:
		return true, nil
	case BuildPolicyAuto:
		return !b.imageExists(ctx, runner, runtimeName, image), nil
	case BuildPolicyNever:
		if b.imageExists(ctx, runner, runtimeName, image) {
			return false, nil
		}
		return false, fmt.Errorf("sandbox image %s not found locally and sandbox build policy is never; run `aileron sandbox build --runtime=%s` or use --sandbox-build=auto|always", image, runtimeName)
	default:
		return false, fmt.Errorf("unsupported sandbox build policy %q (want auto, always, or never)", policy)
	}
}

// isFloatingTag reports whether an image reference carries a floating tag
// (`edge` or `latest`) whose upstream digest can move while the tag name stays
// fixed. The composition layer assigns these tags (composition.imageTag), so a
// local copy of a floating tag can go stale silently. Version-pinned tags (and
// digest references) are stable, so callers may safely cache them; floating tags
// must re-pull under `auto` to track the current digest (issue #1174).
//
// The tag is the segment after the last `:` that follows the final `/` (so a
// registry host:port prefix like `ghcr.io:443/x` is not mistaken for a tag).
// A reference with no tag segment defaults to `latest`, matching Docker's own
// implicit-tag behavior, and is therefore treated as floating.
func isFloatingTag(image string) bool {
	switch parseImageTag(image) {
	case "edge", "latest":
		return true
	default:
		return false
	}
}

// parseImageTag extracts the tag segment from an image reference. It returns the
// substring after the last `:` that appears after the final `/`, or `latest`
// when the reference carries no tag (Docker's implicit default).
func parseImageTag(image string) string {
	ref := image
	if slash := strings.LastIndex(ref, "/"); slash != -1 {
		ref = ref[slash+1:]
	}
	if colon := strings.LastIndex(ref, ":"); colon != -1 {
		return ref[colon+1:]
	}
	return "latest"
}

func (b Builder) imageExists(ctx context.Context, runner Runner, runtimeName, image string) bool {
	return runner.Run(ctx, runtimeName, []string{"image", "inspect", image}, io.Discard, io.Discard) == nil
}

// MCPVersionLabel is the OCI image label the published sandbox-base image
// carries to advertise the aileron-mcp version baked into it (issue #957).
// The launcher reads it to detect a baked image and skip the host-mount;
// an unlabeled image has no baked binary and takes the host-mount fallback
// (ADR-0024). The label is stamped by images/sandbox-base/Containerfile.published;
// keep this string in sync with that file (the Containerfile cannot import it).
const MCPVersionLabel = "ai.aileron.mcp.version"

// imageLabel reports the trimmed value of a single OCI label on image, read via
// `<runtime> image inspect --format`. It returns "" when the image carries no
// such label, is not present locally, or the inspect fails, and it maps the Go
// template "<no value>" sentinel (rendered for a missing key) to "". A nil
// runner defaults to the production execRunner. This is the shared fail-soft
// inspect plumbing behind BakedMCPVersion, ImageMetadataLabel, and
// BakedCLIVersion: "cannot determine" always degrades to "", never a fatal.
func imageLabel(ctx context.Context, runner Runner, runtimeName, image, label string) string {
	if runner == nil {
		runner = execRunner{}
	}
	var stdout bytes.Buffer
	format := "{{ index .Config.Labels \"" + label + "\" }}"
	args := []string{"image", "inspect", "--format", format, image}
	if err := runner.Run(ctx, runtimeName, args, &stdout, io.Discard); err != nil {
		return ""
	}
	out := strings.TrimSpace(stdout.String())
	// An image with no labels at all renders the missing key as the Go
	// template "<no value>" sentinel rather than an empty string; treat it
	// as absent.
	if out == "<no value>" {
		return ""
	}
	return out
}

// BakedMCPVersion reports the aileron-mcp version baked into image, read from
// the MCPVersionLabel OCI label via `<runtime> image inspect`. It returns the
// trimmed label value, or "" when the image is unlabeled, not present locally,
// or the inspect fails. Callers treat "" as "not baked" and fall back to the
// host-mount path, so an inspect error is deliberately not propagated as fatal:
// "cannot determine" degrades to "host-mount", never a broken launch.
func BakedMCPVersion(ctx context.Context, runner Runner, runtimeName, image string) string {
	return imageLabel(ctx, runner, runtimeName, image, MCPVersionLabel)
}

// CLIVersionLabel is the OCI image label the published sandbox-base image
// carries to advertise the aileron CLI version baked into it (issue #1807).
// The image-boot re-entry runs that baked binary against the host's launch
// (ADR-0027 / #1731), so the launcher reads this label to detect version skew
// between the pinned image and the booting host and warn the operator. The
// label is stamped by images/sandbox-base/Containerfile.published; keep this
// string in sync with that file (the Containerfile cannot import it).
const CLIVersionLabel = "ai.aileron.cli.version"

// BakedCLIVersion reports the aileron CLI version baked into image, read from
// the CLIVersionLabel OCI label via `<runtime> image inspect`. It returns the
// trimmed label value, or "" when the image is unlabeled, not present locally,
// or the inspect fails. Callers treat "" as "cannot determine" and stay silent
// (an unlabeled custom image is the operator's contract under ADR-0027), so an
// inspect error is deliberately not propagated as fatal: skew detection degrades
// to "no warning", never a broken launch.
func BakedCLIVersion(ctx context.Context, runner Runner, runtimeName, image string) string {
	return imageLabel(ctx, runner, runtimeName, image, CLIVersionLabel)
}

// DevcontainerMetadataLabel is the OCI image label the devcontainer CLI
// stamps on a built image to carry the merged per-Feature metadata array
// (umbrella #1319). Each element of that JSON array may carry a
// customizations.aileron.cli unit declaring one CLI tool's credential
// acquisition and sealing. The host unit loader reads this label to fan
// those units out to the capture registry and the proxybinding table. The
// label name is the devcontainer CLI's own convention; keep this string in
// sync with the published sandbox image (verified in PR #1350).
const DevcontainerMetadataLabel = "devcontainer.metadata"

// ImageMetadataLabel reports the raw devcontainer.metadata OCI label value
// for image, read via `<runtime> image inspect`. It returns the trimmed
// label value, or "" when the image carries no such label, is not present
// locally, or the inspect fails. Callers treat "" as "no unit to load" and
// degrade to the embedded defaults only, so an inspect error is deliberately
// not propagated as fatal: "cannot determine" degrades to "no unit-derived
// layer", never a broken startup. This mirrors BakedMCPVersion's fail-soft
// posture and inspect plumbing.
func ImageMetadataLabel(ctx context.Context, runner Runner, runtimeName, image string) string {
	return imageLabel(ctx, runner, runtimeName, image, DevcontainerMetadataLabel)
}

// Run starts a one-shot sandbox container for an agent command.
func (b Builder) Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	if opts.Image == "" {
		return RunResult{}, fmt.Errorf("sandbox image is required")
	}
	if len(opts.Command) == 0 || strings.TrimSpace(opts.Command[0]) == "" {
		return RunResult{}, fmt.Errorf("sandbox command is required")
	}
	runner := b.Runner
	if runner == nil {
		runner = execRunner{}
	}
	stdout := b.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := b.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	runtimeName, err := resolveRuntime(firstNonEmpty(opts.Runtime, b.Runtime), b.Runner == nil)
	if err != nil {
		return RunResult{}, err
	}
	args, err := runArgs(runtimeName, opts)
	if err != nil {
		return RunResult{}, err
	}
	if err := runner.Run(ctx, runtimeName, args, stdout, stderr); err != nil {
		return RunResult{}, err
	}
	return RunResult{Runtime: runtimeName}, nil
}

// StopContainer issues `<runtimeName> stop --time <graceSeconds> <name>`
// through runner so the AuthSpec salvage path can request a graceful
// container shutdown on SIGINT/SIGTERM (see ADR-0025). graceSeconds is
// the runtime's own SIGTERM-to-SIGKILL grace window. An empty name is a
// no-op so callers never have to special-case an anonymous container,
// and a nil runner falls back to the real runtime so the launcher path
// works without wiring.
//
// On a normal Ctrl-C the launcher runs containers with `--rm`, so the
// runtime often auto-removes the container before this salvage stop
// runs. The resulting "No such container" / "is not running" stop error
// is benign: the container is already gone, which is exactly what the
// stop wanted. To avoid alarming the user with a spurious warning on the
// common clean-Ctrl-C path, StopContainer tees the runtime's stderr into
// a buffer and treats an already-gone signature as success (returns
// nil). Genuine errors (permission denied, daemon unreachable) are
// returned verbatim so the caller can still warn.
func StopContainer(ctx context.Context, runner Runner, runtimeName, name string, graceSeconds int, stdout, stderr io.Writer) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	if runner == nil {
		runner = execRunner{}
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var stderrBuf bytes.Buffer
	teedStderr := io.MultiWriter(stderr, &stderrBuf)
	args := []string{"stop", "--time", strconv.Itoa(graceSeconds), name}
	err := runner.Run(ctx, runtimeName, args, stdout, teedStderr)
	if err != nil && isContainerAlreadyGone(stderrBuf.String()) {
		return nil
	}
	return err
}

// isContainerAlreadyGone reports whether a container-runtime stop error's
// stderr indicates the target container was already removed or stopped.
// Matched case-insensitively against the signatures Docker
// emits for a missing or non-running container, so the `--rm`
// auto-removal race on a clean Ctrl-C does not surface as a warning. See
// StopContainer and issue #999.
func isContainerAlreadyGone(stderr string) bool {
	lowered := strings.ToLower(stderr)
	for _, signature := range []string{
		"no such container",
		"is not running",
		"already stopped",
	} {
		if strings.Contains(lowered, signature) {
			return true
		}
	}
	return false
}

// Validate checks that an image can satisfy the minimal launch-time sandbox
// runtime contract: shell command execution, the mounted workspace as CWD, a
// writable workspace mount, and the requested agent command on PATH.
func (b Builder) Validate(ctx context.Context, opts ValidateOptions) error {
	if opts.Image == "" {
		return fmt.Errorf("sandbox image is required")
	}
	if len(opts.Command) == 0 || strings.TrimSpace(opts.Command[0]) == "" {
		return fmt.Errorf("sandbox command is required")
	}
	var stderr bytes.Buffer
	builder := b
	if builder.Stderr == nil {
		builder.Stderr = &stderr
	}
	// The validation script runs as the image's default agent user. When the
	// real launch remaps the agent uid to the workspace owner (Linux+Docker,
	// issue #1461), the probe must run through the same remap or it would
	// report exit 3 on a DAC mismatch the launch path no longer hits. Prepend
	// the remap entrypoint and start as root; the helper drops to the agent
	// user via su-exec before exec'ing /bin/sh, so the probe sees the remapped
	// identity exactly as the agent will at runtime.
	command := []string{
		"/bin/sh",
		"-c",
		validationScript,
		"aileron-validate",
		opts.Command[0],
		boolArg(opts.RequireProxyTrust),
		boolArg(opts.RequireMCPBinary),
	}
	user := ""
	if opts.RemapWorkspaceUID {
		user = "root"
		command = append([]string{remapAgentUIDHelper, "su-exec", "agent"}, command...)
	}
	_, err := builder.Run(ctx, RunOptions{
		Runtime: opts.Runtime,
		Image:   opts.Image,
		WorkDir: opts.WorkDir,
		Env:     opts.Env,
		Volumes: opts.Volumes,
		User:    user,
		Command: command,
	})
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(stderr.String())
	// A remap-enabled validate prepends aileron-remap-agent-uid as the literal
	// container command (see above). When the image predates the entrypoint
	// contract and lacks that helper, the image's tini entrypoint fails to exec
	// it (exit 127) BEFORE the validation script ever runs, so none of the
	// script's friendly diagnostics are produced. Detect that tini exec-failure
	// signature and translate it into an actionable message naming the stale
	// image, instead of letting the opaque tini error reach the launcher. See
	// issue #1466.
	if opts.RemapWorkspaceUID && missingEntrypointHelperSignature(detail) {
		return fmt.Errorf("validate sandbox image %s: %s: %w", opts.Image, staleEntrypointContractMessage, err)
	}
	if detail != "" {
		detail = sandboxValidationDetail(detail)
		return fmt.Errorf("validate sandbox image %s: %s: %w", opts.Image, detail, err)
	}
	return fmt.Errorf("validate sandbox image %s: image must support /bin/sh command execution, a writable %s workspace mount, and agent command %q on PATH; see %s: %w", opts.Image, WorkspacePath, opts.Command[0], AgentImagesDocsURL, err)
}

// staleEntrypointContractMessage is surfaced when a remap-enabled launch hits a
// sandbox image whose tini entrypoint cannot exec the aileron-remap-agent-uid
// helper because the image predates the launcher's in-image entrypoint
// contract. It tells the operator the concrete remedy: rebuild the edge images.
const staleEntrypointContractMessage = "sandbox image predates this launcher's entrypoint contract: the in-image " +
	remapAgentUIDHelper + " helper is missing, so the container entrypoint fails before launch. " +
	"Rebuild the base+agent edge images (or pull a newer edge tag); see " + AgentImagesDocsURL

// missingEntrypointHelperSignature reports whether the container runtime's
// stderr carries the tini exec-failure signature emitted when PID 1 cannot
// exec the aileron-remap-agent-uid helper (the helper is absent from a stale
// image). tini prints `exec <prog> failed: No such file or directory` and
// exits 127; matching the helper name plus the "exec ... failed" framing keeps
// this from firing on unrelated stderr that merely mentions the helper.
func missingEntrypointHelperSignature(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "exec "+remapAgentUIDHelper+" failed") ||
		(strings.Contains(lowered, remapAgentUIDHelper) &&
			strings.Contains(lowered, "no such file or directory"))
}

func sandboxValidationDetail(detail string) string {
	if strings.Contains(detail, "agent command not found in sandbox image:") && !strings.Contains(detail, AgentImagesDocsURL) {
		return detail + "\nagent image recipes: " + AgentImagesDocsURL
	}
	return detail
}

const validationScript = `
if [ "$(pwd)" != "` + WorkspacePath + `" ]; then
  echo "sandbox working directory is $(pwd), want ` + WorkspacePath + `" >&2
  exit 2
fi
probe=".aileron-sandbox-validate-$$"
if ! ( : > "$probe" ) 2>/dev/null; then
  echo "sandbox workspace is not writable at ` + WorkspacePath + `" >&2
  exit 3
fi
rm -f "$probe"
if ! command -v "$1" >/dev/null 2>&1; then
  echo "agent command not found in sandbox image: $1" >&2
  echo "install the agent CLI in the sandbox image or launch with --local" >&2
  echo "agent image recipes: ` + AgentImagesDocsURL + `" >&2
  exit 127
fi
if [ "${2:-0}" = "1" ]; then
  if ! command -v aileron-install-proxy-ca >/dev/null 2>&1; then
    echo "sandbox proxy bootstrap requires aileron-install-proxy-ca in the sandbox image" >&2
    echo "extend the current ghcr.io/alrubinger/aileron-sandbox-base image or disable AILERON_SANDBOX_PROXY_BOOTSTRAP" >&2
    exit 127
  fi
  if ! command -v aileron-run-with-proxy-ca >/dev/null 2>&1; then
    echo "sandbox proxy bootstrap requires aileron-run-with-proxy-ca in the sandbox image" >&2
    echo "extend the current ghcr.io/alrubinger/aileron-sandbox-base image or disable AILERON_SANDBOX_PROXY_BOOTSTRAP" >&2
    exit 127
  fi
  aileron-install-proxy-ca --check "${AILERON_SANDBOX_PROXY_CA_FILE:-/etc/aileron/proxy/ca.pem}"
fi
if [ "${3:-0}" = "1" ]; then
  if ! command -v aileron-mcp >/dev/null 2>&1; then
    echo "aileron-mcp not on PATH; sandbox MCP wiring failed (see ADR-0024)" >&2
    exit 127
  fi
  if ! aileron-mcp --version >/dev/null 2>&1; then
    echo "aileron-mcp on PATH but not executable in this container (arch mismatch or corrupt mount); sandbox MCP wiring failed" >&2
    exit 127
  fi
fi
`

func boolArg(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// ResolveRuntime returns the container runtime executable to use.
func ResolveRuntime(name string) (string, error) {
	return resolveRuntime(name, true)
}

func resolveRuntime(name string, checkPath bool) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == DefaultRuntime {
		for _, candidate := range []string{"docker"} {
			if _, err := exec.LookPath(candidate); err == nil {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("no container runtime found on PATH; install Docker, or pass --runtime")
	}
	switch name {
	case "docker":
		if checkPath {
			if _, err := exec.LookPath(name); err != nil {
				return "", fmt.Errorf("%s not found on PATH", name)
			}
		}
		return name, nil
	case "podman":
		// v4 is Docker-only. The Runner seam and runtimeName parameter
		// are preserved so Podman can be re-added later, but resolution
		// fails fast today regardless of whether podman is on PATH.
		return "", fmt.Errorf("podman runtime is not supported yet (v4 is Docker-only); see ADR-0014")
	default:
		if !checkPath {
			return name, nil
		}
		return "", fmt.Errorf("unsupported sandbox runtime %q (want auto or docker)", name)
	}
}

func runArgs(runtimeName string, opts RunOptions) ([]string, error) {
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = "."
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox workdir: %w", err)
	}
	args := []string{"run", "--rm"}
	if opts.Interactive {
		args = append(args, "-i")
	}
	if opts.TTY {
		args = append(args, "-t")
	}
	if name := strings.TrimSpace(opts.Name); name != "" {
		args = append(args, "--name", name)
	}
	if strings.TrimSpace(opts.User) != "" {
		args = append(args, "--user", strings.TrimSpace(opts.User))
	}
	// Linux Docker does not configure host.docker.internal automatically
	// (macOS / Windows Docker Desktop do). aileron-mcp inside the
	// container reaches the daemon through AILERON_URL rewritten to
	// host.docker.internal, so without --add-host the in-container MCP
	// path fails with DNS-not-found on first daemon call. See ADR-0024
	// risks section.
	if runtimeName == "docker" && hostOS() == "linux" {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	// On a Linux host running SELinux in enforcing mode, a host bind mount keeps
	// the host's SELinux label, so the container's confined process (the image's
	// non-root agent user) is denied access to it. Docker's `:z` mount option
	// asks the runtime to relabel the mount with a shared (svirt_sandbox_file_t)
	// label so the container can read and write it. We use `:z` rather than `:Z`
	// (private) because the workspace mount is the operator's real project CWD
	// that the host must keep accessing; a private relabel would lock the host
	// out. The suffix is only emitted on Linux Docker under SELinux enforcing,
	// where it is required; elsewhere it is omitted (it would be rejected or be a
	// no-op). Named volumes are managed by the runtime and already carry a
	// container-accessible label, so they are not relabeled.
	relabel := WorkspaceRelabelActive(runtimeName)
	workspaceSpec := absWorkDir + ":" + WorkspacePath
	if relabel {
		workspaceSpec += ":z"
	}
	args = append(args,
		"--workdir", WorkspacePath,
		"--volume", workspaceSpec,
	)
	for _, volume := range opts.Volumes {
		if strings.TrimSpace(volume.Source) == "" || strings.TrimSpace(volume.Target) == "" {
			return nil, fmt.Errorf("sandbox volume source and target are required")
		}
		source := volume.Source
		if !volume.Named {
			// Host bind mount: resolve to an absolute path. A named volume must
			// be emitted verbatim — filepath.Abs would mangle the volume name
			// into a host path under the working directory.
			abs, err := filepath.Abs(volume.Source)
			if err != nil {
				return nil, fmt.Errorf("resolve sandbox volume source: %w", err)
			}
			source = abs
		}
		// Mount options go in the third `:`-delimited field as a comma-separated
		// list (e.g. `ro,z`). Emitting them as separate `:`-delimited fields
		// (`ro:z`) would make Docker read a fourth field and reject the spec, so
		// ro and the SELinux relabel must be joined here. Host bind mounts need
		// the same relabel as the workspace mount; named volumes are
		// runtime-managed and already container-accessible.
		var mountOpts []string
		if volume.ReadOnly {
			mountOpts = append(mountOpts, "ro")
		}
		if relabel && !volume.Named {
			mountOpts = append(mountOpts, "z")
		}
		spec := source + ":" + volume.Target
		if len(mountOpts) > 0 {
			spec += ":" + strings.Join(mountOpts, ",")
		}
		args = append(args, "--volume", spec)
	}
	keys := make([]string, 0, len(opts.Env))
	for k := range opts.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+opts.Env[k])
	}
	args = append(args, opts.Image)
	args = append(args, opts.Command...)
	return args, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// ProjectImageTag returns the deterministic local image tag for a project.
func ProjectImageTag(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}
	sum := sha256.Sum256([]byte(abs))
	return "aileron/sandbox-project:" + hex.EncodeToString(sum[:])[:12]
}

// buildVerb returns the leading argv tokens for a build: the plain `build` verb
// for a single-arch daemon-loading build, or `buildx build` when a multi-arch
// build (platforms set) is requested, since only buildx can drive `--platform`
// and the OCI-layout `--output`. Kept as a shared helper so the three assemblers
// switch verbs identically.
func buildVerb(platforms []string) []string {
	if len(platforms) == 0 {
		return []string{"build"}
	}
	return []string{"buildx", "build"}
}

// multiArchBuildTokens returns the buildx multi-platform + OCI-layout output flags
// a build appends when platforms is non-empty: `--platform <p1,p2,...> --output
// type=oci,dest=<dir>,tar=false`. `tar=false` makes buildx emit an unpacked OCI
// image-layout directory (the shape internal/flightplan/ociremote reads) rather
// than a tarball. It returns nil when platforms is empty so the single-arch argv
// is byte-identical to before. The three assemblers share it so they never
// diverge on the multi-arch token shape (issue #2036).
func multiArchBuildTokens(platforms []string, ociLayoutDest string) []string {
	if len(platforms) == 0 {
		return nil
	}
	return []string{
		"--platform", strings.Join(platforms, ","),
		"--output", "type=oci,dest=" + ociLayoutDest + ",tar=false",
	}
}

// multiArchLayoutDir reports the OCI-layout directory a build wrote, or "" for a
// single-arch daemon-loading build. A multi-arch build's output only lands in the
// layout when both Platforms and OCILayoutDest are set, so BuildResult.OCILayoutDir
// is populated only then.
func multiArchLayoutDir(opts BuildOptions) string {
	if len(opts.Platforms) == 0 {
		return ""
	}
	return opts.OCILayoutDest
}

// buildStepEnv returns the extra environment entries a build subprocess needs
// for this invocation, or nil when none apply (keeping that build's argv+env
// byte-identical to before):
//
//   - BUILDX_BUILDER=<name>, ONLY for a multi-arch build (Platforms non-empty)
//     with BuildxBuilder set, so buildx — whether invoked as `docker buildx
//     build` (raw tiers) or under `@devcontainers/cli` (which inherits the
//     environment and shells out to buildx) — runs on that dedicated builder
//     without `docker buildx use` repointing the operator's default. It is gated
//     on Platforms so the single-arch daemon-load build never inherits the
//     selection: that build must stay on the default `docker` driver to land the
//     image in the local daemon, which a docker-container builder cannot do
//     (issue #2054).
func buildStepEnv(opts BuildOptions) []string {
	var env []string
	if len(opts.Platforms) > 0 && strings.TrimSpace(opts.BuildxBuilder) != "" {
		env = append(env, "BUILDX_BUILDER="+opts.BuildxBuilder)
	}
	return env
}

// FreezeBuilderName is the name of the dedicated buildx builder `aileron skill
// freeze` provisions and uses for its multi-arch composed build. It is created
// with the `docker-container` driver and, deliberately, is NOT selected as the
// operator's default builder (issue #2054).
const FreezeBuilderName = "aileron-freeze"

// EnsureFreezeBuilder idempotently ensures the dedicated FreezeBuilderName buildx
// builder — backed by the `docker-container` driver — exists, creating it only
// when absent. The docker-container driver is required because the default
// `docker` driver (Docker Desktop's out-of-the-box buildx builder) cannot export
// a multi-arch OCI layout, the artifact a Flight Plan freeze pins; the driver
// also bundles the QEMU emulators, so a single-arch host can build both
// linux/amd64 and linux/arm64 without a separate binfmt install. The builder is
// created but NOT selected as the operator's default (no `docker buildx use`), so
// freeze transparently provisions its own build environment and leaves the
// operator's existing buildx setup untouched. It runs through the Runner seam so
// it is unit-testable without Docker (issue #2054).
func EnsureFreezeBuilder(ctx context.Context, runner Runner, runtimeName string) error {
	if runner == nil {
		runner = execRunner{}
	}
	// `docker buildx inspect <name>` (no --bootstrap) is a cheap existence probe:
	// it succeeds iff the named builder already exists and starts nothing.
	if err := runner.Run(ctx, runtimeName, []string{"buildx", "inspect", FreezeBuilderName}, io.Discard, io.Discard); err == nil {
		return nil
	}
	var out bytes.Buffer
	if err := runner.Run(ctx, runtimeName, []string{"buildx", "create", "--name", FreezeBuilderName, "--driver", "docker-container"}, &out, &out); err != nil {
		detail := strings.TrimSpace(out.String())
		if detail != "" {
			return fmt.Errorf("provision the dedicated `%s` buildx builder for a multi-arch freeze (%w): %s", FreezeBuilderName, err, detail)
		}
		return fmt.Errorf("provision the dedicated `%s` buildx builder for a multi-arch freeze: %w", FreezeBuilderName, err)
	}
	return nil
}

// binfmtRemediation is the guided remediation appended to every multi-arch
// preflight miss (issue #2144). A freeze produces one image that launches on
// both linux/amd64 and linux/arm64 (ADR-0027), so it emulates the non-host
// architecture through the QEMU binfmt handlers. On a stock Linux host those
// handlers are not registered, so the emulated leg cannot build. Aileron does
// not auto-run the fix: it mutates the host kernel's binfmt_misc and needs
// --privileged, so the operator runs it themselves. The message explains why the
// step is needed, gives the exact one-time command, and notes it may not persist
// across reboots on some hosts.
const binfmtRemediation = "A multi-arch freeze emulates the non-host architecture with the QEMU binfmt handlers, which are not registered on this host. Register them once by running, yourself:\n\n    docker run --privileged --rm tonistiigi/binfmt --install all\n\nThen re-run `aileron skill freeze`. Some hosts do not persist this across reboots, so you may need to re-run the command after a restart."

// CheckMultiArchBuild is the freeze-time preflight for a multi-architecture
// composed build. It runs BEFORE the base-image pull so the freeze producer
// aborts with an actionable remediation and no wasted pull rather than silently
// producing a single-arch pin (issues #2036 Q2, #2144). It shells out through the
// Runner seam so it is unit-testable without Docker.
//
// Rather than require the operator's default builder to support multi-arch OCI
// export (Docker Desktop's default `docker` driver cannot), freeze provisions its
// own dedicated `docker-container` builder and scopes the build to it. The
// preflight therefore: verifies `docker buildx version` succeeds (buildx
// present); idempotently ensures the FreezeBuilderName builder exists; bootstraps
// it with `docker buildx inspect <name> --bootstrap` (which pulls the buildkit
// image and registers the bundled QEMU emulators); and confirms it advertises
// every default freeze platform. A genuine miss returns a guided error that
// explains why the QEMU emulators are needed and gives the exact one-time
// `docker run --privileged --rm tonistiigi/binfmt --install all` command
// (binfmtRemediation). Docker-unreachable and buildkit-image-pull failures
// surface through the same error chain (issue #2054).
func CheckMultiArchBuild(ctx context.Context, runner Runner, runtimeName string) error {
	if runner == nil {
		runner = execRunner{}
	}
	if err := runner.Run(ctx, runtimeName, []string{"buildx", "version"}, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("multi-arch freeze build requires docker buildx, which is not available (%w). Install/enable Docker buildx, then register the QEMU emulators.\n\n%s", err, binfmtRemediation)
	}
	// Transparently provision the dedicated docker-container builder (a no-op when
	// it already exists) so a default Docker Desktop setup — whose active builder
	// uses the `docker` driver that cannot export a multi-arch OCI layout — works
	// without any operator setup.
	if err := EnsureFreezeBuilder(ctx, runner, runtimeName); err != nil {
		return err
	}
	var out bytes.Buffer
	if err := runner.Run(ctx, runtimeName, []string{"buildx", "inspect", FreezeBuilderName, "--bootstrap"}, &out, &out); err != nil {
		return fmt.Errorf("multi-arch freeze build could not bootstrap the dedicated `%s` buildx builder (%w).\n\n%s", FreezeBuilderName, err, binfmtRemediation)
	}
	inspect := out.String()
	var missing []string
	for _, want := range composition.MultiArchPlatforms {
		if !buildxInspectHasPlatform(inspect, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the dedicated `%s` buildx builder cannot build %s (needed for a multi-arch freeze).\n\n%s", FreezeBuilderName, strings.Join(missing, ", "), binfmtRemediation)
	}
	return nil
}

// buildxInspectHasPlatform reports whether `docker buildx inspect` output
// advertises platform. The inspect output lists supported platforms on a
// `Platforms:` line as a comma-separated set (with each emulated platform
// sometimes annotated, e.g. `linux/arm64*`), so a substring match on the exact
// platform token is sufficient and tolerant of that annotation.
func buildxInspectHasPlatform(inspect, platform string) bool {
	return strings.Contains(inspect, platform)
}

func baseBuildArgs(start, image string, platforms []string, ociLayoutDest string) ([]string, error) {
	contextDir, err := findBaseContext(start)
	if err != nil {
		return nil, err
	}
	containerfile := filepath.Join(contextDir, "Containerfile")
	args := buildVerb(platforms)
	args = append(args, "-t", image, "-f", containerfile)
	args = append(args, multiArchBuildTokens(platforms, ociLayoutDest)...)
	return append(args, contextDir), nil
}

func findBaseContext(start string) (string, error) {
	if env := strings.TrimSpace(os.Getenv("AILERON_SANDBOX_BASE_CONTEXT")); env != "" {
		return env, nil
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		dir = start
	}
	for {
		candidate := filepath.Join(dir, "images", "sandbox-base")
		if _, err := os.Stat(filepath.Join(candidate, "Containerfile")); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("sandbox-base context not found from %s; set AILERON_SANDBOX_BASE_CONTEXT", start)
		}
		dir = parent
	}
}

func devcontainerBuildArgs(workDir string, plan composition.Plan, image string, platforms []string, ociLayoutDest string) ([]string, error) {
	dockerfile, err := resolveDockerfile(workDir, plan)
	if err != nil {
		return nil, err
	}
	args := buildVerb(platforms)
	args = append(args, "-t", image, "-f", dockerfile)
	keys := make([]string, 0, len(plan.BuildArgs))
	for k := range plan.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+plan.BuildArgs[k])
	}
	args = append(args, multiArchBuildTokens(platforms, ociLayoutDest)...)
	return append(args, workDir), nil
}

// resolveDevcontainerCLI resolves the @devcontainers/cli invocation prefix for
// a Features build, honoring the toolchain seam (#1525). The prefix is the
// executable plus the leading arguments that precede the `build` subcommand. It
// returns one of:
//
//   - the host-npx prefix (the devcontainerCLI package var) when the selection
//     is the explicit ToolchainModeHostNPX opt-out;
//   - the managed escape-hatch prefix `[NodeBinary, DevcontainerCLIEntrypoint]`
//     when the managed mode (the default) is selected and both override paths are
//     set and exist on disk;
//   - the provisioned managed prefix `[<managed node>, <CLI entrypoint>]` when
//     managed is selected without the escape hatch, by invoking the Builder's
//     Provisioner.
//
// A managed selection with no escape hatch and no Provisioner is a hard error —
// the build never silently falls back to host-npx once managed is selected (the
// escape hatch is the only offline path). The host-npx opt-out never touches the
// provisioner, preserving the Docker-only/no-network unit tests for that path.
func (b Builder) resolveDevcontainerCLI(ctx context.Context, opts BuildOptions) ([]string, error) {
	switch resolveToolchainMode(opts.ToolchainMode) {
	case ToolchainModeManaged:
		// Escape hatch: both paths supplied → use them verbatim. Require both so
		// a half-configured override (node without CLI, or vice versa) fails
		// loudly rather than silently provisioning the missing half.
		node := strings.TrimSpace(opts.NodeBinary)
		cli := strings.TrimSpace(opts.DevcontainerCLIEntrypoint)
		if node != "" || cli != "" {
			if node == "" || cli == "" {
				return nil, fmt.Errorf("managed toolchain escape hatch requires both a node binary and a devcontainer CLI entrypoint; got node=%q cli=%q", node, cli)
			}
			if err := statExisting(node, "managed node binary"); err != nil {
				return nil, err
			}
			if err := statExisting(cli, "managed devcontainer CLI entrypoint"); err != nil {
				return nil, err
			}
			return []string{node, cli}, nil
		}
		if b.Provisioner == nil {
			return nil, fmt.Errorf("managed toolchain selected but no provisioner is configured and no escape-hatch paths were supplied; set the AILERON_SANDBOX_NODE/AILERON_DEVCONTAINER_CLI escape hatch or opt out with the host-npx toolchain (--toolchain=host-npx)")
		}
		managed, err := b.Provisioner.Provision(ctx)
		if err != nil {
			// Lead the managed-provision failure with the manual fallback so an
			// operator on an unsupported/offline host has an immediate next step:
			// point Aileron at a host-provided Node + CLI via the escape hatch, or
			// opt out of the managed default entirely with host-npx.
			return nil, fmt.Errorf("provision managed toolchain: %w; fall back to a host-provided Node by setting AILERON_SANDBOX_NODE/AILERON_DEVCONTAINER_CLI, or opt out with --toolchain=host-npx (AILERON_SANDBOX_TOOLCHAIN=host-npx)", err)
		}
		return []string{managed.NodeBinary, managed.CLIEntrypoint}, nil
	default:
		// Host-npx default: the documented override point stays the package var.
		if len(devcontainerCLI) == 0 {
			return nil, fmt.Errorf("devcontainer CLI invocation is not configured")
		}
		return append([]string(nil), devcontainerCLI...), nil
	}
}

// Toolchain selection environment variables. They mirror the --runtime →
// AILERON_SANDBOX_RUNTIME convention: a flag takes precedence, then the env, then
// the default (managed). The escape-hatch vars point at a pre-staged Node
// binary and devcontainer CLI entrypoint so a hermetic/offline host can run the
// managed branch without provisioning.
const (
	// ToolchainModeEnv selects the host-`npx` opt-out when set to "host-npx"
	// (managed otherwise, the default). Read by both `aileron sandbox` and launch.
	ToolchainModeEnv = "AILERON_SANDBOX_TOOLCHAIN"
	// NodeBinaryEnv is the managed-toolchain escape hatch for the Node binary.
	NodeBinaryEnv = "AILERON_SANDBOX_NODE"
	// DevcontainerCLIEnv is the managed-toolchain escape hatch for the
	// @devcontainers/cli JS entrypoint.
	DevcontainerCLIEnv = "AILERON_DEVCONTAINER_CLI"
)

// ResolveToolchainSelection computes the toolchain-related BuildOptions fields
// from a flag value and the process environment, mirroring the
// flag → env → default precedence the --runtime override uses. flagMode is the
// raw --toolchain flag value (empty when unset); the escape-hatch flag values
// (flagNode, flagCLI) likewise override the env vars. The default is managed
// (#1530); host-npx is the explicit opt-out. It returns the (mode, nodeBinary,
// cliEntrypoint) triple a caller copies into BuildOptions.
func ResolveToolchainSelection(flagMode, flagNode, flagCLI string, getenv func(string) string) (mode, nodeBinary, cliEntrypoint string) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	mode = firstNonEmpty(flagMode, getenv(ToolchainModeEnv))
	mode = resolveToolchainMode(mode)
	nodeBinary = strings.TrimSpace(firstNonEmpty(flagNode, getenv(NodeBinaryEnv)))
	cliEntrypoint = strings.TrimSpace(firstNonEmpty(flagCLI, getenv(DevcontainerCLIEnv)))
	return mode, nodeBinary, cliEntrypoint
}

// IsManagedToolchain reports whether a resolved toolchain mode selects the
// managed branch (the default). Callers use it to decide whether to wire the real
// managed provisioner into the Builder (the host-npx opt-out needs none).
func IsManagedToolchain(mode string) bool {
	return resolveToolchainMode(mode) == ToolchainModeManaged
}

// resolveToolchainMode normalizes a raw toolchain-mode selection to one of the
// recognized modes. Only the explicit host-npx token selects the host-`npx`
// branch; an empty/auto value or any unrecognized value resolves to managed (the
// default). The managed default matches the --runtime/--sandbox-proxy tri-state
// convention where an unknown value falls through to the default rather than
// failing.
func resolveToolchainMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case ToolchainModeHostNPX:
		return ToolchainModeHostNPX
	default:
		return ToolchainModeManaged
	}
}

// statExisting returns an actionable error when path does not exist, mirroring
// how devcontainerCLIBuildArgs os.Stats the devcontainer.json before building.
func statExisting(path, label string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s %s: %w", label, path, err)
	}
	return nil
}

// devcontainerCLIBuildArgs assembles the executable and arguments for a
// `devcontainer build` invocation that applies the plan's `features`. The
// invocation prefix (executable plus the arguments preceding the `build`
// subcommand) is resolved by the caller via resolveDevcontainerCLI — either the
// host-npx default (`npx --yes @devcontainers/cli@<pinned>`) or the managed
// `[<node>, <CLI entrypoint>]` form — so this function is a pure argv-assembler
// and the build path never reads global state.
//
// @devcontainers/cli reads `features` (and any base image / Dockerfile) from
// the devcontainer.json under workDir, so the workspace folder is passed rather
// than a Dockerfile. The returned name is prefix[0]; args are the remaining
// prefix tokens followed by `build --workspace-folder <workDir> --image-name
// <image>` plus deterministic (sorted) `--build-arg k=v` tokens mirroring
// devcontainerBuildArgs. The argv tail (everything after the prefix) is
// byte-identical between the host-npx and managed branches.
// materializeSynthesizedWorkspace writes a Discover-synthesized devcontainer.json
// (the no-.devcontainer local agent build, #1451) into a fresh temp directory
// laid out as <tmp>/.devcontainer/devcontainer.json, so devcontainerCLIBuildArgs
// can pass <tmp> as the `--workspace-folder` exactly as it would a real project
// directory. The returned cleanup removes the temp tree; callers defer it. The
// user's actual workDir is never touched — only the build reads this folder, and
// the run path mounts the real workDir.
func materializeSynthesizedWorkspace(content string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "aileron-sandbox-build-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create synthesized devcontainer workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	dir := filepath.Join(tmp, filepath.Dir(composition.DefaultDevcontainerPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("create synthesized devcontainer dir: %w", err)
	}
	path := filepath.Join(tmp, composition.DefaultDevcontainerPath)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write synthesized devcontainer %s: %w", path, err)
	}
	return tmp, cleanup, nil
}

func devcontainerCLIBuildArgs(prefix []string, workDir string, plan composition.Plan, image string, platforms []string, ociLayoutDest string) (string, []string, error) {
	devcontainerPath := filepath.Join(workDir, composition.DefaultDevcontainerPath)
	if _, err := os.Stat(devcontainerPath); err != nil {
		return "", nil, fmt.Errorf("devcontainer features build requires %s: %w", devcontainerPath, err)
	}
	if len(prefix) == 0 {
		return "", nil, fmt.Errorf("devcontainer CLI invocation is not configured")
	}
	name := prefix[0]
	args := append([]string(nil), prefix[1:]...)
	args = append(args, "build", "--workspace-folder", workDir, "--image-name", image)
	keys := make([]string, 0, len(plan.BuildArgs))
	for k := range plan.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+plan.BuildArgs[k])
	}
	// @devcontainers/cli forwards --platform/--output straight to buildx, so a
	// multi-arch composed build emits the same OCI-layout output the raw tiers do.
	args = append(args, multiArchBuildTokens(platforms, ociLayoutDest)...)
	return name, args, nil
}

func resolveDockerfile(workDir string, plan composition.Plan) (string, error) {
	if filepath.IsAbs(plan.DockerfilePath) {
		if _, err := os.Stat(plan.DockerfilePath); err != nil {
			return "", fmt.Errorf("devcontainer Dockerfile not found at %s: %w", plan.DockerfilePath, err)
		}
		return plan.DockerfilePath, nil
	}
	candidates := []string{
		filepath.Join(workDir, filepath.Dir(composition.DefaultDevcontainerPath), plan.DockerfilePath),
		filepath.Join(workDir, plan.DockerfilePath),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("devcontainer Dockerfile %q not found under %s", plan.DockerfilePath, workDir)
}
