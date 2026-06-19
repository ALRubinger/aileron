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

const DefaultRuntime = "auto"
const WorkspacePath = "/home/agent/workspace"
const AgentImagesDocsURL = "https://docs.withaileron.ai/development/sandbox-agent-images/"

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

// DefaultRunner returns the production Runner that shells out to the
// container runtime executable (docker). Callers outside this
// package use it to drive runtime commands (e.g. image inspection)
// through the same code path the Builder uses internally.
func DefaultRunner() Runner { return execRunner{} }

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Put the runtime child in its own process group so a terminal
	// Ctrl-C (SIGINT to the foreground process group) reaches only
	// aileron, not `docker` directly. This keeps aileron's
	// AuthSpec salvage handler the sole owner of teardown — an orderly
	// `docker stop --time` followed by Capture — instead of racing a
	// concurrent runtime-initiated kill. No-op on Windows. See ADR-0025
	// and issue #999.
	configureRuntimeChild(cmd, args, stdinIsTerminal())
	return cmd.Run()
}

// configureRuntimeChild sets the process-group attributes for a runtime
// child. It always isolates the child into its own process group so a
// terminal Ctrl-C reaches aileron rather than docker directly
// (ADR-0025, issue #999). For an interactive container `run -t` on a
// real terminal it additionally hands the child the controlling
// terminal's foreground process group — without that the runtime's
// raw-mode tcsetattr runs from a background group and fails with
// SIGTTOU/EINTR ("unable to set IO streams as raw terminal: interrupted
// system call"). The stdinTTY gate keeps non-interactive / CI
// invocations on the background-isolation path, where promoting a child
// to foreground without a controlling terminal would fail the exec.
func configureRuntimeChild(cmd *exec.Cmd, args []string, stdinTTY bool) {
	setRuntimeChildPgid(cmd)
	if stdinTTY && interactiveTTYRun(args) {
		setRuntimeChildForeground(cmd)
	}
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

// Builder builds the image required by a sandbox composition plan.
type Builder struct {
	Runtime string
	Runner  Runner
	Stdout  io.Writer
	Stderr  io.Writer
}

// BuildOptions configures a sandbox image build.
type BuildOptions struct {
	WorkDir string
	Plan    composition.Plan
	Tag     string
	Policy  string
}

// BuildResult reports the image selected or built for launch.
type BuildResult struct {
	Runtime string
	Image   string
	Built   bool
	Tier    composition.Tier
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
	TTY     bool
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
		args, err := baseBuildArgs(workDir, image)
		if err != nil {
			return BuildResult{}, err
		}
		if err := runner.Run(ctx, runtimeName, args, stdout, stderr); err != nil {
			return BuildResult{}, fmt.Errorf("%s %s: %w", runtimeName, strings.Join(args, " "), err)
		}
		result.Built = true
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
		if opts.Tag == "" {
			result.Image = ProjectImageTag(workDir)
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
			name, args, err := devcontainerCLIBuildArgs(workDir, opts.Plan, result.Image)
			if err != nil {
				return BuildResult{}, err
			}
			if err := runner.Run(ctx, name, args, stdout, stderr); err != nil {
				return BuildResult{}, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
			}
			result.Built = true
			return result, nil
		}
		args, err := devcontainerBuildArgs(workDir, opts.Plan, result.Image)
		if err != nil {
			return BuildResult{}, err
		}
		if err := runner.Run(ctx, runtimeName, args, stdout, stderr); err != nil {
			return BuildResult{}, fmt.Errorf("%s %s: %w", runtimeName, strings.Join(args, " "), err)
		}
		result.Built = true
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
		if err := runner.Run(ctx, runtimeName, pullArgs, stdout, stderr); err != nil {
			return BuildResult{}, fmt.Errorf("%s %s: %w", runtimeName, strings.Join(pullArgs, " "), err)
		}
		return result, nil
	case composition.TierBYOImage:
		result.Image = opts.Plan.Image
		return result, ErrNoBuildRequired
	default:
		return BuildResult{}, fmt.Errorf("unsupported sandbox composition tier %q", opts.Plan.Tier)
	}
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

// BakedMCPVersion reports the aileron-mcp version baked into image, read from
// the MCPVersionLabel OCI label via `<runtime> image inspect`. It returns the
// trimmed label value, or "" when the image is unlabeled, not present locally,
// or the inspect fails. Callers treat "" as "not baked" and fall back to the
// host-mount path, so an inspect error is deliberately not propagated as fatal:
// "cannot determine" degrades to "host-mount", never a broken launch.
func BakedMCPVersion(ctx context.Context, runner Runner, runtimeName, image string) string {
	if runner == nil {
		runner = execRunner{}
	}
	var stdout bytes.Buffer
	format := "{{ index .Config.Labels \"" + MCPVersionLabel + "\" }}"
	args := []string{"image", "inspect", "--format", format, image}
	if err := runner.Run(ctx, runtimeName, args, &stdout, io.Discard); err != nil {
		return ""
	}
	out := strings.TrimSpace(stdout.String())
	// An image with no labels at all renders the missing key as the Go
	// template "<no value>" sentinel rather than an empty string; treat it
	// as "not baked".
	if out == "<no value>" {
		return ""
	}
	return out
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
	_, err := builder.Run(ctx, RunOptions{
		Runtime: opts.Runtime,
		Image:   opts.Image,
		WorkDir: opts.WorkDir,
		Env:     opts.Env,
		Volumes: opts.Volumes,
		Command: []string{
			"/bin/sh",
			"-c",
			validationScript,
			"aileron-validate",
			opts.Command[0],
			boolArg(opts.RequireProxyTrust),
			boolArg(opts.RequireMCPBinary),
		},
	})
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(stderr.String())
	if detail != "" {
		detail = sandboxValidationDetail(detail)
		return fmt.Errorf("validate sandbox image %s: %s: %w", opts.Image, detail, err)
	}
	return fmt.Errorf("validate sandbox image %s: image must support /bin/sh command execution, a writable %s workspace mount, and agent command %q on PATH; see %s: %w", opts.Image, WorkspacePath, opts.Command[0], AgentImagesDocsURL, err)
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
	args := []string{"run", "--rm", "-i"}
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
	args = append(args,
		"--workdir", WorkspacePath,
		"--volume", absWorkDir+":"+WorkspacePath,
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
		spec := source + ":" + volume.Target
		if volume.ReadOnly {
			spec += ":ro"
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

func baseBuildArgs(start, image string) ([]string, error) {
	contextDir, err := findBaseContext(start)
	if err != nil {
		return nil, err
	}
	containerfile := filepath.Join(contextDir, "Containerfile")
	return []string{"build", "-t", image, "-f", containerfile, contextDir}, nil
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

func devcontainerBuildArgs(workDir string, plan composition.Plan, image string) ([]string, error) {
	dockerfile, err := resolveDockerfile(workDir, plan)
	if err != nil {
		return nil, err
	}
	args := []string{"build", "-t", image, "-f", dockerfile}
	keys := make([]string, 0, len(plan.BuildArgs))
	for k := range plan.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+plan.BuildArgs[k])
	}
	args = append(args, workDir)
	return args, nil
}

// devcontainerCLIBuildArgs assembles the executable and arguments for a
// `devcontainer build` invocation that applies the plan's `features`.
// @devcontainers/cli reads `features` (and any base image / Dockerfile) from
// the devcontainer.json under workDir, so the workspace folder is passed rather
// than a Dockerfile. The returned name is devcontainerCLI[0]; args are the
// remaining CLI prefix (e.g. `--yes @devcontainers/cli@<pinned>`) followed by
// `build --workspace-folder <workDir> --image-name <image>` plus deterministic
// (sorted) `--build-arg k=v` tokens mirroring devcontainerBuildArgs.
func devcontainerCLIBuildArgs(workDir string, plan composition.Plan, image string) (string, []string, error) {
	devcontainerPath := filepath.Join(workDir, composition.DefaultDevcontainerPath)
	if _, err := os.Stat(devcontainerPath); err != nil {
		return "", nil, fmt.Errorf("devcontainer features build requires %s: %w", devcontainerPath, err)
	}
	if len(devcontainerCLI) == 0 {
		return "", nil, fmt.Errorf("devcontainer CLI invocation is not configured")
	}
	name := devcontainerCLI[0]
	args := append([]string(nil), devcontainerCLI[1:]...)
	args = append(args, "build", "--workspace-folder", workDir, "--image-name", image)
	keys := make([]string, 0, len(plan.BuildArgs))
	for k := range plan.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+plan.BuildArgs[k])
	}
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
