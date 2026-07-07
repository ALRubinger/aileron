package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/sandbox/composition"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

// ociConfigBody builds a valid OCI image config blob for (os, arch) whose
// Entrypoint carries the given marker, so distinct markers yield distinct content
// digests and the config's own os/architecture drive the platform key
// AllPlatformConfigContentDigests reads. A composed pin binds by the
// serialization-agnostic config CONTENT digest of this blob, not the blob's sha256.
func ociConfigBody(t *testing.T, os, arch, marker string) []byte {
	t.Helper()
	img := ocispec.Image{
		Platform: ocispec.Platform{OS: os, Architecture: arch},
		Config: ocispec.ImageConfig{
			Env:        []string{"PATH=/usr/bin"},
			Entrypoint: []string{"/entry", marker},
			Cmd:        []string{"bash"},
			WorkingDir: "/work",
		},
	}
	img.RootFS.Type = "layers"
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal oci config: %v", err)
	}
	return b
}

// contentDigest is the serialization-agnostic content digest of a config blob,
// the value a composed pin attests and publish verifies.
func contentDigest(t *testing.T, configBody []byte) string {
	t.Helper()
	cc, err := imgconfig.FromOCIImageConfig(configBody)
	if err != nil {
		t.Fatalf("FromOCIImageConfig: %v", err)
	}
	d, err := cc.ContentDigest()
	if err != nil {
		t.Fatalf("ContentDigest: %v", err)
	}
	return d
}

// reserialize returns config bytes that add serialization-only noise
// (author/history) to configBody while preserving every execution-relevant
// field, mimicking a registry's push-time config re-encode. Its content digest
// equals configBody's; its raw sha256 differs.
func reserialize(t *testing.T, configBody []byte) []byte {
	t.Helper()
	var img ocispec.Image
	if err := json.Unmarshal(configBody, &img); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	img.Author = "buildkit"
	img.History = []ocispec.History{{CreatedBy: "RUN noise"}}
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal reserialized config: %v", err)
	}
	if bytes.Equal(b, configBody) {
		t.Fatal("test bug: re-serialized bytes are identical to the base")
	}
	return b
}

// tamper returns config bytes with a changed Entrypoint (an execution-relevant
// field) while keeping os/architecture, so its content digest differs from
// configBody's but its platform key is unchanged.
func tamper(t *testing.T, configBody []byte) []byte {
	t.Helper()
	var img ocispec.Image
	if err := json.Unmarshal(configBody, &img); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	img.Config.Entrypoint = []string{"/evil"}
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal tampered config: %v", err)
	}
	return b
}

// seedImage pushes a minimal single-config, single-layer OCI image whose config
// blob is configBody into st and returns the image manifest descriptor. A
// composed pin binds by the config content digest; a foreign-base pin by the
// manifest digest.
func seedImage(t *testing.T, st content.Storage, configBody []byte) ocispec.Descriptor {
	t.Helper()
	cfg := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, configBody)
	mustPush(t, st, cfg, configBody)
	layerBody := []byte("layer-" + string(cfg.Digest))
	layer := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayerGzip, layerBody)
	mustPush(t, st, layer, layerBody)
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfg,
		Layers:    []ocispec.Descriptor{layer},
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	md := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, mb)
	mustPush(t, st, md, mb)
	return md
}

func mustPush(t *testing.T, st content.Storage, desc ocispec.Descriptor, data []byte) {
	t.Helper()
	err := st.Push(context.Background(), desc, bytes.NewReader(data))
	if err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		t.Fatalf("push %s: %v", desc.Digest, err)
	}
}

// layoutArch names one platform child of a synthetic composed layout: the (os,
// arch) to stamp on the manifest and the config blob to embed.
type layoutArch struct {
	os, arch string
	body     []byte
}

// composedLayout is a synthetic stand-in for the freeze-produced OCI image layout
// the ComposedLayout seam opens: an in-memory oras store holding an OCI index over
// the given platform children (each a real image manifest) plus, optionally, a
// buildkit attestation child. byPlat maps "os/arch" to the config CONTENT digest
// the signed lock attests for that child.
type composedLayout struct {
	store  *memory.Store
	index  ocispec.Descriptor
	byPlat map[string]string
}

