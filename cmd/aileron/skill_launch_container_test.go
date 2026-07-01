package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// stubContainerBoot swaps the single real-Docker call site for a recorder so the
// container image runner's spec-construction is testable with no live runtime.
func stubContainerBoot(t *testing.T, capture *sandboxcontainer.RunOptions, err error) {
	t.Helper()
	orig := containerRunFlightPlan
	containerRunFlightPlan = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		*capture = opts
		return sandboxcontainer.RunResult{}, err
	}
	t.Cleanup(func() { containerRunFlightPlan = orig })
}

func TestContainerImageRunner_BootsExactImageWithMounts(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	outDir := t.TempDir()
	spec := runtime.ImageRunSpec{
		Image:   "registry.example.com/runner:1.4@sha256:abc",
		Name:    "weekly-metrics-digest",
		Version: "1.0.0",
		Inputs:  runtime.LaunchArgs{"window_days": "30"},
		OutDir:  outDir,
	}
	res, err := containerImageRunner{}.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The booted image must be the exact pin, never re-resolved.
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
	// The store is mounted read-only; the out-dir writable.
	var storeRO, outRW bool
	for _, v := range got.Volumes {
		if v.Source == storeDir && v.ReadOnly {
			storeRO = true
		}
		if v.Source == outDir && !v.ReadOnly {
			outRW = true
		}
	}
	if !storeRO {
		t.Errorf("frozen store must be mounted read-only, got %+v", got.Volumes)
	}
	if !outRW {
		t.Errorf("out-dir must be mounted writable, got %+v", got.Volumes)
	}
	// The in-container command re-enters aileron against the mounted unit.
	joined := strings.Join(got.Command, " ")
	if !strings.Contains(joined, "skill launch weekly-metrics-digest") {
		t.Errorf("command = %v, want an in-container skill launch of the unit", got.Command)
	}
	if !strings.Contains(joined, "--version 1.0.0") {
		t.Errorf("command must pin the version: %v", got.Command)
	}
	// The inner binary must be pointed at the bind-mounted store, not the empty
	// default store inside the image.
	if !strings.Contains(joined, "--store-dir /aileron/skills") {
		t.Errorf("command must point the inner binary at the mounted store: %v", got.Command)
	}
	// The container name carries a run-unique suffix so concurrent launches of
	// the same unit never collide.
	if got.Name == flightPlanContainerName(spec) {
		t.Error("container name must include a run-unique suffix, not a fully deterministic value")
	}
	if !strings.HasPrefix(got.Name, "aileron-flightplan-weekly-metrics-digest-1.0.0-") {
		t.Errorf("container name = %q, want the stable base prefix plus a suffix", got.Name)
	}
	// The result echoes the resolved inputs in v1.
	if res.ResolvedInputs["window_days"] != "30" {
		t.Errorf("result inputs = %v, want the launch inputs echoed", res.ResolvedInputs)
	}
}

// stubContainerToolBoot swaps the single real-Docker call site for the tool
// image runner with a recorder that also simulates the tool writing its output
// to the collect mount, so the runner's spec-construction and collect-readback
// are testable with no live runtime. collectContent is written to the collect
// volume's host source under the CollectPath basename.
func stubContainerToolBoot(t *testing.T, capture *sandboxcontainer.RunOptions, collectBasename, collectContent string, err error) {
	t.Helper()
	orig := containerRunToolImage
	containerRunToolImage = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		*capture = opts
		if err != nil {
			return sandboxcontainer.RunResult{}, err
		}
		// Simulate the tool writing its output into the collect mount so the
		// runner's readback path is exercised end to end.
		if collectBasename != "" {
			for _, v := range opts.Volumes {
				if !v.ReadOnly {
					_ = os.WriteFile(filepath.Join(v.Source, collectBasename), []byte(collectContent), 0o644)
				}
			}
		}
		return sandboxcontainer.RunResult{}, nil
	}
	t.Cleanup(func() { containerRunToolImage = orig })
}

