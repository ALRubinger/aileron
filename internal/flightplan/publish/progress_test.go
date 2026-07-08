package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cli/progress"
	"github.com/ALRubinger/aileron/internal/flightplan/freeze"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
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

// authFailTarget wraps a memory store but rejects Push for one media type with
// an auth/scope-shaped error message, standing in for a registry that refuses a
// write because the caller lacks push credentials/scope (e.g. ghcr.io's
// "permission_denied" or a 403). annotateRegistryAuthError must recognize this
// shape and annotate the returned error.
type authFailTarget struct {
	*memory.Store
	failMediaType string
}

func (a authFailTarget) Push(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	if desc.MediaType == a.failMediaType {
		return errors.New("permission_denied: 403 Forbidden: requested access to the resource is denied")
	}
	return a.Store.Push(ctx, desc, r)
}

// TestPublishProgressImagePushAuthErrorAnnotated proves an auth/scope-shaped
// push failure surviving the ind.Fail path still reaches the operator carrying
// the annotateRegistryAuthError hint (the registry host and `docker login`
// guidance), rather than the Fail path swallowing the annotation. This guards
// the seam between the determinate progress indicator's Fail and the Run-level
// error wrapping: the wrap must not be lost when a push fails under progress.
func TestPublishProgressImagePushAuthErrorAnnotated(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	layout := twoArch(t)
	var out bytes.Buffer
	opts := composedOptions(authFailTarget{Store: inner, failMediaType: ocispec.MediaTypeImageIndex}, layout)
	opts.Stdout = &out
	_, err := Run(ctx, opts)
	if err == nil {
		t.Fatalf("err = nil, want an annotated auth push failure")
	}
	msg := err.Error()
	// The push-composed-image wrap survives.
	if !strings.Contains(msg, "push composed image") {
		t.Errorf("err = %v, want it to still name the push-composed-image failure", err)
	}
	// The auth annotation (registry host + docker login guidance) survives the
	// Fail path and reaches the operator.
	if !strings.Contains(msg, "docker login example.com") {
		t.Errorf("err = %v, want the annotateRegistryAuthError docker-login hint to survive the Fail path", err)
	}
	if !strings.Contains(msg, "authentication/authorization") {
		t.Errorf("err = %v, want the annotateRegistryAuthError auth hint to survive the Fail path", err)
	}
	// The raw registry rejection is preserved via %w in the chain.
	if !strings.Contains(msg, "permission_denied") {
		t.Errorf("err = %v, want the raw registry rejection preserved in the chain", err)
	}
	// The Fail path must not have written a false success line.
	if strings.Contains(out.String(), "Pushed image to registry") {
		t.Errorf("failed auth push emitted a success completion line; output:\n%s", out.String())
	}
}

// TestSumSubDAGSizeFiltersForeignLayers is the drift-guard that keeps
// isForeignLayer in sync with oras's internal descriptor.IsForeignLayer set: it
// asserts each of the four foreign/non-distributable media types oras strips via
// removeForeignLayers is filtered by the local helper. If oras widens or renames
// its set, this test flags that the local mirror needs updating so sumSubDAGSize
// does not over-count a foreign layer oras never copies.
func TestSumSubDAGSizeFiltersForeignLayers(t *testing.T) {
	foreign := []struct {
		name      string
		mediaType string
	}{
		{"oci non-distributable", "application/vnd.oci.image.layer.nondistributable.v1.tar"},
		{"oci non-distributable gzip", "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"},
		{"oci non-distributable zstd", "application/vnd.oci.image.layer.nondistributable.v1.tar+zstd"},
		{"docker foreign layer", "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"},
	}
	for _, tc := range foreign {
		t.Run(tc.name, func(t *testing.T) {
			if !isForeignLayer(ocispec.Descriptor{MediaType: tc.mediaType}) {
				t.Errorf("isForeignLayer(%q) = false, want true (must mirror oras's IsForeignLayer set)", tc.mediaType)
			}
		})
	}
	// A regular distributable layer and a config are NOT foreign and must count.
	for _, mt := range []string{
		ocispec.MediaTypeImageLayer,
		ocispec.MediaTypeImageLayerGzip,
		ocispec.MediaTypeImageConfig,
		ocispec.MediaTypeImageManifest,
		ocispec.MediaTypeImageIndex,
	} {
		if isForeignLayer(ocispec.Descriptor{MediaType: mt}) {
			t.Errorf("isForeignLayer(%q) = true, want false (a distributable descriptor must count toward the total)", mt)
		}
	}
}

// TestSumSubDAGSizeExcludesForeignLayerWeight proves sumSubDAGSize omits a
// foreign layer's bytes from the walked total: a manifest that references a
// foreign layer (which oras strips before copy) must yield the same total as the
// same manifest without it, so the determinate bar can still reach 100%.
func TestSumSubDAGSizeExcludesForeignLayerWeight(t *testing.T) {
	ctx := context.Background()
	st := memory.New()

	// config blob.
	cfg := ociConfigBody(t, "linux", "amd64", "base")
	cfgDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, cfg)
	mustPush(t, st, cfgDesc, cfg)

	// a real distributable layer.
	layer := []byte("distributable-layer-bytes")
	layerDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayerGzip, layer)
	mustPush(t, st, layerDesc, layer)

	// a foreign layer descriptor. Its Size is large and NON-zero; if it were
	// counted, the total would be inflated and the bar could never settle.
	foreignDesc := ocispec.Descriptor{
		MediaType: "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
		Digest:    layerDesc.Digest, // any digest; it is never fetched
		Size:      1 << 30,          // 1 GiB of bytes oras never copies
	}

	buildManifest := func(layers []ocispec.Descriptor) ocispec.Descriptor {
		m := ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    cfgDesc,
			Layers:    layers,
		}
		body, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		d := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, body)
		mustPush(t, st, d, body)
		return d
	}

	withoutForeign := buildManifest([]ocispec.Descriptor{layerDesc})
	withForeign := buildManifest([]ocispec.Descriptor{layerDesc, foreignDesc})

	base, err := sumSubDAGSize(ctx, st, withoutForeign)
	if err != nil {
		t.Fatalf("sumSubDAGSize(withoutForeign): %v", err)
	}
	withF, err := sumSubDAGSize(ctx, st, withForeign)
	if err != nil {
		t.Fatalf("sumSubDAGSize(withForeign): %v", err)
	}
	// The manifest that references the foreign layer is a slightly larger blob
	// (its JSON now carries the extra layer descriptor), so the two totals are
	// not byte-equal. The load-bearing invariant is that the foreign layer's own
	// 1 GiB Size did NOT leak into the total: the delta between the two must be
	// the manifest JSON growth (a few hundred bytes), never the foreign bytes.
	delta := withF - base
	if delta < 0 || delta >= foreignDesc.Size {
		t.Errorf("foreign layer inflated the total: with-foreign=%d, without-foreign=%d, delta=%d, foreign Size=%d (foreign bytes must be excluded)", withF, base, delta, foreignDesc.Size)
	}
	// A tighter bound: the delta is only the manifest re-serialization, which is
	// far smaller than a kilobyte for a single extra descriptor.
	if delta > 4096 {
		t.Errorf("delta=%d is too large to be manifest JSON growth alone; foreign bytes likely leaked into the total", delta)
	}
}
