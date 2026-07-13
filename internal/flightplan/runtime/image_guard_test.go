package runtime

import (
	"context"
	"errors"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
)

// hostConfigDigests builds a one-entry per-arch config-digest set keyed by the
// host platform (linux/GOARCH), so a composed pin's HostConfigDigest selects it
// on whatever arch the test runs on. The package is named runtime, so the
// stdlib runtime is aliased goruntime.
func hostConfigDigests(digest string) []freeze.PlatformDigest {
	return []freeze.PlatformDigest{{OS: "linux", Arch: goruntime.GOARCH, Digest: digest}}
}

// fakeImageDigestResolver is the shared test double for the
// LocalImageDigestResolver seam: it records the image it was asked to resolve
// and returns a canned digest or error, so the boot-time Id-vs-Digest guard is
// testable with no live daemon.
type fakeImageDigestResolver struct {
	digest   string
	err      error
	called   bool
	gotImage string
}

func (f *fakeImageDigestResolver) Resolve(_ context.Context, image string) (string, error) {
	f.called = true
	f.gotImage = image
	return f.digest, f.err
}

func TestImageDigestResolver_FakeSatisfiesInterface(t *testing.T) {
	// Compile-time and behavioral proof that the SPI is satisfiable by a fake:
	// the guard path never needs a live daemon to test.
	var r LocalImageDigestResolver = &fakeImageDigestResolver{digest: "sha256:x"}
	got, err := r.Resolve(context.Background(), "aileron/sandbox-tools:x")
	if err != nil {
		t.Fatalf("fake LocalImageDigestResolver: %v", err)
	}
	if got != "sha256:x" {
		t.Errorf("resolved digest = %q, want the canned value", got)
	}
}

// composedPin builds a composed-tools LoadedPlan: a descriptive/unbootable Ref,
// an image-Id Digest, and a bootable LocalTag, the shape freeze produces for an
// environment-with-tools unit (#1856).
func composedPin(localTag, imageID string) LoadedPlan {
	descriptiveRef := "ghcr.io/alrubinger/aileron-sandbox-base:edge@sha256:" +
		strings.Repeat("a", 64) + "+tools(gh@2.x)"
	return LoadedPlan{
		ContentHash: "sha256:content",
		ResolvedImages: []freeze.ImagePin{
			{Ref: descriptiveRef, ConfigDigests: hostConfigDigests(imageID), LocalTag: localTag},
		},
	}
}

// TestRunInImage_LocalBootComparesAgainstLocalHostConfigDigest is the #2138
// regression: freeze builds the composed image twice (the multi-arch OCI layout,
// whose digests are ConfigDigests, and a separate host-arch daemon load), and the
// two non-reproducible builds can legitimately produce DIFFERENT config content
// digests. The local no-publish boot guard must compare the daemon-resolved digest
// against the recorded daemon digest (LocalHostConfigDigest), NOT the OCI-layout
// ConfigDigests[host]. Before the fix the guard compared against ConfigDigests[host]
// and refused to boot the operator's own freshly-frozen image (observed != want)
// even though nothing was rebuilt. After the fix a daemon image resolving to
// LocalHostConfigDigest boots, while a resolver returning the OCI-layout digest
// (the value the daemon does NOT carry) is correctly refused.
func TestRunInImage_LocalBootComparesAgainstLocalHostConfigDigest(t *testing.T) {
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	ociLayoutDigest := "sha256:" + strings.Repeat("a", 64) // ConfigDigests[host], the published identity
	daemonDigest := "sha256:" + strings.Repeat("d", 64)    // LocalHostConfigDigest, what the daemon actually carries
	if ociLayoutDigest == daemonDigest {
		t.Fatal("precondition: the two digests must differ to exercise the two-source-of-truth path")
	}
	pin := freeze.ImagePin{
		Ref:                   "aileron/sandbox-tools+tools(gh)",
		ConfigDigests:         hostConfigDigests(ociLayoutDigest),
		LocalTag:              localTag,
		LocalHostConfigDigest: daemonDigest,
	}
	lp := LoadedPlan{ContentHash: "sha256:content", ResolvedImages: []freeze.ImagePin{pin}}

	// The daemon resolves the tag to the daemon digest (what freeze actually
	// loaded). The guard must accept this — it compares against LocalHostConfigDigest.
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	resolver := &fakeImageDigestResolver{digest: daemonDigest}
	if _, err := runInImage(context.Background(), lp, Options{
		Name:                "tools-plan",
		Version:             "1.0.0",
		ImageRunner:         runner,
		ImageDigestResolver: resolver,
	}); err != nil {
		t.Fatalf("a daemon image resolving to LocalHostConfigDigest must boot (the #2138 fix): %v", err)
	}
	if !runner.called {
		t.Fatal("the guard must boot when the daemon digest matches LocalHostConfigDigest")
	}

	// A resolver returning the OCI-layout digest (the value the daemon does NOT
	// carry) must be refused: the guard is still fail-closed, just against the
	// correct local target.
	runner2 := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	resolver2 := &fakeImageDigestResolver{digest: ociLayoutDigest}
	_, err := runInImage(context.Background(), lp, Options{
		Name:                "tools-plan",
		Version:             "1.0.0",
		ImageRunner:         runner2,
		ImageDigestResolver: resolver2,
	})
	if err == nil {
		t.Fatal("a daemon digest that matches only the OCI-layout ConfigDigests (not LocalHostConfigDigest) must fail closed")
	}
	if runner2.called {
		t.Fatal("a mismatch against LocalHostConfigDigest must NOT boot the runner")
	}
	// The mismatch error names the daemon target (LocalHostConfigDigest), proving
	// the compare used the local field, not ConfigDigests[host].
	if !strings.Contains(err.Error(), daemonDigest) {
		t.Errorf("mismatch error %q must name the daemon target %q", err.Error(), daemonDigest)
	}
}