// TestContainerToolImageRunner_MountsRunsCollects proves the production tool
// runner boots the exact pinned image, mounts the resolved input read-only at
// the declared mount path, mounts the collect dir writable, and reads back the
// collected output as the step's result. It is the #1733 CLI-wiring acceptance:
// the pinned image and mount/collect wiring are asserted without touching Docker.
func TestContainerToolImageRunner_MountsRunsCollects(t *testing.T) {
	var got sandboxcontainer.RunOptions
	stubContainerToolBoot(t, &got, "out", "COLLECTED-BYTES", nil)

	spec := runtime.ToolRunSpec{
		Image:       "registry.example.com/tool-a:1@sha256:abc",
		StepID:      "extract",
		MountPath:   "/work/in",
		Input:       map[string]any{"payload": "hello"},
		CollectPath: "/work/out/out",
	}
	res, err := containerToolImageRunner{}.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The booted image is the exact pin, never re-resolved.
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
	// The mount is read-only at the declared mount path; the collect is writable
	// at the collect path's parent.
	var mountRO, collectRW bool
	for _, v := range got.Volumes {
		if v.Target == "/work/in" && v.ReadOnly {
			mountRO = true
		}
		if v.Target == "/work/out" && !v.ReadOnly {
			collectRW = true
		}
	}
	if !mountRO {
		t.Errorf("input must be mounted read-only at the declared mount path, got %+v", got.Volumes)
	}
	if !collectRW {
		t.Errorf("collect dir must be mounted writable at the collect path parent, got %+v", got.Volumes)
	}
	// The collected bytes become the step's output.
	if res.Output != "COLLECTED-BYTES" {
		t.Errorf("collected output = %v, want the tool's written bytes", res.Output)
	}
	// The container name carries a run-unique suffix.
	if !strings.HasPrefix(got.Name, "aileron-flightplan-tool-extract-") {
		t.Errorf("container name = %q, want the stable tool prefix plus a suffix", got.Name)
	}
}

// TestContainerToolImageRunner_NoMountNoCollect proves a minimal rung-3 step
// (image only, no mount/collect) boots the image and returns a nil output with
// no mount or collect volume.
func TestContainerToolImageRunner_NoMountNoCollect(t *testing.T) {
	var got sandboxcontainer.RunOptions
	stubContainerToolBoot(t, &got, "", "", nil)

	res, err := containerToolImageRunner{}.Run(context.Background(), runtime.ToolRunSpec{
		Image:  "registry.example.com/tool-a:1@sha256:abc",
		StepID: "s1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.Volumes) != 0 {
		t.Errorf("a mount/collect-free dispatch must mount nothing, got %+v", got.Volumes)
	}
	if res.Output != nil {
		t.Errorf("a collect-free dispatch must produce no output, got %v", res.Output)
	}
}

// TestContainerToolImageRunner_EmptyImageErrors proves an empty pin errors
// before any boot.
func TestContainerToolImageRunner_EmptyImageErrors(t *testing.T) {
	if _, err := (containerToolImageRunner{}).Run(context.Background(), runtime.ToolRunSpec{}); err == nil {
		t.Fatal("an empty tool image must error before any boot")
	}
}

// TestContainerToolImageRunner_BootFailureSurfaces proves a boot failure
// surfaces rather than being swallowed.
func TestContainerToolImageRunner_BootFailureSurfaces(t *testing.T) {
	var got sandboxcontainer.RunOptions
	stubContainerToolBoot(t, &got, "", "", errors.New("tool boot exploded"))

	_, err := containerToolImageRunner{}.Run(context.Background(), runtime.ToolRunSpec{
		Image:  "img@sha256:abc",
		StepID: "s1",
	})
	if err == nil || !strings.Contains(err.Error(), "tool boot exploded") {
		t.Fatalf("boot failure must surface, got %v", err)
	}
}

// TestContainerToolImageRunner_MissingCollectedFileErrors proves a tool that
// declares a collect path but writes no file surfaces a readback error rather
// than silently producing an empty output.
func TestContainerToolImageRunner_MissingCollectedFileErrors(t *testing.T) {
	var got sandboxcontainer.RunOptions
	// collectBasename empty => the stub writes nothing, so the readback fails.
	stubContainerToolBoot(t, &got, "", "", nil)

	_, err := containerToolImageRunner{}.Run(context.Background(), runtime.ToolRunSpec{
		Image:       "img@sha256:abc",
		StepID:      "s1",
		CollectPath: "/work/out/out",
	})
	if err == nil || !strings.Contains(err.Error(), "collect output") {
		t.Fatalf("a missing collected file must error, got %v", err)
	}
}

func TestContainerImageRunner_EmptyImageErrors(t *testing.T) {
	_, err := containerImageRunner{}.Run(context.Background(), runtime.ImageRunSpec{})
	if err == nil {
		t.Fatal("an empty image must error before any boot")
	}
}

func TestContainerImageRunner_BootFailureSurfaces(t *testing.T) {
	origStore := skillStoreDir
	skillStoreDir = t.TempDir()
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, errors.New("boot exploded"))

	_, err := containerImageRunner{}.Run(context.Background(), runtime.ImageRunSpec{
		Image: "img@sha256:abc",
		Name:  "unit",
	})
	if err == nil || !strings.Contains(err.Error(), "boot exploded") {
		t.Fatalf("boot failure must surface, got %v", err)
	}
}
