package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// containerImageRunner is the production runtime.ImageRunner: it boots the
// verified pinned rung-1/rung-2 image and runs the frozen Flight Plan inside it
// (#1731). It mirrors the sandboxRunContainer seam in internal/launch: the boot
// is a package-level indirection (containerRunFlightPlan) so CLI tests swap in a
// fake image runner and never touch Docker, while production shells out to the
// real container runtime through the same container.Builder path the sandbox
// launch uses.
//
// The image it boots is spec.Image, taken straight from the verified lock by
// the runtime core. containerImageRunner MUST NOT re-resolve or rewrite it: the
// digest is the load-bearing assertion that the environment entered corresponds
// to the lock's signed image. It mounts the frozen store read-only and the
// out-dir writable, then re-enters the aileron binary against the bind-mounted
// unit so the in-container run reaches the same daemon action boundary over the
// existing network wiring.
type containerImageRunner struct{}

// containerRunFlightPlan boots the pinned image and runs the plan inside it. It
// is a package variable purely for symmetry with sandboxRunContainer; the
// production launch swaps the whole runtime.ImageRunner via newLaunchImageRunner
// in tests, so this indirection is the single real-Docker call site.
var containerRunFlightPlan = func(ctx context.Context, runtimeName string, stdout, stderr io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
	return sandboxcontainer.Builder{
		Runtime: runtimeName,
		Runner:  sandboxcontainer.DefaultRunner(),
		Stdout:  stdout,
		Stderr:  stderr,
	}.Run(ctx, opts)
}