// buildComposedLayout stages arches (and an optional attestation child) as an OCI
// index in an in-memory store. The attestation child is a real, fetchable image
// manifest stamped unknown/unknown so oras.CopyGraph can copy it while the
// per-arch verify filters it out — exactly what a buildx-provenance index looks
// like on both sides.
func buildComposedLayout(t *testing.T, arches []layoutArch, withAttestation bool) composedLayout {
	t.Helper()
	st := memory.New()
	byPlat := make(map[string]string, len(arches))
	children := make([]ocispec.Descriptor, 0, len(arches)+1)
	for _, a := range arches {
		md := seedImage(t, st, a.body)
		md.Platform = &ocispec.Platform{OS: a.os, Architecture: a.arch}
		children = append(children, md)
		byPlat[a.os+"/"+a.arch] = contentDigest(t, a.body)
	}
	if withAttestation {
		att := seedImage(t, st, ociConfigBody(t, "unknown", "unknown", "attestation"))
		att.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
		att.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
		children = append(children, att)
	}
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: children,
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	id := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, ib)
	mustPush(t, st, id, ib)
	return composedLayout{store: st, index: id, byPlat: byPlat}
}

// twoArch is the standard multi-arch composition: honest linux/amd64 +
// linux/arm64 children and a buildkit attestation child.
func twoArch(t *testing.T) composedLayout {
	t.Helper()
	return buildComposedLayout(t, []layoutArch{
		{"linux", "amd64", ociConfigBody(t, "linux", "amd64", "amd")},
		{"linux", "arm64", ociConfigBody(t, "linux", "arm64", "arm")},
	}, true)
}

func testFrozen() store.FrozenVersion {
	return store.FrozenVersion{
		SkillMD:   []byte("# frozen skill\n"),
		Lockfile:  []byte("resolvedImages: []\n"),
		Signature: []byte("signature-bytes"),
		PublicKey: []byte("-----BEGIN PUBLIC KEY-----\n"),
	}
}

// lockDigests returns the composed pin's per-arch ConfigDigests set derived from a
// layout's attested content digests, so an honest lock matches the layout exactly.
func lockDigests(layout composedLayout) []freeze.PlatformDigest {
	out := make([]freeze.PlatformDigest, 0, len(layout.byPlat))
	for plat, dig := range layout.byPlat {
		os, arch, _ := strings.Cut(plat, "/")
		out = append(out, freeze.PlatformDigest{OS: os, Arch: arch, Digest: dig})
	}
	return out
}

// composedOptions builds publish options for a composed pin whose per-arch set is
// the layout's attested digests, with the ComposedLayout seam returning that
// layout. target is the destination store.
func composedOptions(target oras.Target, layout composedLayout) Options {
	return Options{
		Name:      "demo",
		VersionID: "v1",
		Registry:  "example.com/demo",
		Frozen:    testFrozen(),
		Lock: freeze.Lockfile{ResolvedImages: []freeze.ImagePin{
			{Ref: "aileron/sandbox-tools", LocalTag: "aileron/sandbox-tools:abc123", ConfigDigests: lockDigests(layout)},
		}},
		ComposedLayout: func(context.Context, freeze.ImagePin) (oras.ReadOnlyTarget, ocispec.Descriptor, error) {
			return layout.store, layout.index, nil
		},
		Target: target,
	}
}

// resolvePlatformChildren resolves the pushed image tag in target to an index and
// returns the runnable (os/arch) platforms it carries, so a test can assert the
// pushed manifest list is per-arch resolvable.
func resolvePlatformChildren(t *testing.T, target *memory.Store, tag string) map[string]bool {
	t.Helper()
	ctx := context.Background()
	desc, err := target.Resolve(ctx, tag)
	if err != nil {
		t.Fatalf("resolve %q: %v", tag, err)
	}
	if desc.MediaType != ocispec.MediaTypeImageIndex {
		t.Fatalf("pushed %q media type = %q, want an image index (manifest list)", tag, desc.MediaType)
	}
	raw, err := content.FetchAll(ctx, target, desc)
	if err != nil {
		t.Fatalf("fetch pushed index: %v", err)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("decode pushed index: %v", err)
	}
	got := map[string]bool{}
	for _, m := range idx.Manifests {
		if m.Platform != nil && m.Platform.OS != "unknown" {
			got[m.Platform.OS+"/"+m.Platform.Architecture] = true
		}
	}
	return got
}

// TestRunComposedMultiArchPublishesManifestList proves a composed publish pushes a
// manifest list a differing-arch consumer can pull: the pushed tag resolves to an
// OCI index carrying both platform children, and the signed artifact's subject is
// that index.
func TestRunComposedMultiArchPublishesManifestList(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)

	res, err := Run(ctx, composedOptions(target, layout))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingConfigContentDigest)
	}
	if res.ImageDigest != layout.index.Digest.String() {
		t.Errorf("image digest = %q, want the manifest-list digest %q", res.ImageDigest, layout.index.Digest)
	}
	children := resolvePlatformChildren(t, target, freeze.ComposedImageTag("v1"))
	for _, plat := range []string{"linux/amd64", "linux/arm64"} {
		if !children[plat] {
			t.Errorf("pushed manifest list is missing %s (got %v)", plat, children)
		}
	}
	assertReferrerAttached(t, target, res, layout.index.Digest.String(), freeze.BindingConfigContentDigest)
}