// TestRunInImage_LocalBootFallsBackToConfigDigestsWhenLocalFieldAbsent proves the
// backward-compatible fallback: a pin frozen before the LocalHostConfigDigest field
// existed (empty field) still compares against the host ConfigDigests entry, so an
// older lock keeps its pre-#2138 behavior.
func TestRunInImage_LocalBootFallsBackToConfigDigestsWhenLocalFieldAbsent(t *testing.T) {
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	configDigest := "sha256:" + strings.Repeat("b", 64)
	pin := freeze.ImagePin{
		Ref:           "aileron/sandbox-tools+tools(gh)",
		ConfigDigests: hostConfigDigests(configDigest),
		LocalTag:      localTag,
		// LocalHostConfigDigest intentionally empty (older freeze).
	}
	lp := LoadedPlan{ContentHash: "sha256:content", ResolvedImages: []freeze.ImagePin{pin}}
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	resolver := &fakeImageDigestResolver{digest: configDigest}
	if _, err := runInImage(context.Background(), lp, Options{
		Name:                "tools-plan",
		Version:             "1.0.0",
		ImageRunner:         runner,
		ImageDigestResolver: resolver,
	}); err != nil {
		t.Fatalf("an older lock with no LocalHostConfigDigest must compare against ConfigDigests[host]: %v", err)
	}
	if !runner.called {
		t.Fatal("the fallback compare against ConfigDigests[host] must boot on a match")
	}
}

func TestRunInImage_GuardBootsOnDigestMatch(t *testing.T) {
	// A composed pin whose LocalTag resolves in the daemon to the attested
	// Digest boots: the guard consults the resolver, the digests match, and the
	// ImageRunner is called with the LocalTag verbatim.
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	imageID := "sha256:" + strings.Repeat("b", 64)
	lp := composedPin(localTag, imageID)
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	resolver := &fakeImageDigestResolver{digest: imageID}
	if _, err := runInImage(context.Background(), lp, Options{
		Name:                "tools-plan",
		Version:             "1.0.0",
		ImageRunner:         runner,
		ImageDigestResolver: resolver,
	}); err != nil {
		t.Fatalf("runInImage: %v", err)
	}
	if !resolver.called {
		t.Fatal("the guard never consulted the resolver on a composed pin")
	}
	if resolver.gotImage != localTag {
		t.Errorf("resolver asked about %q, want the local tag %q", resolver.gotImage, localTag)
	}
	if !runner.called {
		t.Fatal("a matching guard must boot: the image runner was never called")
	}
	if runner.spec.Image != localTag {
		t.Errorf("booted image = %q, want the local tag %q", runner.spec.Image, localTag)
	}
}

