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
	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"
	"github.com/ALRubinger/aileron/internal/flightplan/store"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

// hostConfigDigests builds a one-entry per-arch config-digest set keyed by the
// host platform (linux/GOARCH), so a composed pin's HostConfigDigest selects it
// on whatever arch the test runs on. Mirrors freeze.resolveImages's S2 producer.
func hostConfigDigests(digest string) []freeze.PlatformDigest {
	return []freeze.PlatformDigest{{OS: "linux", Arch: runtime.GOARCH, Digest: digest}}
}

// ociConfigBody builds a valid OCI image config blob whose Entrypoint carries
// the given marker, so distinct markers yield distinct content digests. A
// composed pin binds by the serialization-agnostic config CONTENT digest of
// this blob, not the blob's own sha256.
func ociConfigBody(t *testing.T, marker string) []byte {
	t.Helper()
	img := ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
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
// field, mimicking the containerd store's push-time config re-encode. Its
// content digest equals configBody's; its raw sha256 differs.
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
// field), so its content digest differs from configBody's.
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

// fakeImageSource stands in for the docker CLI: ConfigContentDigest returns a
// controllable local config content digest, and Push is a no-op (composed tests
// pre-seed the target with the "pushed" image tagged as freeze.ComposedImageTag).
type fakeImageSource struct {
	configContentDigest string
	configErr           error
	pushErr             error
}

func (f fakeImageSource) ConfigContentDigest(ctx context.Context, localTag string) (string, error) {
	return f.configContentDigest, f.configErr
}

func (f fakeImageSource) Push(ctx context.Context, localTag, registry, imageTag string) error {
	return f.pushErr
}

// seedComposedTarget seeds target with a composed image (config blob = configBody)
// tagged where publishComposed resolves it, returning the manifest descriptor.
func seedComposedTarget(t *testing.T, target *memory.Store, versionID string, configBody []byte) ocispec.Descriptor {
	t.Helper()
	manifest := seedImage(t, target, configBody)
	if err := target.Tag(context.Background(), manifest, freeze.ComposedImageTag(versionID)); err != nil {
		t.Fatalf("tag composed image: %v", err)
	}
	return manifest
}

func testFrozen() store.FrozenVersion {
	return store.FrozenVersion{
		SkillMD:   []byte("# frozen skill\n"),
		Lockfile:  []byte("resolvedImages: []\n"),
		Signature: []byte("signature-bytes"),
		PublicKey: []byte("-----BEGIN PUBLIC KEY-----\n"),
	}
}

// composedOptions builds publish options for a composed pin whose attested
// Digest is the content digest of configBody, with a fake image source that
// reports the same local content digest (an honest local image).
func composedOptions(target *memory.Store, configBody []byte) Options {
	cd := contentDigestNoT(configBody)
	return Options{
		Name:      "demo",
		VersionID: "v1",
		Registry:  "example.com/demo",
		Frozen:    testFrozen(),
		Lock: freeze.Lockfile{ResolvedImages: []freeze.ImagePin{
			{Ref: "aileron/sandbox-tools", ConfigDigests: hostConfigDigests(cd), LocalTag: "aileron/sandbox-tools:abc123"},
		}},
		ImageSource: fakeImageSource{configContentDigest: cd},
		Target:      target,
	}
}

// contentDigestNoT computes the content digest outside a *testing.T context (for
// option construction); it panics on a malformed body, which only a test bug
// produces.
func contentDigestNoT(configBody []byte) string {
	cc, err := imgconfig.FromOCIImageConfig(configBody)
	if err != nil {
		panic(err)
	}
	d, err := cc.ContentDigest()
	if err != nil {
		panic(err)
	}
	return d
}

func TestRunComposedContentDigestBinding(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	body := ociConfigBody(t, "composed")
	manifest := seedComposedTarget(t, target, "v1", body)

	res, err := Run(ctx, composedOptions(target, body))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingConfigContentDigest)
	}
	if res.ImageDigest != manifest.Digest.String() {
		t.Errorf("image digest = %q, want %q", res.ImageDigest, manifest.Digest)
	}
	assertReferrerAttached(t, target, res, manifest.Digest.String(), freeze.BindingConfigContentDigest)
}