// TestRunComposedPerArchPostPushVerifyPasses proves the post-push re-verify passes
// when both archs match the lock's ConfigDigests set (the happy multi-arch path).
func TestRunComposedPerArchPostPushVerifyPasses(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)
	if _, err := Run(ctx, composedOptions(target, layout)); err != nil {
		t.Fatalf("Run over a matching two-arch layout must pass pre- and post-push verify: %v", err)
	}
}

// TestRunComposedSingleArchPublishes proves a single-entry ConfigDigests set flows
// through the same generalized verify (the single-arch/foreign-base parity).
func TestRunComposedSingleArchPublishes(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := buildComposedLayout(t, []layoutArch{
		{"linux", runtime.GOARCH, ociConfigBody(t, "linux", runtime.GOARCH, "solo")},
	}, false)
	res, err := Run(ctx, composedOptions(target, layout))
	if err != nil {
		t.Fatalf("Run single-arch: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingConfigContentDigest)
	}
}

// TestRunComposedReserializedConfigPublishes is the #2014 core regression at
// multi-arch shape: an arch whose layout config blob is re-serialized (byte-
// different blob, identical runtime fields) still publishes because the binding is
// content-based, not blob-digest.
func TestRunComposedReserializedConfigPublishes(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	honestArm := ociConfigBody(t, "linux", "arm64", "arm")
	layout := buildComposedLayout(t, []layoutArch{
		{"linux", "amd64", ociConfigBody(t, "linux", "amd64", "amd")},
		{"linux", "arm64", reserialize(t, honestArm)}, // blob re-encoded, content preserved
	}, true)
	// Attest the honest content digest for arm64; it equals the re-serialized one.
	opts := composedOptions(target, layout)
	setLockArch(opts, "linux", "arm64", contentDigest(t, honestArm))

	res, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run over a re-serialized child config must succeed: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingConfigContentDigest)
	}
}

// TestRunComposedContentDigestBindingOverOCIIndex is the #2012+#2014 regression:
// the composed artifact is an OCI index wrapping the platform image manifests plus
// a buildkit attestation. Publish's per-arch verify must unwrap the index and
// verify each runnable child against the signed lock rather than hard-erroring on
// the attestation entry.
func TestRunComposedContentDigestBindingOverOCIIndex(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t) // carries a buildkit attestation child
	res, err := Run(ctx, composedOptions(target, layout))
	if err != nil {
		t.Fatalf("Run over an attestation-laden index: %v", err)
	}
	if res.ImageDigest != layout.index.Digest.String() {
		t.Errorf("image digest = %q, want the index digest %q", res.ImageDigest, layout.index.Digest)
	}
	assertReferrerAttached(t, target, res, layout.index.Digest.String(), freeze.BindingConfigContentDigest)
}

// setLockArch overwrites the composed pin's attested digest for one platform,
// leaving the layout untouched — a lock/artifact divergence for the fail-closed
// tests.
func setLockArch(opts Options, os, arch, digest string) {
	cds := opts.Lock.ResolvedImages[0].ConfigDigests
	for i := range cds {
		if cds[i].OS == os && cds[i].Arch == arch {
			cds[i].Digest = digest
			return
		}
	}
	opts.Lock.ResolvedImages[0].ConfigDigests = append(cds, freeze.PlatformDigest{OS: os, Arch: arch, Digest: digest})
}

// TestRunComposedTamperedArchFailsClosedPrePush proves a pre-push local tamper (an
// arch whose layout config content digest differs from its lock entry) fails
// closed BEFORE any bytes leave the machine.
func TestRunComposedTamperedArchFailsClosedPrePush(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)
	opts := composedOptions(target, layout)
	// The lock now attests a digest the layout's arm64 child does not carry.
	setLockArch(opts, "linux", "arm64", "sha256:"+strings.Repeat("0", 64))

	_, err := Run(ctx, opts)
	if !errors.Is(err, ErrConfigContentDigestMismatch) {
		t.Fatalf("err = %v, want ErrConfigContentDigestMismatch", err)
	}
	if !strings.Contains(err.Error(), "local") || !strings.Contains(err.Error(), "arm64") {
		t.Errorf("err = %v, want a local arm64 mismatch message", err)
	}
	// Nothing must have been published on the pre-push mismatch path.
	if _, err := target.Resolve(ctx, "v1"); err == nil {
		t.Error("artifact was tagged despite a pre-push config-content mismatch")
	}
}

