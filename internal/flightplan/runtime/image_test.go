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
			{Ref: descriptiveRef, ConfigDigests: hostConfigDigests(imageID), LocalTag: localTag},
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

// fakeRegistryImageResolver records the origin+pin it was handed and returns a
// canned bootRef or error, so the registry-origin boot path is testable with no
// live registry.
type fakeRegistryImageResolver struct {
	bootRef   string
	err       error
	called    bool
	gotOrigin RegistryImageOrigin
	gotPin    freeze.ImagePin
}

func (f *fakeRegistryImageResolver) Resolve(_ context.Context, origin RegistryImageOrigin, pin freeze.ImagePin) (string, error) {
	f.called = true
	f.gotOrigin = origin
	f.gotPin = pin
	return f.bootRef, f.err
}

// registryOriginPlan builds a composed-tools LoadedPlan carrying a registry
// origin, the shape LoadVerified produces for an OCI-installed plan.
func registryOriginPlan() LoadedPlan {
	return LoadedPlan{
		ContentHash: "sha256:content",
		ResolvedImages: []freeze.ImagePin{{
			Ref:           "aileron/sandbox-tools+tools(gh)",
			ConfigDigests: hostConfigDigests("sha256:" + strings.Repeat("c", 64)),
			LocalTag:      "aileron/sandbox-tools:abc123",
		}},
		ImageOrigin: RegistryImageOrigin{
			Registry:   "ghcr.io/acme/plan",
			VersionTag: "v1abc",
			Present:    true,
		},
	}
}

func TestRunInImage_RegistryOriginBootsResolvedRef(t *testing.T) {
	// A registry-origin plan pulls+verifies the published image through the
	// RegistryImageResolver seam and boots the returned reference verbatim. The
	// local-tag imageRef/#1863 guard path is NOT taken: the resolver already
	// verified the pulled bytes against the signed pin.
	lp := registryOriginPlan()
	bootRef := "ghcr.io/acme/plan:v1abc-image"
	resolver := &fakeRegistryImageResolver{bootRef: bootRef}
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	// A local resolver is also wired to prove it is NOT consulted on this path.
	localGuard := &fakeImageDigestResolver{digest: "sha256:" + strings.Repeat("z", 64)}

	res, err := runInImage(context.Background(), lp, Options{
		Name:                  "tools-plan",
		Version:               "1.0.0",
		ImageRunner:           runner,
		RegistryImageResolver: resolver,
		ImageDigestResolver:   localGuard,
	})
	if err != nil {
		t.Fatalf("runInImage: %v", err)
	}
	if !resolver.called {
		t.Fatal("the registry resolver was never consulted for a registry-origin plan")
	}
	if resolver.gotOrigin.Registry != "ghcr.io/acme/plan" || resolver.gotOrigin.VersionTag != "v1abc" {
		t.Errorf("resolver origin = %+v, want the recorded install origin", resolver.gotOrigin)
	}
	if resolver.gotPin.ConfigDigests[0].Digest != lp.ResolvedImages[0].ConfigDigests[0].Digest {
		t.Errorf("resolver pin digest = %q, want the signed lock pin", resolver.gotPin.ConfigDigests[0].Digest)
	}
	if localGuard.called {
		t.Error("the local-tag #1863 guard must NOT run on the registry-origin path")
	}
	if !runner.called {
		t.Fatal("a verified registry pull must boot: the image runner was never called")
	}
	if runner.spec.Image != bootRef {
		t.Errorf("booted image = %q, want the resolver's returned ref %q", runner.spec.Image, bootRef)
	}
	if res.ContentHash != "sha256:content" {
		t.Errorf("RunResult not mapped from the boot, got %+v", res)
	}
}

func TestRunInImage_RegistryOriginResolverErrorRefusesBoot(t *testing.T) {
	// A pull/verify failure from the resolver refuses the boot with a precise
	// message and never calls the runner.
	lp := registryOriginPlan()
	boom := errors.New("pull: pulled image does not match the signed lock digest")
	resolver := &fakeRegistryImageResolver{err: boom}
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}

	res, err := runInImage(context.Background(), lp, Options{
		Name:                  "tools-plan",
		Version:               "1.0.0",
		ImageRunner:           runner,
		RegistryImageResolver: resolver,
	})
	if err == nil {
		t.Fatal("a resolver pull/verify failure must refuse the boot")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the wrapped resolver error", err)
	}
	if !strings.Contains(err.Error(), "ghcr.io/acme/plan") {
		t.Errorf("error = %q, want the registry named", err.Error())
	}
	if runner.called {
		t.Fatal("a refused registry pull must NOT boot the image runner")
	}
	if res.ContentHash != "" {
		t.Errorf("a refused boot must produce a zero RunResult, got %+v", res)
	}
}

func TestRunInImage_RegistryOriginNilResolverFailsClosed(t *testing.T) {
	// A registry-origin plan with no RegistryImageResolver is a fail-closed
	// error: there is no local tag to boot instead, so the runtime must refuse
	// rather than fall through to the local-tag path.
	lp := registryOriginPlan()
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}

	res, err := runInImage(context.Background(), lp, Options{
		Name:        "tools-plan",
		Version:     "1.0.0",
		ImageRunner: runner,
		// RegistryImageResolver intentionally nil.
	})
	if err == nil {
		t.Fatal("a registry-origin plan with no registry resolver must fail closed")
	}
	if !strings.Contains(err.Error(), "no registry image resolver is configured") {
		t.Errorf("error = %q, want a no-resolver-configured message", err.Error())
	}
	if runner.called {
		t.Fatal("a fail-closed registry-origin boot must NOT call the image runner")
	}
	if res.ContentHash != "" {
		t.Errorf("a refused boot must produce a zero RunResult, got %+v", res)
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
