package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
)

func TestRunInImage_BootsExactPinnedImage(t *testing.T) {
	// A plan that pins an environment image boots that exact ref@digest through the
	// ImageRunner. The recorded image is the load-bearing assertion: it must
	// equal ref@sha256:<digest-from-lock>, never a re-resolved tag.
	digest := "sha256:" + strings.Repeat("a", 64)
	lp := LoadedPlan{
		ContentHash:    "sha256:content",
		ResolvedImages: []freeze.ImagePin{{Ref: "registry.example.com/runner:1.4", Digest: digest}},
	}
	fake := &fakeImageRunner{result: ImageRunResult{
		ContentHash:    "sha256:content",
		ResolvedInputs: map[string]any{"k": "v"},
		StepOutputs:    map[string]map[string]any{"s1": {"out": 1}},
		AuditIDs:       []string{"audit-1"},
	}}
	res, err := runInImage(context.Background(), lp, Options{
		Name:        "weekly-metrics-digest",
		Version:     "1.0.0",
		Inputs:      LaunchArgs{"since": "2026-01-01"},
		OutDir:      "/tmp/out",
		ImageRunner: fake,
	})
	if err != nil {
		t.Fatalf("runInImage: %v", err)
	}
	if !fake.called {
		t.Fatal("the image runner was never called")
	}
	wantImage := "registry.example.com/runner:1.4@" + digest
	if fake.spec.Image != wantImage {
		t.Errorf("booted image = %q, want %q", fake.spec.Image, wantImage)
	}
	if fake.spec.Name != "weekly-metrics-digest" || fake.spec.Version != "1.0.0" {
		t.Errorf("spec selector = %q/%q, want the launch selector", fake.spec.Name, fake.spec.Version)
	}
	if fake.spec.OutDir != "/tmp/out" {
		t.Errorf("spec OutDir = %q, want the launch out-dir", fake.spec.OutDir)
	}
	if fake.spec.Inputs["since"] != "2026-01-01" {
		t.Errorf("spec Inputs = %v, want the launch inputs", fake.spec.Inputs)
	}
	// The runner's result maps straight onto the public RunResult shape.
	if res.ContentHash != "sha256:content" || res.ResolvedInputs["k"] != "v" ||
		res.StepOutputs["s1"]["out"] != 1 || len(res.AuditIDs) != 1 {
		t.Errorf("RunResult not mapped from ImageRunResult: %+v", res)
	}
}

func TestRunInImage_BootsComposedLocalTag(t *testing.T) {
	// A composed-tools pin carries a descriptive, unbootable Ref and an image-Id
	// Digest, plus the bootable local-daemon tag in LocalTag. The runtime must
	// boot the LocalTag verbatim (the daemon-resolvable identity), never the
	// `ref@digest` join, which names no image the daemon can resolve. This is the
	// #1856 regression: before the fix imageRef produced the unbootable join and
	// the boot failed closed.
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	descriptiveRef := "ghcr.io/alrubinger/aileron-sandbox-base:edge@sha256:" +
		strings.Repeat("a", 64) + "+tools(gh@2.x)"
	imageID := "sha256:" + strings.Repeat("b", 64)
	lp := LoadedPlan{
		ContentHash: "sha256:content",
		ResolvedImages: []freeze.ImagePin{
			{Ref: descriptiveRef, Digest: imageID, LocalTag: localTag},
		},
	}
	fake := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	if _, err := runInImage(context.Background(), lp, Options{
		Name:        "tools-plan",
		Version:     "1.0.0",
		ImageRunner: fake,
	}); err != nil {
		t.Fatalf("runInImage: %v", err)
	}
	if !fake.called {
		t.Fatal("the image runner was never called")
	}
	if fake.spec.Image != localTag {
		t.Errorf("booted image = %q, want the bootable local tag %q", fake.spec.Image, localTag)
	}
	if strings.Contains(fake.spec.Image, "@"+imageID) || strings.Contains(fake.spec.Image, "+tools(") {
		t.Errorf("booted the unbootable ref@digest join %q, want the local tag", fake.spec.Image)
	}
}