// resolveOverrideTarget serves overrideDesc for overrideTag on Resolve while
// pushing/fetching through the embedded store, standing in for a registry that
// substitutes the manifest a tag points at after the honest push landed.
type resolveOverrideTarget struct {
	*memory.Store
	overrideTag  string
	overrideDesc ocispec.Descriptor
}

func (r resolveOverrideTarget) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	if ref == r.overrideTag {
		return r.overrideDesc, nil
	}
	return r.Store.Resolve(ctx, ref)
}

// TestRunComposedTamperedArchFailsClosedPostPush proves a post-push registry
// tamper is caught: the honest graph copies fine, but the tag resolves to a
// substituted manifest list whose arm64 child config differs from the lock, so the
// post-push re-verify fails closed.
func TestRunComposedTamperedArchFailsClosedPostPush(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	honest := twoArch(t)

	// Pre-seed a tampered manifest list into the destination: arm64 child carries a
	// changed execution field (self-consistent digests, so it is not a corruption
	// but a substitution).
	tamperedArm := tamper(t, ociConfigBody(t, "linux", "arm64", "arm"))
	tampered := buildComposedLayout(t, []layoutArch{
		{"linux", "amd64", ociConfigBody(t, "linux", "amd64", "amd")},
		{"linux", "arm64", tamperedArm},
	}, true)
	// Copy the tampered graph into inner so its children are fetchable there.
	if err := oras.CopyGraph(ctx, tampered.store, inner, tampered.index, oras.DefaultCopyGraphOptions); err != nil {
		t.Fatalf("seed tampered graph: %v", err)
	}

	target := resolveOverrideTarget{Store: inner, overrideTag: freeze.ComposedImageTag("v1"), overrideDesc: tampered.index}
	opts := composedOptions(target, honest) // lock + layout are honest; the registry lies

	_, err := Run(ctx, opts)
	if !errors.Is(err, ErrConfigContentDigestMismatch) {
		t.Fatalf("err = %v, want ErrConfigContentDigestMismatch on a post-push tamper", err)
	}
	if !strings.Contains(err.Error(), "pushed") {
		t.Errorf("err = %v, want a pushed-side mismatch message", err)
	}
}

// TestRunComposedMissingLockArchFailsClosed proves a layout that carries fewer
// archs than the lock pins fails closed (the artifact cannot satisfy every arch a
// consumer might launch on).
func TestRunComposedMissingLockArchFailsClosed(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	// Layout has only amd64; lock pins amd64 + arm64.
	layout := buildComposedLayout(t, []layoutArch{
		{"linux", "amd64", ociConfigBody(t, "linux", "amd64", "amd")},
	}, false)
	opts := composedOptions(target, layout)
	setLockArch(opts, "linux", "arm64", "sha256:"+strings.Repeat("b", 64))

	_, err := Run(ctx, opts)
	if !errors.Is(err, ErrConfigContentDigestMismatch) {
		t.Fatalf("err = %v, want ErrConfigContentDigestMismatch for a missing lock arch", err)
	}
	if !strings.Contains(err.Error(), "missing arch") || !strings.Contains(err.Error(), "arm64") {
		t.Errorf("err = %v, want a missing-arm64 message", err)
	}
}

// TestRunComposedUnattestedLayoutArchFailsClosed proves a layout carrying an arch
// the lock does NOT attest fails closed (an image the signed lock never vouched
// for must not ship under this plan's tag).
func TestRunComposedUnattestedLayoutArchFailsClosed(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t) // amd64 + arm64
	opts := composedOptions(target, layout)
	// Drop arm64 from the lock, leaving only amd64 attested; the layout still has arm64.
	opts.Lock.ResolvedImages[0].ConfigDigests = []freeze.PlatformDigest{
		{OS: "linux", Arch: "amd64", Digest: layout.byPlat["linux/amd64"]},
	}
	_, err := Run(ctx, opts)
	if !errors.Is(err, ErrConfigContentDigestMismatch) {
		t.Fatalf("err = %v, want ErrConfigContentDigestMismatch for an unattested layout arch", err)
	}
	if !strings.Contains(err.Error(), "not attested") || !strings.Contains(err.Error(), "arm64") {
		t.Errorf("err = %v, want an unattested-arm64 message", err)
	}
}