func TestRunInImage_GuardFailsClosedOnDigestMismatch(t *testing.T) {
	// A composed pin whose LocalTag resolves to a DIFFERENT digest than the
	// attested one fails closed: the boot returns an error naming both digests
	// and the tag, the ImageRunner is NEVER called, and the result is zero.
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	attested := "sha256:" + strings.Repeat("b", 64)
	observed := "sha256:" + strings.Repeat("c", 64)
	lp := composedPin(localTag, attested)
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	resolver := &fakeImageDigestResolver{digest: observed}
	res, err := runInImage(context.Background(), lp, Options{
		Name:                "tools-plan",
		Version:             "1.0.0",
		ImageRunner:         runner,
		ImageDigestResolver: resolver,
	})
	if err == nil {
		t.Fatal("a composed pin whose local tag resolves to a different digest must fail closed")
	}
	if runner.called {
		t.Fatal("a mismatched guard must NOT boot the image runner")
	}
	// The error names the mismatch: both the observed and attested digests and
	// the refused local tag, so an operator can see exactly what diverged.
	msg := err.Error()
	for _, want := range []string{localTag, attested, observed} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must name %q", msg, want)
		}
	}
	if res.ContentHash != "" || len(res.Artifacts) != 0 {
		t.Errorf("a refused boot must produce a zero RunResult, got %+v", res)
	}
}

func TestRunInImage_GuardFailsClosedOnResolveError(t *testing.T) {
	// A composed pin whose LocalTag cannot be resolved in the daemon (image gone,
	// inspect failure) fails closed: the attested image is not present, so the
	// boot refuses rather than boot an unverified tag. The ImageRunner is never
	// called and the resolve error is wrapped into the boot error.
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	imageID := "sha256:" + strings.Repeat("b", 64)
	lp := composedPin(localTag, imageID)
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	boom := errors.New("no such image")
	resolver := &fakeImageDigestResolver{err: boom}
	res, err := runInImage(context.Background(), lp, Options{
		Name:                "tools-plan",
		Version:             "1.0.0",
		ImageRunner:         runner,
		ImageDigestResolver: resolver,
	})
	if err == nil {
		t.Fatal("a composed pin whose local tag cannot be resolved must fail closed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the wrapped resolve error", err)
	}
	if runner.called {
		t.Fatal("a resolve error must NOT boot the image runner")
	}
	if res.ContentHash != "" {
		t.Errorf("a refused boot must produce a zero RunResult, got %+v", res)
	}
}

func TestRunInImage_GuardFailsClosedWhenHostArchAbsent(t *testing.T) {
	// A composed pin whose per-arch config-digest set carries no entry for the
	// host platform fails closed with the "not frozen for <host platform>"
	// message: the artifact was built for other arches only, so this host must not
	// boot an unattested image. The resolver is never consulted and the runner is
	// never called. The local no-publish boot path says "frozen" (not "published")
	// because this is the author's own freeze, with no publish involved (#2138).
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	lp := LoadedPlan{
		ContentHash: "sha256:content",
		ResolvedImages: []freeze.ImagePin{{
			Ref: "aileron/sandbox-tools+tools(gh)",
			// A bogus arch that never equals the host GOARCH, so HostConfigDigest misses.
			ConfigDigests: []freeze.PlatformDigest{{OS: "linux", Arch: "no-such-arch", Digest: "sha256:" + strings.Repeat("b", 64)}},
			LocalTag:      localTag,
		}},
	}
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	resolver := &fakeImageDigestResolver{digest: "sha256:" + strings.Repeat("b", 64)}
	_, err := runInImage(context.Background(), lp, Options{
		Name:                "tools-plan",
		Version:             "1.0.0",
		ImageRunner:         runner,
		ImageDigestResolver: resolver,
	})
	if err == nil {
		t.Fatal("a composed pin lacking the host arch must fail closed")
	}
	if !strings.Contains(err.Error(), "not frozen for") {
		t.Errorf("error = %v, want a 'not frozen for <host platform>' message", err)
	}
	if resolver.called {
		t.Error("the guard must fail closed BEFORE consulting the resolver when the host arch is absent")
	}
	if runner.called {
		t.Fatal("a host-arch-absent pin must NOT boot the image runner")
	}
}

func TestRunInImage_ComposedPinBootsWhenResolverAbsent(t *testing.T) {
	// Backward-compat: when no resolver is wired, a composed pin boots as it did
	// before the guard (#1856 behavior), booting the LocalTag verbatim. The guard
	// degrades gracefully when the seam is absent, mirroring the ImageRunner
	// nil-guard discipline.
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	imageID := "sha256:" + strings.Repeat("b", 64)
	lp := composedPin(localTag, imageID)
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	if _, err := runInImage(context.Background(), lp, Options{
		Name:        "tools-plan",
		Version:     "1.0.0",
		ImageRunner: runner,
		// ImageDigestResolver intentionally nil.
	}); err != nil {
		t.Fatalf("runInImage: %v", err)
	}
	if !runner.called {
		t.Fatal("a composed pin with no resolver must boot as before")
	}
	if runner.spec.Image != localTag {
		t.Errorf("booted image = %q, want the local tag %q", runner.spec.Image, localTag)
	}
}

