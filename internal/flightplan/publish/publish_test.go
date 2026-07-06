package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/store"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// seedImage pushes a minimal single-config, single-layer OCI image into st and
// returns the image manifest descriptor and its config blob descriptor. A
// composed pin binds by the config digest; a foreign-base pin by the manifest
// digest.
func seedImage(t *testing.T, st content.Storage, configBody string) (manifest, config ocispec.Descriptor) {
	t.Helper()
	cfg := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, []byte(configBody))
	mustPush(t, st, cfg, []byte(configBody))
	layer := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayerGzip, []byte("layer-bytes"))
	mustPush(t, st, layer, []byte("layer-bytes"))
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
	return md, cfg
}

func mustPush(t *testing.T, st content.Storage, desc ocispec.Descriptor, data []byte) {
	t.Helper()
	if err := st.Push(context.Background(), desc, bytes.NewReader(data)); err != nil {
		t.Fatalf("push %s: %v", desc.Digest, err)
	}
}

// fakeImageSource stands in for the docker CLI: ConfigDigest returns a
// controllable local config digest, and Push is a no-op (composed tests
// pre-seed the target with the "pushed" image tagged as freeze.ComposedImageTag).
type fakeImageSource struct {
	configDigest string
	configErr    error
	pushErr      error
}

func (f fakeImageSource) ConfigDigest(ctx context.Context, localTag string) (string, error) {
	return f.configDigest, f.configErr
}

func (f fakeImageSource) Push(ctx context.Context, localTag, registry, imageTag string) error {
	return f.pushErr
}

// seedComposedTarget seeds target with a composed image tagged where
// publishComposed resolves it, returning the manifest and config descriptors.
func seedComposedTarget(t *testing.T, target *memory.Store, versionID, configBody string) (manifest, config ocispec.Descriptor) {
	t.Helper()
	manifest, config = seedImage(t, target, configBody)
	if err := target.Tag(context.Background(), manifest, freeze.ComposedImageTag(versionID)); err != nil {
		t.Fatalf("tag composed image: %v", err)
	}
	return manifest, config
}

func testFrozen() store.FrozenVersion {
	return store.FrozenVersion{
		SkillMD:   []byte("# frozen skill\n"),
		Lockfile:  []byte("resolvedImages: []\n"),
		Signature: []byte("signature-bytes"),
		PublicKey: []byte("-----BEGIN PUBLIC KEY-----\n"),
	}
}

func composedOptions(target *memory.Store, cfg ocispec.Descriptor) Options {
	return Options{
		Name:      "demo",
		VersionID: "v1",
		Registry:  "example.com/demo",
		Frozen:    testFrozen(),
		Lock: freeze.Lockfile{ResolvedImages: []freeze.ImagePin{
			{Ref: "aileron/sandbox-tools", Digest: cfg.Digest.String(), LocalTag: "aileron/sandbox-tools:abc123"},
		}},
		ImageSource: fakeImageSource{configDigest: cfg.Digest.String()},
		Target:      target,
	}
}

func TestRunComposedConfigDigestBinding(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	manifest, cfg := seedComposedTarget(t, target, "v1", "composed-config")

	res, err := Run(ctx, composedOptions(target, cfg))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingConfigDigest)
	}
	if res.ImageDigest != manifest.Digest.String() {
		t.Errorf("image digest = %q, want %q", res.ImageDigest, manifest.Digest)
	}
	assertReferrerAttached(t, target, res, manifest.Digest.String(), freeze.BindingConfigDigest)
}

// seedComposedIndexTarget seeds target with the composed image published as an
// OCI image index (the shape `docker push` emits on Docker Desktop's containerd
// image store): the single-platform image manifest plus a buildkit attestation
// manifest, tagged where publishComposed resolves it. It returns the index and
// config descriptors. The config-digest read-back must unwrap the index.
func seedComposedIndexTarget(t *testing.T, target *memory.Store, versionID, configBody string) (index, config ocispec.Descriptor) {
	t.Helper()
	ctx := context.Background()
	image, cfg := seedImage(t, target, configBody)
	image.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}

	// A buildkit attestation entry: unknown/unknown platform + attestation type.
	// Selection skips it, so its blobs are never fetched and need not be pushed.
	attManifest := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, []byte("attestation-"+configBody))
	attManifest.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	attManifest.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}

	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{image, attManifest},
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	id := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, ib)
	mustPush(t, target, id, ib)
	if err := target.Tag(ctx, id, freeze.ComposedImageTag(versionID)); err != nil {
		t.Fatalf("tag composed index: %v", err)
	}
	return id, cfg
}