// TestRunComposedLayoutOpenError proves a ComposedLayout seam failure surfaces as
// a publish error rather than a panic.
func TestRunComposedLayoutOpenError(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	layout := twoArch(t)
	opts := composedOptions(target, layout)
	opts.ComposedLayout = func(context.Context, freeze.ImagePin) (oras.ReadOnlyTarget, ocispec.Descriptor, error) {
		return nil, ocispec.Descriptor{}, errors.New("layout dir gone")
	}
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "open composed image layout") {
		t.Fatalf("err = %v, want an open-composed-image-layout error", err)
	}
}

// pushFailTarget wraps a memory store but rejects Push for one media type,
// standing in for a registry that refuses a specific write.
type pushFailTarget struct {
	*memory.Store
	failMediaType string
}

func (p pushFailTarget) Push(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	if desc.MediaType == p.failMediaType {
		return errors.New("registry rejected write")
	}
	return p.Store.Push(ctx, desc, r)
}

// TestRunComposedPushError proves a failure copying the manifest-list graph into
// the destination surfaces as a push-composed-image error.
func TestRunComposedPushError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	layout := twoArch(t)
	// Reject the index write so oras.CopyGraph fails on the root manifest.
	target := pushFailTarget{Store: inner, failMediaType: ocispec.MediaTypeImageIndex}
	opts := composedOptions(target, layout)
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "push composed image") {
		t.Fatalf("err = %v, want a push-composed-image error", err)
	}
}

// TestRunComposedTagError proves a failure tagging the pushed manifest list
// surfaces as a tag-composed-image error.
func TestRunComposedTagError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	layout := twoArch(t)
	target := tagFailTarget{Store: inner, failOn: freeze.ComposedImageTag("v1")}
	opts := composedOptions(target, layout)
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "tag composed image") {
		t.Fatalf("err = %v, want a tag-composed-image error", err)
	}
}

func TestRunComposedPushBlobError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	layout := twoArch(t)
	// Let the image graph push through but reject the first artifact blob.
	opts := composedOptions(pushFailTarget{Store: inner, failMediaType: freeze.MediaTypeSkillMD}, layout)
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "push artifact blob") {
		t.Fatalf("err = %v, want a push-artifact-blob error", err)
	}
}

// resolveFailTarget wraps a memory store but errors on Resolve of failOn, standing
// in for a registry whose tag read fails right after the push.
type resolveFailTarget struct {
	*memory.Store
	failOn string
}

func (r resolveFailTarget) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	if ref == r.failOn {
		return ocispec.Descriptor{}, errors.New("registry resolve failed")
	}
	return r.Store.Resolve(ctx, ref)
}

func TestRunComposedResolveError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	layout := twoArch(t)
	target := resolveFailTarget{Store: inner, failOn: freeze.ComposedImageTag("v1")}
	opts := composedOptions(target, layout)
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "resolve pushed composed image") {
		t.Fatalf("err = %v, want a resolve-pushed-image error", err)
	}
}

// stageProductionLayout writes a composed layout to the deterministic on-disk
// path composition.OCILayoutDir(tag) resolves — exactly where freeze writes and
// the production ComposedLayout opener reads. It copies every blob via an
// oci.Store, then overwrites index.json with the single manifest-list root buildx
// `type=oci` emits (oci.Store would otherwise digest-tag every manifest, leaving a
// multi-root index the opener rejects).
func stageProductionLayout(t *testing.T, tag string, layout composedLayout) {
	t.Helper()
	ctx := context.Background()
	dir, err := composition.OCILayoutDir(tag)
	if err != nil {
		t.Fatalf("OCILayoutDir: %v", err)
	}
	st, err := oci.New(dir)
	if err != nil {
		t.Fatalf("oci.New: %v", err)
	}
	if err := oras.CopyGraph(ctx, layout.store, st, layout.index, oras.DefaultCopyGraphOptions); err != nil {
		t.Fatalf("stage graph on disk: %v", err)
	}
	singleRoot := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{layout.index},
	}
	ib, err := json.Marshal(singleRoot)
	if err != nil {
		t.Fatalf("marshal single-root index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageIndexFile), ib, 0o644); err != nil {
		t.Fatalf("write single-root index.json: %v", err)
	}
}