func TestRunInImage_LocallyFrozenPlanTakesLocalPathNotRegistry(t *testing.T) {
	// Regression: a locally-frozen composed plan (no origin sidecar, so
	// ImageOrigin.Present == false) must still take the local-tag #1863 boot
	// path unchanged and must NEVER consult a wired RegistryImageResolver. This
	// is the coexistence guarantee: adding the registry-pull branch (#1903) does
	// not change how a plan frozen on this machine boots.
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	imageID := "sha256:" + strings.Repeat("b", 64)
	lp := composedPin(localTag, imageID) // composedPin leaves ImageOrigin zero
	if lp.ImageOrigin.Present {
		t.Fatal("test precondition: a locally-frozen plan must have no registry origin")
	}
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	localGuard := &fakeImageDigestResolver{digest: imageID} // matches → boots
	registryResolver := &fakeRegistryImageResolver{bootRef: "should-never-be-used"}

	if _, err := runInImage(context.Background(), lp, Options{
		Name:                  "tools-plan",
		Version:               "1.0.0",
		ImageRunner:           runner,
		ImageDigestResolver:   localGuard,
		RegistryImageResolver: registryResolver,
	}); err != nil {
		t.Fatalf("runInImage: %v", err)
	}
	if registryResolver.called {
		t.Fatal("a locally-frozen plan must NOT consult the registry resolver")
	}
	if !localGuard.called {
		t.Fatal("a locally-frozen composed plan must take the local-tag #1863 guard path")
	}
	if !runner.called || runner.spec.Image != localTag {
		t.Errorf("booted image = %q, want the local tag %q", runner.spec.Image, localTag)
	}
}

func TestRunInImage_LocallyFrozenPlanGuardMismatchStillFailsClosed(t *testing.T) {
	// Coexistence + fail-closed: a locally-frozen plan whose local tag resolves
	// to a different digest still fails closed on the local path even when a
	// registry resolver is wired (the registry path is never entered).
	localTag := "aileron/sandbox-tools:0123456789abcdef"
	attested := "sha256:" + strings.Repeat("b", 64)
	observed := "sha256:" + strings.Repeat("c", 64)
	lp := composedPin(localTag, attested)
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	localGuard := &fakeImageDigestResolver{digest: observed}
	registryResolver := &fakeRegistryImageResolver{bootRef: "should-never-be-used"}

	_, err := runInImage(context.Background(), lp, Options{
		Name:                  "tools-plan",
		Version:               "1.0.0",
		ImageRunner:           runner,
		ImageDigestResolver:   localGuard,
		RegistryImageResolver: registryResolver,
	})
	if err == nil {
		t.Fatal("a locally-frozen plan with a mismatched local tag must fail closed")
	}
	if registryResolver.called {
		t.Fatal("the registry resolver must not be consulted for a locally-frozen plan")
	}
	if runner.called {
		t.Fatal("a mismatched local guard must not boot")
	}
}

func TestRunInImage_GuardSkippedForNonComposedPin(t *testing.T) {
	// The guard is scoped to composed pins: an image-only / custom-base pin
	// (LocalTag == "") is booted content-addressed as `ref@digest` and the
	// resolver is NEVER consulted, even when wired. Guarding those would be wrong:
	// their Digest is a registry digest, and the resolver inspects a local tag.
	digest := "sha256:" + strings.Repeat("a", 64)
	lp := LoadedPlan{
		ContentHash:    "sha256:content",
		ResolvedImages: []freeze.ImagePin{{Ref: "registry.example.com/runner:1.4", Digest: digest}},
	}
	runner := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:content"}}
	resolver := &fakeImageDigestResolver{digest: "sha256:" + strings.Repeat("z", 64)}
	if _, err := runInImage(context.Background(), lp, Options{
		Name:                "image-only",
		Version:             "1.0.0",
		ImageRunner:         runner,
		ImageDigestResolver: resolver,
	}); err != nil {
		t.Fatalf("runInImage: %v", err)
	}
	if resolver.called {
		t.Fatal("the guard must NOT consult the resolver for a non-composed pin")
	}
	if !runner.called {
		t.Fatal("a non-composed pin must boot ref@digest")
	}
	wantImage := "registry.example.com/runner:1.4@" + digest
	if runner.spec.Image != wantImage {
		t.Errorf("booted image = %q, want %q", runner.spec.Image, wantImage)
	}
}
