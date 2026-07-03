package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
)

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
			{Ref: descriptiveRef, Digest: imageID, LocalTag: localTag},
		},
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