// TestComposedLayoutOpenerReadsProductionLayout exercises the production
// ComposedLayout seam (composedLayoutOpener -> composition.OCILayoutDir ->
// ociremote.OpenOCILayout) end to end against a real on-disk layout, no docker: it
// derives the layout dir from the pin's LocalTag, opens it, and yields the
// manifest-list root plus both per-arch config digests.
func TestComposedLayoutOpenerReadsProductionLayout(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	t.Setenv("HOME", base)

	tag := "aileron/sandbox-tools:opener"
	layout := twoArch(t)
	stageProductionLayout(t, tag, layout)

	ctx := context.Background()
	store, root, err := composedLayoutOpener(ctx, freeze.ImagePin{LocalTag: tag})
	if err != nil {
		t.Fatalf("composedLayoutOpener: %v", err)
	}
	if root.Digest != layout.index.Digest {
		t.Errorf("root = %s, want the manifest-list digest %s", root.Digest, layout.index.Digest)
	}
	digs, err := ociremote.AllPlatformConfigContentDigests(ctx, store, root)
	if err != nil {
		t.Fatalf("read per-arch digests through the opener: %v", err)
	}
	if len(digs) != 2 {
		t.Fatalf("want 2 per-arch digests, got %d: %+v", len(digs), digs)
	}
}

func TestRunForeignBaseManifestDigestBinding(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	manifest := seedImage(t, src, ociConfigBody(t, "linux", "amd64", "base"))
	// The source registry resolves the pin's manifest digest as a reference.
	if err := src.Tag(ctx, manifest, manifest.Digest.String()); err != nil {
		t.Fatalf("tag source: %v", err)
	}
	target := memory.New()

	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: manifest.Digest.String()} // no LocalTag => foreign-base
	res, err := Run(ctx, Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
		Target: target,
		SourceRepo: func(ctx context.Context, ref string) (oras.ReadOnlyTarget, error) {
			return src, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BindingKind != freeze.BindingManifestDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingManifestDigest)
	}
	if res.ImageDigest != manifest.Digest.String() {
		t.Errorf("image digest = %q, want %q (manifest digest must be preserved)", res.ImageDigest, manifest.Digest)
	}
	assertReferrerAttached(t, target, res, manifest.Digest.String(), freeze.BindingManifestDigest)
}

func TestRunIdempotentReferrerDigest(t *testing.T) {
	ctx := context.Background()
	run := func() Result {
		target := memory.New()
		res, err := Run(ctx, composedOptions(target, twoArch(t)))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res
	}
	a, b := run(), run()
	if a.ArtifactDigest != b.ArtifactDigest {
		t.Errorf("artifact digest not byte-stable across runs: %q vs %q (created annotation must be pinned)", a.ArtifactDigest, b.ArtifactDigest)
	}
}

// TestRunTagsLatestAndSemver is the ergonomics regression (#2027): after a run,
// the mutable `latest` tag, the (valid) semver label, and the canonical
// content-hash tag all resolve to the SAME artifact descriptor.
func TestRunTagsLatestAndSemver(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	opts := composedOptions(target, twoArch(t))
	opts.Lock.Version = "1.2.3" // a legal OCI tag
	res, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, tag := range []string{"v1", "latest", "1.2.3"} {
		desc, err := target.Resolve(ctx, tag)
		if err != nil {
			t.Fatalf("resolve %q: %v", tag, err)
		}
		if desc.Digest.String() != res.ArtifactDigest {
			t.Errorf("tag %q resolves to %q, want the artifact digest %q", tag, desc.Digest, res.ArtifactDigest)
		}
	}
}

// TestRunNoSemverLabelTagsLatestOnly proves that with no semver label set, only
// the content-hash and `latest` tags are written (no spurious empty tag).
func TestRunNoSemverLabelTagsLatestOnly(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	res, err := Run(ctx, composedOptions(target, twoArch(t))) // Lock.Version is empty
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, tag := range []string{"v1", "latest"} {
		desc, err := target.Resolve(ctx, tag)
		if err != nil {
			t.Fatalf("resolve %q: %v", tag, err)
		}
		if desc.Digest.String() != res.ArtifactDigest {
			t.Errorf("tag %q resolves to %q, want %q", tag, desc.Digest, res.ArtifactDigest)
		}
	}
}

