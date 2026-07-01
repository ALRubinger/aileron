package main

import (
	"context"
	"errors"
	"io"
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
	// The result echoes the resolved inputs in v1.
	if res.ResolvedInputs["window_days"] != "30" {
		t.Errorf("result inputs = %v, want the launch inputs echoed", res.ResolvedInputs)
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