func (containerImageRunner) Run(ctx context.Context, spec runtime.ImageRunSpec) (runtime.ImageRunResult, error) {
	if spec.Image == "" {
		return runtime.ImageRunResult{}, fmt.Errorf("skill launch: image runner requires a pinned image")
	}
	runtimeName, err := sandboxcontainer.ResolveRuntime("")
	if err != nil {
		return runtime.ImageRunResult{}, fmt.Errorf("skill launch: resolve container runtime: %w", err)
	}

	// Mount the frozen store read-only so the in-container run reads the exact
	// bytes verified on the host, and the out-dir writable so materialized
	// artifacts land where the host expects them.
	const (
		storeMount  = "/aileron/skills"
		outDirMount = "/aileron/out"
	)
	volumes := []sandboxcontainer.Volume{
		{Source: store.New(skillStoreDir).Root(), Target: storeMount, ReadOnly: true},
	}
	if spec.OutDir != "" {
		abs, err := filepath.Abs(spec.OutDir)
		if err != nil {
			return runtime.ImageRunResult{}, fmt.Errorf("skill launch: resolve out-dir: %w", err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return runtime.ImageRunResult{}, fmt.Errorf("skill launch: create out-dir: %w", err)
		}
		volumes = append(volumes, sandboxcontainer.Volume{Source: abs, Target: outDirMount})
	}

	// Re-enter the aileron binary against the bind-mounted unit. The in-container
	// run loads the same frozen version from the mounted store and materializes
	// into the mounted out-dir. --store-dir points the inner binary at the
	// bind-mounted store so it reads the exact verified bytes rather than the
	// empty default store inside the image. The daemon action boundary stays
	// reachable over the existing network wiring; no credentials cross this
	// boundary.
	command := []string{"aileron", "skill", "launch", spec.Name, "--store-dir", storeMount}
	if spec.Version != "" {
		command = append(command, "--version", spec.Version)
	}
	if spec.OutDir != "" {
		command = append(command, "--out-dir", outDirMount)
	}

	opts := sandboxcontainer.RunOptions{
		Runtime: runtimeName,
		Image:   spec.Image,
		Volumes: volumes,
		Command: command,
		Name:    flightPlanContainerName(spec),
	}
	if _, err := containerRunFlightPlan(ctx, runtimeName, os.Stdout, os.Stderr, opts); err != nil {
		return runtime.ImageRunResult{}, fmt.Errorf("skill launch: run pinned image %q: %w", spec.Image, err)
	}

	// v1 assembles a minimal result: the resolved inputs are echoed and
	// artifacts are collected from the out-dir mount. The content hash is left
	// to the in-container run's own audit; the CLI surfaces the launch line
	// regardless. Later sub-issues thread the full RunResult back from the
	// in-container run.
	return runtime.ImageRunResult{
		ResolvedInputs: map[string]any(spec.Inputs),
	}, nil
}

// containerToolImageRunner is the production runtime.ToolImageRunner: it
// dispatches a single rung-3 step to its pinned sibling tool image with
// mount → run → collect I/O (#1733). Unlike containerImageRunner (which boots
// one image and runs the WHOLE plan inside it), this boots the tool image for a
// single step: it writes the resolved step input into a host mount dir, mounts
// it read-only at the declared MountPath, mounts a writable collect dir at the
// declared CollectPath's parent, runs the tool, then reads back the collected
// file as the step's output. The real-Docker call goes through the same
// containerRunToolImage indirection so CLI tests swap a fake and never touch
// Docker. No credentials cross this boundary (scope note #1733).
type containerToolImageRunner struct{}

// containerToolInputFile is the filename the resolved step input is written to
// inside the mount dir. The tool reads its input from MountPath/<this>.
const containerToolInputFile = "input.json"

// containerRunToolImage boots the pinned tool image and runs it. It is a package
// variable so CLI tests swap the single real-Docker call site with a recorder,
// mirroring containerRunFlightPlan.
var containerRunToolImage = func(ctx context.Context, runtimeName string, stdout, stderr io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
	return sandboxcontainer.Builder{
		Runtime: runtimeName,
		Runner:  sandboxcontainer.DefaultRunner(),
		Stdout:  stdout,
		Stderr:  stderr,
	}.Run(ctx, opts)
}

func (containerToolImageRunner) Run(ctx context.Context, spec runtime.ToolRunSpec) (runtime.ToolRunResult, error) {
	if spec.Image == "" {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: tool image runner requires a pinned image")
	}
	runtimeName, err := sandboxcontainer.ResolveRuntime("")
	if err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: resolve container runtime: %w", err)
	}

	// A per-dispatch scratch root holds the mount input and the collect output.
	// It is removed after the run so a step's I/O never leaks between dispatches.
	scratch, err := os.MkdirTemp("", "aileron-tool-"+spec.StepID+"-")
	if err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: create tool scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)

	var volumes []sandboxcontainer.Volume

	// Mount side: serialize the resolved input to a host file and bind-mount its
	// directory read-only at the declared MountPath. The input is binding-resolved
	// data only, never a credential.
	if spec.MountPath != "" {
		mountHost := filepath.Join(scratch, "mount")
		if err := os.MkdirAll(mountHost, 0o755); err != nil {
			return runtime.ToolRunResult{}, fmt.Errorf("skill launch: create tool mount dir: %w", err)
		}
		payload, err := json.Marshal(spec.Input)
		if err != nil {
			return runtime.ToolRunResult{}, fmt.Errorf("skill launch: serialize tool input: %w", err)
		}
		if err := os.WriteFile(filepath.Join(mountHost, containerToolInputFile), payload, 0o644); err != nil {
			return runtime.ToolRunResult{}, fmt.Errorf("skill launch: write tool input: %w", err)
		}
		volumes = append(volumes, sandboxcontainer.Volume{Source: mountHost, Target: spec.MountPath, ReadOnly: true})
	}

	// Collect side: bind-mount a writable host dir at the declared CollectPath's
	// parent so the tool can write its output there, and read the file back after
	// the run.
	var collectHost, collectFile string
	if spec.CollectPath != "" {
		collectHost = filepath.Join(scratch, "collect")
		if err := os.MkdirAll(collectHost, 0o755); err != nil {
			return runtime.ToolRunResult{}, fmt.Errorf("skill launch: create tool collect dir: %w", err)
		}
		// spec.CollectPath is a container-internal (Linux) path, so parse it with
		// path, not filepath: on Windows filepath.Dir would yield backslashes
		// (\work\out) and produce an invalid, non-matching container mount target.
		collectFile = path.Base(spec.CollectPath)
		volumes = append(volumes, sandboxcontainer.Volume{Source: collectHost, Target: path.Dir(spec.CollectPath)})
	}

	opts := sandboxcontainer.RunOptions{
		Runtime: runtimeName,
		Image:   spec.Image,
		Volumes: volumes,
		Name:    toolContainerName(spec),
	}
	if _, err := containerRunToolImage(ctx, runtimeName, os.Stdout, os.Stderr, opts); err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: run pinned tool image %q: %w", spec.Image, err)
	}

	// Read back the collected output when a collect path was declared. A step
	// with no collect produces no output.
	if collectHost == "" {
		return runtime.ToolRunResult{}, nil
	}
	// The collect dir is bind-mounted writable and the tool controls its
	// contents. A compromised or malicious tool image could write the collected
	// file as a SYMLINK pointing at an arbitrary host path (e.g. /etc/passwd)
	// readable by the aileron process; a naive os.ReadFile would follow it and
	// smuggle host file contents into the step output (CWE-61), defeating the
	// isolation this per-step dispatch exists to provide. Lstat first and refuse
	// anything that is not a regular file, so only bytes the tool actually wrote
	// into the mount are collected.
	collectAbs := filepath.Join(collectHost, collectFile)
	info, err := os.Lstat(collectAbs)
	if err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: collect output from %q: %w", spec.CollectPath, err)
	}
	if !info.Mode().IsRegular() {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: collected output %q is not a regular file (refusing to follow symlinks or read special files)", spec.CollectPath)
	}
	out, err := os.ReadFile(collectAbs)
	if err != nil {
		return runtime.ToolRunResult{}, fmt.Errorf("skill launch: collect output from %q: %w", spec.CollectPath, err)
	}
	return runtime.ToolRunResult{Output: string(out)}, nil
}

// toolContainerName builds an addressable, run-unique name for a per-step tool
// dispatch, so two concurrent dispatches never collide on the container name.
func toolContainerName(spec runtime.ToolRunSpec) string {
	id := spec.StepID
	if id == "" {
		id = "step"
	}
	return "aileron-flightplan-tool-" + id + "-" + randomSuffix()
}

// flightPlanContainerName builds an addressable container name for the boot. The
// name keeps a stable, human-readable base (the skill name and version) and
// appends a short random suffix so two concurrent launches of the same frozen
// unit never collide on the container name. A stop/cleanup path reuses the value
// returned here for a given launch rather than recomputing it.
func flightPlanContainerName(spec runtime.ImageRunSpec) string {
	v := spec.Version
	if v == "" {
		v = "latest"
	}
	return "aileron-flightplan-" + spec.Name + "-" + v + "-" + randomSuffix()
}

// randomSuffix returns a short hex token for disambiguating container names. On
// the vanishingly unlikely RNG failure it degrades to a fixed token rather than
// failing the launch, since the suffix is a collision-avoidance convenience, not
// a security boundary.
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}