// TestRunInvalidSemverLabelWarnsAndSkips proves an invalid semver label (semver
// `+build` metadata is not a legal OCI tag) is skipped with a stderr warning
// rather than failing publish; the content-hash and `latest` tags still land.
func TestRunInvalidSemverLabelWarnsAndSkips(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	opts := composedOptions(target, twoArch(t))
	opts.Lock.Version = "1.2.3+build.5" // '+' is outside the OCI tag grammar
	var errb bytes.Buffer
	opts.Stderr = &errb

	res, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("an invalid semver label must not fail publish: %v", err)
	}
	if _, err := target.Resolve(ctx, "1.2.3+build.5"); err == nil {
		t.Error("the invalid semver label was tagged; want it skipped")
	}
	for _, tag := range []string{"v1", "latest"} {
		desc, err := target.Resolve(ctx, tag)
		if err != nil {
			t.Fatalf("resolve %q: %v", tag, err)
		}
		if desc.Digest.String() != res.ArtifactDigest {
			t.Errorf("tag %q resolves to %q, want %q", tag, desc.Digest, res.ArtifactDigest)
		}
	}
	if !strings.Contains(errb.String(), "not a valid OCI tag") {
		t.Errorf("stderr = %q, want a warning that the label is not a valid OCI tag", errb.String())
	}
}

// tagFailTarget wraps a memory store but fails Tag for the tag named failOn,
// standing in for a registry that rejects a specific tag write.
type tagFailTarget struct {
	*memory.Store
	failOn string
}

func (t tagFailTarget) Tag(ctx context.Context, desc ocispec.Descriptor, ref string) error {
	if ref == t.failOn {
		return errors.New("registry rejected tag " + ref)
	}
	return t.Store.Tag(ctx, desc, ref)
}

// TestRunLatestTagError proves a failure writing the mutable `latest` tag
// surfaces as a publish error rather than being swallowed.
func TestRunLatestTagError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	opts := composedOptions(tagFailTarget{Store: inner, failOn: "latest"}, twoArch(t))
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "tag artifact latest") {
		t.Fatalf("err = %v, want a tag-artifact-latest error", err)
	}
}

// TestRunSemverTagError proves a failure writing a valid semver label tag
// surfaces as a publish error.
func TestRunSemverTagError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	opts := composedOptions(tagFailTarget{Store: inner, failOn: "1.2.3"}, twoArch(t))
	opts.Lock.Version = "1.2.3"
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "tag artifact 1.2.3") {
		t.Fatalf("err = %v, want a tag-artifact-1.2.3 error", err)
	}
}

func TestRunNoImage(t *testing.T) {
	_, err := Run(context.Background(), Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{}, // no ResolvedImages
		Target: memory.New(),
	})
	if !errors.Is(err, ErrNoImage) {
		t.Fatalf("err = %v, want ErrNoImage", err)
	}
}

func TestNewRemoteRepository(t *testing.T) {
	loop, err := newRemoteRepository("localhost:5000/acme/plan")
	if err != nil {
		t.Fatalf("localhost repo: %v", err)
	}
	if !loop.PlainHTTP {
		t.Error("localhost registry should use PlainHTTP")
	}
	remote, err := newRemoteRepository("ghcr.io/acme/plan:v1")
	if err != nil {
		t.Fatalf("ghcr repo: %v", err)
	}
	if remote.PlainHTTP {
		t.Error("ghcr.io registry should use HTTPS, not PlainHTTP")
	}
	if _, err := newRemoteRepository("not a ref!!"); err == nil {
		t.Error("invalid reference: want error")
	}
}

func TestAnnotateRegistryAuthErrorGHCR(t *testing.T) {
	// The exact shape docker push surfaces for ghcr.io: an opaque
	// permission_denied naming "expected scopes" with no guidance (issue #2011).
	raw := errors.New("denied: permission_denied: The token provided does not match the expected scopes")
	got := annotateRegistryAuthError(raw, "ghcr.io/acme/plan")
	if !errors.Is(got, raw) {
		t.Fatalf("annotated error must preserve the raw error via errors.Is; got %v", got)
	}
	msg := got.Error()
	if !strings.Contains(msg, "ghcr.io") {
		t.Errorf("hint must name the registry host ghcr.io; got %q", msg)
	}
	if !strings.Contains(msg, "write:packages") {
		t.Errorf("ghcr.io hint must cite the exact write:packages scope; got %q", msg)
	}
	if !strings.Contains(msg, "expected scopes") {
		t.Errorf("raw registry message must survive in the wrapped error; got %q", msg)
	}
}

func TestAnnotateRegistryAuthErrorGenericHostNoGHCRScope(t *testing.T) {
	// A non-ghcr registry gets the host-named hint but must not have the
	// GitHub-specific write:packages scope invented for it.
	raw := errors.New("unexpected status from PUT request: 403 Forbidden")
	got := annotateRegistryAuthError(raw, "registry.example.com/team/plan")
	if !errors.Is(got, raw) {
		t.Fatalf("annotated error must preserve the raw error; got %v", got)
	}
	msg := got.Error()
	if !strings.Contains(msg, "registry.example.com") {
		t.Errorf("hint must name the registry host; got %q", msg)
	}
	if strings.Contains(msg, "write:packages") {
		t.Errorf("non-ghcr host must not invent the GHCR write:packages scope; got %q", msg)
	}
}

