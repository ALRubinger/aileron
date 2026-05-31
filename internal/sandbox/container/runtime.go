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
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/sandbox/composition"
)

const DefaultRuntime = "auto"
const WorkspacePath = "/home/agent/workspace"

var ErrNoBuildRequired = errors.New("sandbox image does not require a build")

const (
	BuildPolicyAlways = "always"
	BuildPolicyAuto   = "auto"
	BuildPolicyNever  = "never"
)

// Runner executes a container runtime command. It exists so build planning can
// be tested without Docker or Podman on the host.
type Runner interface {
	Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
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
	TTY     bool
}

// Volume describes an additional host bind mount for a sandbox container.
type Volume struct {
	Source   string
	Target   string
	ReadOnly bool
}

// RunResult reports the selected runtime after a sandbox container exits.
type RunResult struct {
	Runtime string
}

// ValidateOptions configures a launch-time sandbox image validation run.
type ValidateOptions struct {
	Runtime string
	Image   string
	WorkDir string
	Volumes []Volume
	Command []string
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
		if opts.Plan.DockerfilePath == "" {
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
		args, err := devcontainerBuildArgs(workDir, opts.Plan, result.Image)
		if err != nil {
			return BuildResult{}, err
		}
		if err := runner.Run(ctx, runtimeName, args, stdout, stderr); err != nil {
			return BuildResult{}, fmt.Errorf("%s %s: %w", runtimeName, strings.Join(args, " "), err)
		}
		result.Built = true
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

func (b Builder) imageExists(ctx context.Context, runner Runner, runtimeName, image string) bool {
	return runner.Run(ctx, runtimeName, []string{"image", "inspect", image}, io.Discard, io.Discard) == nil
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
	args, err := runArgs(opts)
	if err != nil {
		return RunResult{}, err
	}
	if err := runner.Run(ctx, runtimeName, args, stdout, stderr); err != nil {
		return RunResult{}, err
	}
	return RunResult{Runtime: runtimeName}, nil
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
		Volumes: opts.Volumes,
		Command: []string{
			"/bin/sh",
			"-c",
			validationScript,
			"aileron-validate",
			opts.Command[0],
			boolArg(requiresShimHTTPClient(opts.Volumes)),
		},
	})
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(stderr.String())
	if detail != "" {
		return fmt.Errorf("validate sandbox image %s: %s: %w", opts.Image, detail, err)
	}
	return fmt.Errorf("validate sandbox image %s: image must support /bin/sh command execution, a writable %s workspace mount, and agent command %q on PATH: %w", opts.Image, WorkspacePath, opts.Command[0], err)
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
  echo "install the agent CLI in the sandbox image or launch with --sandbox=off" >&2
  exit 127
fi
if [ "${2:-0}" = "1" ] && ! command -v wget >/dev/null 2>&1; then
  echo "generated Aileron connector shims require wget in the sandbox image" >&2
  echo "install wget in the sandbox image or launch with --sandbox=off" >&2
  exit 127
fi
if [ "${2:-0}" = "1" ]; then
  wget_help="$(wget --help 2>&1 || true)"
  for flag in "--header" "--post-data" "-T" "-t" "-O"; do
    case "$wget_help" in
      *"$flag"*) ;;
      *)
        echo "generated Aileron connector shims require wget support for $flag" >&2
        echo "install GNU wget or an equivalent wget implementation in the sandbox image" >&2
        exit 127
        ;;
    esac
  done
fi
`

func boolArg(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func requiresShimHTTPClient(volumes []Volume) bool {
	for _, volume := range volumes {
		if strings.HasPrefix(volume.Target, "/usr/local/bin/") {
			return true
		}
	}
	return false
}

// ResolveRuntime returns the container runtime executable to use.
func ResolveRuntime(name string) (string, error) {
	return resolveRuntime(name, true)
}

func resolveRuntime(name string, checkPath bool) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == DefaultRuntime {
		for _, candidate := range []string{"docker", "podman"} {
			if _, err := exec.LookPath(candidate); err == nil {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("no container runtime found on PATH; install Docker or Podman, or pass --runtime")
	}
	switch name {
	case "docker", "podman":
		if checkPath {
			if _, err := exec.LookPath(name); err != nil {
				return "", fmt.Errorf("%s not found on PATH", name)
			}
		}
		return name, nil
	default:
		if !checkPath {
			return name, nil
		}
		return "", fmt.Errorf("unsupported sandbox runtime %q (want auto, docker, or podman)", name)
	}
}

func runArgs(opts RunOptions) ([]string, error) {
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
	args = append(args,
		"--workdir", WorkspacePath,
		"--volume", absWorkDir+":"+WorkspacePath,
	)
	for _, volume := range opts.Volumes {
		if strings.TrimSpace(volume.Source) == "" || strings.TrimSpace(volume.Target) == "" {
			return nil, fmt.Errorf("sandbox volume source and target are required")
		}
		source, err := filepath.Abs(volume.Source)
		if err != nil {
			return nil, fmt.Errorf("resolve sandbox volume source: %w", err)
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
