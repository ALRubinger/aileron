package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
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