func TestAnnotateRegistryAuthErrorNetworkNotAnnotated(t *testing.T) {
	// A transient network fault carries no auth/scope signal and must pass
	// through untouched (identical error), not gain a misleading login hint.
	raw := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	got := annotateRegistryAuthError(raw, "ghcr.io/acme/plan")
	if got != raw {
		t.Fatalf("network fault must pass through unchanged; got %v", got)
	}
}

func TestAnnotateRegistryAuthErrorDigitRunNotAnnotated(t *testing.T) {
	// A non-auth failure whose message merely contains "403"/"401" as part of a
	// digest or size must not be misread as an auth rejection: the numeric
	// signals only count on word boundaries.
	raw := errors.New("copy manifest sha256:a403b1c0d401e2f3: unexpected EOF")
	got := annotateRegistryAuthError(raw, "ghcr.io/acme/plan")
	if got != raw {
		t.Fatalf("digit run inside a digest must not trigger the auth hint; got %v", got)
	}
}

func TestRunInvalidTargetRegistry(t *testing.T) {
	// Target nil forces production repo construction; an unparsable registry
	// fails before any network I/O.
	_, err := Run(context.Background(), Options{
		Name: "demo", VersionID: "v1", Registry: "",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{{Ref: "r", Digest: "sha256:abc"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "target registry") {
		t.Fatalf("err = %v, want a target-registry error", err)
	}
}

func TestRunForeignBaseInvalidSourceRegistry(t *testing.T) {
	// SourceRepo nil forces production source construction; an unparsable pin
	// ref fails before any network I/O.
	_, err := Run(context.Background(), Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{{Ref: "bad ref!!", Digest: "sha256:abc"}}},
		Target: memory.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "source registry") {
		t.Fatalf("err = %v, want a source-registry error", err)
	}
}

// assertReferrerAttached verifies the artifact manifest is tagged at the
// version, has the image as its subject, carries the four layers, and records
// the expected binding-kind annotation.
func assertReferrerAttached(t *testing.T, target *memory.Store, res Result, wantSubject, wantBinding string) {
	t.Helper()
	ctx := context.Background()
	desc, err := target.Resolve(ctx, "v1")
	if err != nil {
		t.Fatalf("resolve artifact tag: %v", err)
	}
	raw, err := content.FetchAll(ctx, target, desc)
	if err != nil {
		t.Fatalf("fetch artifact: %v", err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	if m.ArtifactType != freeze.ArtifactType {
		t.Errorf("artifactType = %q, want %q", m.ArtifactType, freeze.ArtifactType)
	}
	if m.Subject == nil || m.Subject.Digest.String() != wantSubject {
		t.Errorf("subject = %v, want image %q", m.Subject, wantSubject)
	}
	if len(m.Layers) != 4 {
		t.Errorf("layers = %d, want 4 (skill/lock/signature/pubkey)", len(m.Layers))
	}
	if got := m.Annotations[freeze.AnnotationBindingKind]; got != wantBinding {
		t.Errorf("binding-kind annotation = %q, want %q", got, wantBinding)
	}
	if got := m.Annotations[ocispec.AnnotationCreated]; got != reproducibleCreated {
		t.Errorf("created = %q, want fixed %q", got, reproducibleCreated)
	}
	if res.ArtifactDigest != desc.Digest.String() {
		t.Errorf("result artifact digest %q != tagged %q", res.ArtifactDigest, desc.Digest)
	}
}

func TestRunForeignBaseNoDigest(t *testing.T) {
	// A foreign-base pin (no LocalTag) with no digest cannot be copied.
	_, err := Run(context.Background(), Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{{Ref: "docker.io/library/python"}}},
		Target: memory.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "no digest") {
		t.Fatalf("err = %v, want a no-digest error", err)
	}
}

func TestRunForeignBaseCopyError(t *testing.T) {
	ctx := context.Background()
	empty := memory.New() // source cannot resolve the pin digest
	_, err := Run(ctx, Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{{Ref: "docker.io/library/python", Digest: "sha256:" + strings.Repeat("b", 64)}}},
		Target: memory.New(),
		SourceRepo: func(context.Context, string) (oras.ReadOnlyTarget, error) {
			return empty, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "copy base image") {
		t.Fatalf("err = %v, want a copy-base-image error", err)
	}
}