// TestRunComposedConfigDigestBindingOverOCIIndex is the #2012 regression: the
// containerd image store makes the pushed composed ref an OCI index wrapping the
// image manifest plus a buildkit attestation. Publish's post-push config-digest
// read-back must unwrap the index and verify against the signed lock rather than
// hard-erroring "image manifest has no config digest".
func TestRunComposedConfigDigestBindingOverOCIIndex(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	index, cfg := seedComposedIndexTarget(t, target, "v1", "composed-config")

	res, err := Run(ctx, composedOptions(target, cfg))
	if err != nil {
		t.Fatalf("Run over an OCI-index composed image: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingConfigDigest)
	}
	// The subject/image digest is the resolved INDEX digest (what Docker/containerd
	// boot correctly); the config-digest binding was verified from the unwrapped
	// image manifest.
	if res.ImageDigest != index.Digest.String() {
		t.Errorf("image digest = %q, want the resolved index digest %q", res.ImageDigest, index.Digest)
	}
	assertReferrerAttached(t, target, res, index.Digest.String(), freeze.BindingConfigDigest)
}

func TestRunComposedConfigDigestMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	target := memory.New()

	// The local image's config digest differs from what the signed lock attests,
	// so publish must fail BEFORE pushing anything (no image seeded into target).
	opts := Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock: freeze.Lockfile{ResolvedImages: []freeze.ImagePin{
			{Ref: "r", Digest: "sha256:" + strings.Repeat("0", 64), LocalTag: "t"},
		}},
		ImageSource: fakeImageSource{configDigest: "sha256:" + strings.Repeat("1", 64)},
		Target:      target,
	}
	if _, err := Run(ctx, opts); !errors.Is(err, ErrConfigDigestMismatch) {
		t.Fatalf("err = %v, want ErrConfigDigestMismatch", err)
	}
	// Nothing must have been published on the mismatch path.
	if _, err := target.Resolve(ctx, "v1"); err == nil {
		t.Error("artifact was tagged despite a config-digest mismatch")
	}
}

func TestRunComposedPushError(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	_, cfg := seedComposedTarget(t, target, "v1", "composed-config")
	opts := composedOptions(target, cfg)
	opts.ImageSource = fakeImageSource{configDigest: cfg.Digest.String(), pushErr: errors.New("registry unauthorized")}
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "push composed image") {
		t.Fatalf("err = %v, want a push-composed-image error", err)
	}
}

func TestRunForeignBaseManifestDigestBinding(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	manifest, _ := seedImage(t, src, "base-config")
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
		_, cfg := seedComposedTarget(t, target, "v1", "composed-config")
		res, err := Run(ctx, composedOptions(target, cfg))
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

// pushFailTarget wraps a memory store but rejects every Push, standing in for a
// registry that refuses a write (unauthenticated / quota / transient).
type pushFailTarget struct{ *memory.Store }

func (pushFailTarget) Push(context.Context, ocispec.Descriptor, io.Reader) error {
	return errors.New("registry rejected write")
}

func TestRunPushBlobError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	_, cfg := seedComposedTarget(t, inner, "v1", "composed-config") // image pre-seeded, no push needed
	opts := composedOptions(inner, cfg)
	opts.Target = pushFailTarget{inner}
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "push artifact blob") {
		t.Fatalf("err = %v, want a push-artifact-blob error", err)
	}
}

func TestRunComposedResolveError(t *testing.T) {
	ctx := context.Background()
	target := memory.New() // NOT seeded: the "pushed" image is absent, so resolve fails
	opts := Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock: freeze.Lockfile{ResolvedImages: []freeze.ImagePin{
			{Ref: "r", Digest: "sha256:" + strings.Repeat("a", 64), LocalTag: "t"},
		}},
		ImageSource: fakeImageSource{configDigest: "sha256:" + strings.Repeat("a", 64)},
		Target:      target,
	}
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "resolve pushed composed image") {
		t.Fatalf("err = %v, want a resolve-pushed-image error", err)
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
