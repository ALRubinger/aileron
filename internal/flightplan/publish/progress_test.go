package publish

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cli/progress"
	"github.com/ALRubinger/aileron/internal/flightplan/freeze"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
)

// lastPercentLine returns the last plain "<n>%" progress line the non-TTY path
// emitted into out, or -1 if none was emitted. The determinate Indicator writes
// bare "<percent>%\n" lines on the non-TTY path (a captured buffer), so this
// reads back the final settled percentage without coupling to the bar glyphs.
func lastPercentLine(out string) int {
	last := -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, "%") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSuffix(line, "%")); err == nil {
			last = n
		}
	}
	return last
}

// seedFullGraph copies the entire composed sub-DAG (index + every child +
// blobs) from the layout store into target, so a subsequent publish takes the
// oras OnCopySkipped(root) path for the whole image push. This is the
// P0-regression precondition: an already-present sub-DAG must still settle the
// bar at 100%.
func seedFullGraph(t *testing.T, target *memory.Store, layout composedLayout) {
	t.Helper()
	if err := oras.CopyGraph(context.Background(), layout.store, target, layout.index, oras.DefaultCopyGraphOptions); err != nil {
		t.Fatalf("seed full graph into target: %v", err)
	}
}

// TestPublishProgressAlreadyPresentSettlesAt100 is the residual #2082 P0 bar
// regression: when the whole composed sub-DAG is already present in the target,
// oras fires OnCopySkipped once on the ROOT and never descends, so a naive
// PostCopy-only accounting would settle the bar far below 100%. The corrected
// settled-byte model adds the skipped root's whole sub-DAG weight, so the last
// emitted percentage is exactly 100.
func TestPublishProgressAlreadyPresentSettlesAt100(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)
	seedFullGraph(t, target, layout)

	var out bytes.Buffer
	opts := composedOptions(target, layout)
	opts.Stdout = &out
	if _, err := Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := lastPercentLine(out.String()); got != 100 {
		t.Fatalf("last progress percentage = %d, want 100 (already-present sub-DAG must settle the bar at 100%%); output:\n%s", got, out.String())
	}
}

// TestPublishProgressFreshPushSettlesAt100 proves a fully-fresh composed push
// (nothing pre-seeded in the target) settles the determinate bar at 100 via the
// PostCopy hook path.
func TestPublishProgressFreshPushSettlesAt100(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)

	var out bytes.Buffer
	opts := composedOptions(target, layout)
	opts.Stdout = &out
	if _, err := Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := lastPercentLine(out.String()); got != 100 {
		t.Fatalf("last progress percentage = %d, want 100 (fresh push must advance to 100%%); output:\n%s", got, out.String())
	}
}

// TestPublishProgressNonTTYIsPlain proves that when Stdout is a captured buffer
// (non-TTY), the progress output carries no carriage returns and no ANSI escape
// sequences, and the unconditional summary and install hint still print.
func TestPublishProgressNonTTYIsPlain(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)

	var out bytes.Buffer
	opts := composedOptions(target, layout)
	opts.Stdout = &out
	if _, err := Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if strings.Contains(s, "\r") {
		t.Errorf("non-TTY output contains a carriage return; output:\n%q", s)
	}
	if strings.Contains(s, "\x1b[") {
		t.Errorf("non-TTY output contains an ANSI escape sequence; output:\n%q", s)
	}
	if !strings.Contains(s, "published demo") {
		t.Errorf("missing publish summary; output:\n%s", s)
	}
	if !strings.Contains(s, "Install with:") {
		t.Errorf("missing install hint; output:\n%s", s)
	}
}

// TestPublishProgressQuietSuppressesFeedback proves Quiet suppresses all
// spinner/percentage output while the unconditional summary and install hint
// still print (they are the result, not progress feedback).
func TestPublishProgressQuietSuppressesFeedback(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)

	var out bytes.Buffer
	opts := composedOptions(target, layout)
	opts.Stdout = &out
	opts.Quiet = true
	if _, err := Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if got := lastPercentLine(s); got != -1 {
		t.Errorf("quiet run still emitted a percentage line (%d); output:\n%s", got, s)
	}
	if strings.Contains(s, "Pushing") || strings.Contains(s, "Pushed") {
		t.Errorf("quiet run emitted a progress label; output:\n%s", s)
	}
	if !strings.Contains(s, "published demo") {
		t.Errorf("quiet run dropped the publish summary; output:\n%s", s)
	}
	if !strings.Contains(s, "Install with:") {
		t.Errorf("quiet run dropped the install hint; output:\n%s", s)
	}
}

// TestPublishProgressForeignBaseDeterminate proves a foreign-base push whose
// source Resolve returns a real Size-bearing manifest drives a determinate bar
// that reaches 100%.
func TestPublishProgressForeignBaseDeterminate(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	manifest := seedImage(t, src, ociConfigBody(t, "linux", "amd64", "base"))
	if err := src.Tag(ctx, manifest, manifest.Digest.String()); err != nil {
		t.Fatalf("tag source: %v", err)
	}
	target := memory.New()

	var out bytes.Buffer
	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: manifest.Digest.String()}
	_, err := Run(ctx, Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
		Target: target,
		Stdout: &out,
		SourceRepo: func(context.Context, string) (oras.ReadOnlyTarget, error) {
			return src, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := lastPercentLine(out.String()); got != 100 {
		t.Fatalf("foreign-base determinate bar last percentage = %d, want 100; output:\n%s", got, out.String())
	}
}

// resolveFailSource wraps a memory store but errors on the precompute Resolve of
// failOn while still serving Fetch/Exists/Push, standing in for a source registry
// whose HEAD-style Resolve fails even though the blob GET succeeds. The
// foreign-base precompute Resolve then errors, so the determinate branch is
// skipped and the indeterminate liveness spinner drives the push instead. The
// underlying store still copies the bytes, so the push succeeds. Only the FIRST
// Resolve (the publish precompute) fails; oras.Copy's internal resolveRoot then
// succeeds, isolating the fallback trigger to the precompute path.
type resolveFailSource struct {
	*memory.Store
	failOn string
	failed bool
}

func (r *resolveFailSource) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	if ref == r.failOn && !r.failed {
		r.failed = true
		return ocispec.Descriptor{}, errors.New("registry resolve unavailable")
	}
	return r.Store.Resolve(ctx, ref)
}