func TestRunInImage_PinnedButNoRunnerErrors(t *testing.T) {
	// A plan that declares and pins an environment image but supplies no ImageRunner is
	// an explicit error, never a silent in-process fallback: ignoring the pin
	// would enter an environment the attestation never certified.
	lp := LoadedPlan{
		ResolvedImages: []freeze.ImagePin{{Ref: "registry.example.com/runner:1.4", Digest: "sha256:" + strings.Repeat("a", 64)}},
	}
	res, err := runInImage(context.Background(), lp, Options{ImageRunner: nil})
	if err == nil {
		t.Fatal("a pinned image with no runner must error")
	}
	if !strings.Contains(err.Error(), "no image runner is configured") {
		t.Errorf("error = %q, want a no-runner-configured message", err.Error())
	}
	if res.ContentHash != "" || len(res.Artifacts) != 0 {
		t.Errorf("a refused boot must produce a zero RunResult, got %+v", res)
	}
}

func TestRunInImage_PropagatesRunnerError(t *testing.T) {
	// A boot failure surfaces from the runner unchanged and yields no result.
	lp := LoadedPlan{
		ResolvedImages: []freeze.ImagePin{{Ref: "r", Digest: "sha256:" + strings.Repeat("a", 64)}},
	}
	boom := errors.New("boot failed")
	res, err := runInImage(context.Background(), lp, Options{ImageRunner: &fakeImageRunner{err: boom}})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the runner error", err)
	}
	if res.ContentHash != "" {
		t.Errorf("a failed boot must produce a zero RunResult, got %+v", res)
	}
}

func TestRunInImage_RejectsMultiplePins(t *testing.T) {
	// The attestation certifies exactly one image environment, so a plan that
	// somehow carries more than one resolved pin must error rather than silently
	// boot pins[0] and ignore the rest. Guards the single-pin invariant against a
	// future multi-pin environment.
	lp := LoadedPlan{
		ResolvedImages: []freeze.ImagePin{
			{Ref: "registry.example.com/runner:1.4", Digest: "sha256:" + strings.Repeat("a", 64)},
			{Ref: "registry.example.com/runner:2.0", Digest: "sha256:" + strings.Repeat("b", 64)},
		},
	}
	res, err := runInImage(context.Background(), lp, Options{ImageRunner: &fakeImageRunner{}})
	if err == nil {
		t.Fatal("a plan with two resolved pins must error")
	}
	if !strings.Contains(err.Error(), "expected exactly one resolved image pin") {
		t.Errorf("error = %q, want a single-pin invariant message", err.Error())
	}
	if !strings.Contains(err.Error(), "got 2") {
		t.Errorf("error = %q, want the observed pin count", err.Error())
	}
	if res.ContentHash != "" || len(res.Artifacts) != 0 {
		t.Errorf("a rejected boot must produce a zero RunResult, got %+v", res)
	}
}

// TestHasWholePlanImage proves the boot routing: any non-empty pin set is a
// whole-plan boot (every pin is a whole-plan pin since #1839; tool steps run
// as subprocesses inside that one boot, #1829), and an empty set stays on the
// in-process path.
func TestHasWholePlanImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		pins []freeze.ImagePin
		want bool
	}{
		{"empty", nil, false},
		{"single whole-plan pin", []freeze.ImagePin{{Ref: "r", Digest: digest}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasWholePlanImage(tc.pins); got != tc.want {
				t.Errorf("hasWholePlanImage = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestImageRef_JoinsRefAndDigest(t *testing.T) {
	got := imageRef("registry.example.com/runner:1.4", "sha256:abc", "")
	if got != "registry.example.com/runner:1.4@sha256:abc" {
		t.Errorf("imageRef = %q", got)
	}
	// An empty digest degrades to the bare ref rather than a dangling `@`.
	if got := imageRef("r", "", ""); got != "r" {
		t.Errorf("imageRef with empty digest = %q, want the bare ref", got)
	}
	// A non-empty local tag wins over the ref@digest join: it is the
	// daemon-resolvable identity of a locally-built composed image.
	if got := imageRef("registry.example.com/runner:1.4", "sha256:abc", "aileron/sandbox-tools:x"); got != "aileron/sandbox-tools:x" {
		t.Errorf("imageRef with local tag = %q, want the local tag", got)
	}
}
