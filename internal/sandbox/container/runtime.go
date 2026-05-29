// Package container builds and runs sandbox container images from composition plans.
package container

import (
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
	Command []string
	TTY     bool
}

// RunResult reports the selected runtime after a sandbox container exits.
type RunResult struct {
	Runtime string
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
	switch opts.Plan.Tier {
	case composition.TierBase:
		runtimeName, err := resolveRuntime(b.Runtime, b.Runner == nil)
		if err != nil {
			return BuildResult{}, err
		}
		result.Runtime = runtimeName
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