// TestRunComposedHostArchAbsentFailsClosed proves publish fails closed when the
// composed pin's per-arch config-digest set carries no entry for the host
// platform: single-arch publish can only verify the image it built for this
// host, so a pin built for other arches only is refused before any bytes leave.
func TestRunComposedHostArchAbsentFailsClosed(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	body := ociConfigBody(t, "composed")
	seedComposedTarget(t, target, "v1", body)

	opts := composedOptions(target, body)
	// Re-key the config-digest set to a bogus arch that never equals the host.
	opts.Lock.ResolvedImages[0].ConfigDigests = []freeze.PlatformDigest{
		{OS: "linux", Arch: "no-such-arch", Digest: contentDigestNoT(body)},
	}

	_, err := Run(ctx, opts)
	if !errors.Is(err, ErrConfigContentDigestMismatch) {
		t.Fatalf("err = %v, want ErrConfigContentDigestMismatch for a host-arch-absent pin", err)
	}
	if !strings.Contains(err.Error(), "not published for") {
		t.Errorf("err = %v, want a 'not published for <host platform>' message", err)
	}
}

// TestRunComposedReserializedConfigPublishes is the #2014 core regression: a
// composed image whose PUSHED config blob was re-serialized (byte-different
// config blob, identical runtime fields, as the containerd store produces)
// publishes successfully because the binding is content-based, not blob-digest.
func TestRunComposedReserializedConfigPublishes(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	honest := ociConfigBody(t, "composed")
	// The registry holds the re-serialized config (what containerd `docker push`
	// emits); the local image reports the honest content digest.
	pushed := reserialize(t, honest)
	manifest := seedComposedTarget(t, target, "v1", pushed)

	// The lock attests the honest content digest; the fake local source reports it.
	opts := composedOptions(target, honest)

	res, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run over a re-serialized pushed config must succeed: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingConfigContentDigest)
	}
	if res.ImageDigest != manifest.Digest.String() {
		t.Errorf("image digest = %q, want %q", res.ImageDigest, manifest.Digest)
	}
}

// TestRunComposedTamperedPushedConfigFailsClosed proves the integrity guarantee
// is preserved: if the registry serves a composed image whose config has a
// changed execution-relevant field, the post-push content-digest check refuses
// it even though the local pre-push check passed.
func TestRunComposedTamperedPushedConfigFailsClosed(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	honest := ociConfigBody(t, "composed")
	tampered := tamper(t, honest)
	seedComposedTarget(t, target, "v1", tampered)

	opts := composedOptions(target, honest) // local honest, pushed tampered
	_, err := Run(ctx, opts)
	if !errors.Is(err, ErrConfigContentDigestMismatch) {
		t.Fatalf("err = %v, want ErrConfigContentDigestMismatch", err)
	}
	if !strings.Contains(err.Error(), "pushed config") {
		t.Errorf("err = %v, want a pushed-config mismatch message", err)
	}
}

// seedComposedIndexTarget seeds target with the composed image published as an
// OCI image index (the shape `docker push` emits on Docker Desktop's containerd
// image store): the single-platform image manifest plus a buildkit attestation
// manifest, tagged where publishComposed resolves it. It returns the index
// descriptor. The content-digest read-back must unwrap the index.
func seedComposedIndexTarget(t *testing.T, target *memory.Store, versionID string, configBody []byte) ocispec.Descriptor {
	t.Helper()
	ctx := context.Background()
	image := seedImage(t, target, configBody)
	image.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}

	// A buildkit attestation entry: unknown/unknown platform + attestation type.
	// Selection skips it, so its blobs are never fetched and need not be pushed.
	attManifest := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, []byte("attestation-"+string(image.Digest)))
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
	return id
}

// TestRunComposedContentDigestBindingOverOCIIndex is the #2012+#2014 regression:
// the containerd image store makes the pushed composed ref an OCI index wrapping
// the image manifest plus a buildkit attestation. Publish's post-push
// content-digest read-back must unwrap the index and verify against the signed
// lock rather than hard-erroring.
func TestRunComposedContentDigestBindingOverOCIIndex(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	body := ociConfigBody(t, "composed")
	index := seedComposedIndexTarget(t, target, "v1", body)

	res, err := Run(ctx, composedOptions(target, body))
	if err != nil {
		t.Fatalf("Run over an OCI-index composed image: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", res.BindingKind, freeze.BindingConfigContentDigest)
	}
	// The subject/image digest is the resolved INDEX digest (what Docker/containerd
	// boot correctly); the content-digest binding was verified from the unwrapped
	// image manifest.
	if res.ImageDigest != index.Digest.String() {
		t.Errorf("image digest = %q, want the resolved index digest %q", res.ImageDigest, index.Digest)
	}
	assertReferrerAttached(t, target, res, index.Digest.String(), freeze.BindingConfigContentDigest)
}

func TestRunComposedContentDigestMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	target := memory.New()

	// The local image's config content digest differs from what the signed lock
	// attests, so publish must fail BEFORE pushing anything (no image seeded).
	opts := Options{
		Name: "demo", VersionID: "v1", Registry: "example.com/demo",
		Frozen: testFrozen(),
		Lock: freeze.Lockfile{ResolvedImages: []freeze.ImagePin{
			{Ref: "r", ConfigDigests: hostConfigDigests("sha256:" + strings.Repeat("0", 64)), LocalTag: "t"},
		}},
		ImageSource: fakeImageSource{configContentDigest: "sha256:" + strings.Repeat("1", 64)},
		Target:      target,
	}
	if _, err := Run(ctx, opts); !errors.Is(err, ErrConfigContentDigestMismatch) {
		t.Fatalf("err = %v, want ErrConfigContentDigestMismatch", err)
	}
	// Nothing must have been published on the mismatch path.
	if _, err := target.Resolve(ctx, "v1"); err == nil {
		t.Error("artifact was tagged despite a config-content mismatch")
	}
}

func TestRunComposedPushError(t *testing.T) {
	ctx := context.Background()
	target := memory.New()
	body := ociConfigBody(t, "composed")
	seedComposedTarget(t, target, "v1", body)
	opts := composedOptions(target, body)
	opts.ImageSource = fakeImageSource{configContentDigest: contentDigest(t, body), pushErr: errors.New("registry unauthorized")}
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "push composed image") {
		t.Fatalf("err = %v, want a push-composed-image error", err)
	}
}

func TestRunForeignBaseManifestDigestBinding(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	manifest := seedImage(t, src, ociConfigBody(t, "base"))
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
	body := ociConfigBody(t, "composed")
	run := func() Result {
		target := memory.New()
		seedComposedTarget(t, target, "v1", body)
		res, err := Run(ctx, composedOptions(target, body))
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
	body := ociConfigBody(t, "composed")
	seedComposedTarget(t, target, "v1", body)

	opts := composedOptions(target, body)
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
	body := ociConfigBody(t, "composed")
	seedComposedTarget(t, target, "v1", body)

	res, err := Run(ctx, composedOptions(target, body)) // Lock.Version is empty
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
	body := ociConfigBody(t, "composed")
	seedComposedTarget(t, target, "v1", body)

	opts := composedOptions(target, body)
	opts.Lock.Version = "1.2.3+build.5" // '+' is outside the OCI tag grammar
	var errb bytes.Buffer
	opts.Stderr = &errb

	res, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("an invalid semver label must not fail publish: %v", err)
	}
	// The illegal label must NOT have been tagged.
	if _, err := target.Resolve(ctx, "1.2.3+build.5"); err == nil {
		t.Error("the invalid semver label was tagged; want it skipped")
	}
	// The valid tags still resolve.
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
	body := ociConfigBody(t, "composed")
	seedComposedTarget(t, inner, "v1", body)
	opts := composedOptions(inner, body)
	opts.Target = tagFailTarget{Store: inner, failOn: "latest"}
	if _, err := Run(ctx, opts); err == nil || !strings.Contains(err.Error(), "tag artifact latest") {
		t.Fatalf("err = %v, want a tag-artifact-latest error", err)
	}
}

// TestRunSemverTagError proves a failure writing a valid semver label tag
// surfaces as a publish error.
func TestRunSemverTagError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	body := ociConfigBody(t, "composed")
	seedComposedTarget(t, inner, "v1", body)
	opts := composedOptions(inner, body)
	opts.Lock.Version = "1.2.3"
	opts.Target = tagFailTarget{Store: inner, failOn: "1.2.3"}
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

// pushFailTarget wraps a memory store but rejects every Push, standing in for a
// registry that refuses a write (unauthenticated / quota / transient).
type pushFailTarget struct{ *memory.Store }

func (pushFailTarget) Push(context.Context, ocispec.Descriptor, io.Reader) error {
	return errors.New("registry rejected write")
}

func TestRunPushBlobError(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	body := ociConfigBody(t, "composed")
	seedComposedTarget(t, inner, "v1", body) // image pre-seeded, no push needed
	opts := composedOptions(inner, body)
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
			{Ref: "r", ConfigDigests: hostConfigDigests("sha256:" + strings.Repeat("a", 64)), LocalTag: "t"},
		}},
		ImageSource: fakeImageSource{configContentDigest: "sha256:" + strings.Repeat("a", 64)},
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