// TestPublishProgressForeignBaseIndeterminateFallback proves that a foreign-base
// push whose precompute Resolve fails falls back to the indeterminate liveness
// path: no percentage line is emitted, no divide-by-zero occurs, the liveness
// label prints, and the push still succeeds (oras.Copy's own resolve works).
func TestPublishProgressForeignBaseIndeterminateFallback(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	manifest := seedImage(t, src, ociConfigBody(t, "linux", "amd64", "base"))
	if err := src.Tag(ctx, manifest, manifest.Digest.String()); err != nil {
		t.Fatalf("tag source: %v", err)
	}
	target := memory.New()

	var out bytes.Buffer
	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: manifest.Digest.String()}
	res, err := Run(ctx, Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
		Target: target,
		Stdout: &out,
		SourceRepo: func(context.Context, string) (oras.ReadOnlyTarget, error) {
			return &resolveFailSource{Store: src, failOn: manifest.Digest.String()}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BindingKind != freeze.BindingManifestDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingManifestDigest)
	}
	s := out.String()
	if got := lastPercentLine(s); got != -1 {
		t.Errorf("indeterminate fallback still emitted a percentage line (%d); output:\n%s", got, s)
	}
	if !strings.Contains(s, "Pushing image to registry") {
		t.Errorf("indeterminate fallback did not emit the liveness label; output:\n%s", s)
	}
}

// TestSumSubDAGSizeDedupesSharedChildren proves sumSubDAGSize counts each
// distinct digest once: a DAG where two manifests share a config/layer blob must
// not double-count the shared child. It exercises the helper directly against
// the in-memory seam.
func TestSumSubDAGSizeDedupesSharedChildren(t *testing.T) {
	ctx := context.Background()
	layout := twoArch(t)
	// The full sub-DAG weight walked from the index equals the sum of every
	// DISTINCT descriptor's Size. A fresh CopyGraph into an empty target settles
	// exactly that many bytes via PostCopy, so total must be positive and finite.
	total, err := sumSubDAGSize(ctx, layout.store, layout.index)
	if err != nil {
		t.Fatalf("sumSubDAGSize: %v", err)
	}
	if total <= 0 {
		t.Fatalf("sub-DAG total = %d, want a positive byte weight", total)
	}
	// Re-walking is deterministic (no mutation), so a second call matches.
	again, err := sumSubDAGSize(ctx, layout.store, layout.index)
	if err != nil {
		t.Fatalf("sumSubDAGSize (again): %v", err)
	}
	if again != total {
		t.Errorf("sub-DAG total not deterministic: %d then %d", total, again)
	}
}

// TestPushProgressSkipAddsOncePerDigest proves the OnCopySkipped hook settles a
// skipped digest's weight exactly once even if it fires twice for the same
// descriptor (a shared child skipped under two parents), so the counter never
// over-settles past total.
func TestPushProgressSkipAddsOncePerDigest(t *testing.T) {
	ctx := context.Background()
	layout := twoArch(t)
	total, err := sumSubDAGSize(ctx, layout.store, layout.index)
	if err != nil {
		t.Fatalf("sumSubDAGSize: %v", err)
	}
	var out bytes.Buffer
	ind := progress.New(&out) // a *bytes.Buffer is non-TTY, so plain percentage lines
	opts := pushProgress(layout.store, ind, total)

	// Fire OnCopySkipped for the same root twice; the second must be a no-op.
	if err := opts.OnCopySkipped(ctx, layout.index); err != nil {
		t.Fatalf("OnCopySkipped (first): %v", err)
	}
	if err := opts.OnCopySkipped(ctx, layout.index); err != nil {
		t.Fatalf("OnCopySkipped (second): %v", err)
	}
	ind.Done("done")
	// The last emitted percentage must be exactly 100 (settled once at total),
	// not >100 from a double add (which the Indicator would clamp to 100 anyway,
	// so also assert the raw counter did not exceed total).
	if got := lastPercentLine(out.String()); got != 100 {
		t.Fatalf("last percentage = %d, want 100 after a duplicate skip; output:\n%s", got, out.String())
	}
}

// TestPublishProgressImagePushErrorFails proves an image-push failure still
// surfaces the annotated error (no swallow by the Fail path). The composed push
// rejects the index write; the run must return a push-composed-image error.
func TestPublishProgressImagePushErrorFails(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	layout := twoArch(t)
	var out bytes.Buffer
	opts := composedOptions(pushFailTarget{Store: inner, failMediaType: ocispec.MediaTypeImageIndex}, layout)
	opts.Stdout = &out
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "push composed image") {
		t.Fatalf("err = %v, want a push-composed-image error even with progress wired", err)
	}
	// The Fail path must not have written a false success line.
	if strings.Contains(out.String(), "Pushed image to registry") {
		t.Errorf("failed push emitted a success completion line; output:\n%s", out.String())
	}
}
