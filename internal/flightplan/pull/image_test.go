package pull

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

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// ociConfigBody builds a valid OCI image config blob whose Entrypoint carries
// the given marker, so distinct markers yield distinct content digests. A
// composed pin binds by the serialization-agnostic config CONTENT digest.
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
// the value a composed pin attests and pull verifies.
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

// reserialize returns config bytes that add serialization-only noise to
// configBody while preserving every execution-relevant field, mimicking the
// containerd store's push-time config re-encode. Its content digest equals
// configBody's; its raw sha256 differs.
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

// seedImage pushes a minimal single-config, single-layer OCI image whose config
// blob is configBody into st and returns the image manifest descriptor. A
// composed pin binds by the config content digest; a foreign-base pin by the
// manifest digest.
func seedImage(t *testing.T, st content.Storage, configBody []byte) ocispec.Descriptor {
	t.Helper()
	ctx := context.Background()
	cfg := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, configBody)
	if err := st.Push(ctx, cfg, bytes.NewReader(configBody)); err != nil {
		t.Fatalf("push config: %v", err)
	}
	layerBody := []byte("layer-" + string(cfg.Digest))
	layer := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayerGzip, layerBody)
	if err := st.Push(ctx, layer, bytes.NewReader(layerBody)); err != nil {
		t.Fatalf("push layer: %v", err)
	}
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
	if err := st.Push(ctx, md, bytes.NewReader(mb)); err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	// A real registry resolves a pushed manifest by its digest (a HEAD on the
	// manifest); the in-memory store only resolves references it was tagged with,
	// so tag the manifest under its own digest to mirror registry behavior for
	// the manifest-digest binding's Resolve(pin.Digest) lookup.
	if tagger, ok := st.(interface {
		Tag(context.Context, ocispec.Descriptor, string) error
	}); ok {
		if err := tagger.Tag(ctx, md, md.Digest.String()); err != nil {
			t.Fatalf("tag manifest by digest: %v", err)
		}
	}
	return md
}

const testRegistry = "example.com/acme/plan"

// seedComposed seeds a composed image (config blob = configBody) tagged where
// PullImage's composed branch resolves it, and returns a pin whose attested
// Digest is the config content digest plus the image manifest descriptor.
func seedComposed(t *testing.T, st *memory.Store, versionTag string, configBody []byte) (freeze.ImagePin, ocispec.Descriptor) {
	t.Helper()
	manifest := seedImage(t, st, configBody)
	if err := st.Tag(context.Background(), manifest, freeze.ComposedImageTag(versionTag)); err != nil {
		t.Fatalf("tag composed image: %v", err)
	}
	pin := freeze.ImagePin{
		Ref:      "aileron/sandbox-tools+tools(gh)",
		Digest:   contentDigest(t, configBody),
		LocalTag: "aileron/sandbox-tools:abc123",
	}
	return pin, manifest
}

// seedComposedIndex publishes the composed image as an OCI image index (the
// shape Docker Desktop's containerd store emits): the single-platform image
// manifest plus a buildkit attestation manifest, tagged where PullImage's
// composed branch resolves it. It returns the composed pin and the index
// descriptor (the value BootRef must anchor to).
func seedComposedIndex(t *testing.T, st *memory.Store, versionTag string, configBody []byte) (freeze.ImagePin, ocispec.Descriptor) {
	t.Helper()
	ctx := context.Background()
	image := seedImage(t, st, configBody)
	image.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}

	// Selection skips the attestation, so its blobs are never fetched; craft the
	// descriptor without pushing (the shared layer would collide on a second push).
	att := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, []byte("attestation-"+string(image.Digest)))
	att.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	att.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}

	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{image, att},
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	id := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, ib)
	if err := st.Push(ctx, id, bytes.NewReader(ib)); err != nil {
		t.Fatalf("push index: %v", err)
	}
	if err := st.Tag(ctx, id, freeze.ComposedImageTag(versionTag)); err != nil {
		t.Fatalf("tag composed index: %v", err)
	}
	pin := freeze.ImagePin{
		Ref:      "aileron/sandbox-tools+tools(gh)",
		Digest:   contentDigest(t, configBody),
		LocalTag: "aileron/sandbox-tools:abc123",
	}
	return pin, id
}

// TestPullImage_ComposedOverOCIIndex is the consumer half of the containerd
// regression: `skill install`+launch resolves the same published composed tag,
// which is an OCI index on a containerd store. The content-digest read-back must
// unwrap the index and verify, and BootRef must anchor to the resolved index
// digest (what Docker/containerd boot correctly).
func TestPullImage_ComposedOverOCIIndex(t *testing.T) {
	st := memory.New()
	pin, index := seedComposedIndex(t, st, "v1abc", ociConfigBody(t, "composed"))

	got, err := PullImage(context.Background(), ImagePullOptions{
		Source:     st,
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        pin,
	})
	if err != nil {
		t.Fatalf("PullImage over an OCI-index composed image: %v", err)
	}
	if got.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", got.BindingKind, freeze.BindingConfigContentDigest)
	}
	wantRef := testRegistry + "@" + index.Digest.String()
	if got.BootRef != wantRef {
		t.Errorf("boot ref = %q, want the content-addressed index ref %q", got.BootRef, wantRef)
	}
	if got.ImageDigest != pin.Digest {
		t.Errorf("image digest = %q, want the config content digest %q", got.ImageDigest, pin.Digest)
	}
}

// TestPullImage_ComposedReserializedConfigInstalls is the #2014 core consumer
// regression: a clean-machine install of a composed image whose PUBLISHED config
// blob was re-serialized on push (byte-different blob, identical runtime fields)
// verifies and returns a bootable ref, because the binding is content-based.
func TestPullImage_ComposedReserializedConfigInstalls(t *testing.T) {
	st := memory.New()
	honest := ociConfigBody(t, "composed")
	// The registry serves the re-serialized config; the lock still attests the
	// honest content digest.
	pin, manifest := seedComposed(t, st, "v1abc", reserialize(t, honest))
	pin.Digest = contentDigest(t, honest)

	got, err := PullImage(context.Background(), ImagePullOptions{
		Source:     st,
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        pin,
	})
	if err != nil {
		t.Fatalf("PullImage over a re-serialized composed config must succeed: %v", err)
	}
	if got.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", got.BindingKind, freeze.BindingConfigContentDigest)
	}
	if got.BootRef != testRegistry+"@"+manifest.Digest.String() {
		t.Errorf("boot ref = %q, want the content-addressed manifest ref", got.BootRef)
	}
}

// TestPullImage_ComposedTamperedConfigRejected proves the integrity guarantee:
// a registry serving a composed image whose config has a changed
// execution-relevant field (Entrypoint) is refused, never booted.
func TestPullImage_ComposedTamperedConfigRejected(t *testing.T) {
	st := memory.New()
	honest := ociConfigBody(t, "composed")
	// Serve a config with a tampered Entrypoint but attest the honest digest.
	var img ocispec.Image
	if err := json.Unmarshal(honest, &img); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	img.Config.Entrypoint = []string{"/evil"}
	tampered, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal tampered: %v", err)
	}
	pin, _ := seedComposed(t, st, "v1abc", tampered)
	pin.Digest = contentDigest(t, honest)

	got, err := PullImage(context.Background(), ImagePullOptions{
		Source:     st,
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        pin,
	})
	if !errors.Is(err, ErrImageDigestMismatch) {
		t.Fatalf("err = %v, want ErrImageDigestMismatch", err)
	}
	if got.BootRef != "" {
		t.Errorf("a tampered config must return no bootable ref, got %q", got.BootRef)
	}
}

// TestPullImage_ComposedIndexNoRunnableManifestFailsClosed proves an index with
// no runnable image manifest (only an attestation) fails closed with an
// actionable message rather than booting an unverifiable image.
func TestPullImage_ComposedIndexNoRunnableManifestFailsClosed(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	att := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, []byte("attestation-only"))
	att.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	att.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{att},
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	id := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, ib)
	if err := st.Push(ctx, id, bytes.NewReader(ib)); err != nil {
		t.Fatalf("push index: %v", err)
	}
	if err := st.Tag(ctx, id, freeze.ComposedImageTag("v1abc")); err != nil {
		t.Fatalf("tag: %v", err)
	}
	pin := freeze.ImagePin{Ref: "r", Digest: "sha256:" + strings.Repeat("a", 64), LocalTag: "t"}
	_, err = PullImage(ctx, ImagePullOptions{
		Source: st, Registry: testRegistry, VersionTag: "v1abc", Pin: pin,
	})
	if !errors.Is(err, ErrImagePullFailed) {
		t.Fatalf("err = %v, want ErrImagePullFailed", err)
	}
	if !strings.Contains(err.Error(), "runnable image manifest") {
		t.Errorf("err = %v, want an actionable no-runnable-manifest message", err)
	}
}

func TestPullImage_ComposedMatchReturnsBootableRef(t *testing.T) {
	st := memory.New()
	pin, manifest := seedComposed(t, st, "v1abc", ociConfigBody(t, "composed"))

	got, err := PullImage(context.Background(), ImagePullOptions{
		Source:     st,
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        pin,
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if got.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", got.BindingKind, freeze.BindingConfigContentDigest)
	}
	// The boot ref is content-addressed to the resolved MANIFEST digest, not the
	// mutable tag: this closes the TOCTOU a tag would leave between verification
	// and boot. The config-content binding is still the identity check.
	wantRef := testRegistry + "@" + manifest.Digest.String()
	if got.BootRef != wantRef {
		t.Errorf("boot ref = %q, want the content-addressed manifest ref %q", got.BootRef, wantRef)
	}
	if got.ImageDigest != pin.Digest {
		t.Errorf("image digest = %q, want the config content digest %q", got.ImageDigest, pin.Digest)
	}
}

func TestPullImage_ComposedMismatchFailsClosed(t *testing.T) {
	st := memory.New()
	pin, _ := seedComposed(t, st, "v1abc", ociConfigBody(t, "composed"))
	// The lock attests a DIFFERENT config content digest than the seeded image.
	pin.Digest = "sha256:" + strings.Repeat("f", 64)

	got, err := PullImage(context.Background(), ImagePullOptions{
		Source:     st,
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        pin,
	})
	if !errors.Is(err, ErrImageDigestMismatch) {
		t.Fatalf("err = %v, want ErrImageDigestMismatch", err)
	}
	if got.BootRef != "" {
		t.Errorf("a mismatch must return no bootable ref, got %q", got.BootRef)
	}
}

func TestPullImage_ComposedMissingIsErrImageMissing(t *testing.T) {
	st := memory.New() // nothing seeded under the composed tag
	pin := freeze.ImagePin{
		Ref:      "aileron/sandbox-tools",
		Digest:   "sha256:" + strings.Repeat("a", 64),
		LocalTag: "aileron/sandbox-tools:abc",
	}
	_, err := PullImage(context.Background(), ImagePullOptions{
		Source:     st,
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        pin,
	})
	if !errors.Is(err, ErrImageMissing) {
		t.Fatalf("err = %v, want ErrImageMissing", err)
	}
}

func TestPullImage_ManifestDigestMatchReturnsContentAddressedRef(t *testing.T) {
	st := memory.New()
	manifest := seedImage(t, st, ociConfigBody(t, "foreign-base"))
	// An image-only/foreign-base pin: no LocalTag, Digest = the manifest digest.
	pin := freeze.ImagePin{Ref: "registry.example.com/runner:1.4", Digest: manifest.Digest.String()}

	got, err := PullImage(context.Background(), ImagePullOptions{
		Source:     st,
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        pin,
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if got.BindingKind != freeze.BindingManifestDigest {
		t.Errorf("binding = %q, want %q", got.BindingKind, freeze.BindingManifestDigest)
	}
	wantRef := testRegistry + "@" + manifest.Digest.String()
	if got.BootRef != wantRef {
		t.Errorf("boot ref = %q, want %q", got.BootRef, wantRef)
	}
}

func TestPullImage_ManifestDigestMissingIsErrImageMissing(t *testing.T) {
	st := memory.New() // nothing seeded
	pin := freeze.ImagePin{
		Ref:    "registry.example.com/runner:1.4",
		Digest: "sha256:" + strings.Repeat("b", 64),
	}
	_, err := PullImage(context.Background(), ImagePullOptions{
		Source:     st,
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        pin,
	})
	if !errors.Is(err, ErrImageMissing) {
		t.Fatalf("err = %v, want ErrImageMissing", err)
	}
}

// TestPullImage_BindingDerivedFromPinNotAnnotation proves the verification path
// is governed by freeze.BindingKind(pin), never any registry annotation: a
// composed pin (LocalTag set) always verifies by config content digest even
// though the image manifest carries no binding annotation at all, and an
// image-only pin (LocalTag empty) always verifies by manifest digest. Tampering
// with the annotation cannot flip the binding because the binding is a function
// of the signed pin, which the tests here supply directly.
func TestPullImage_BindingDerivedFromPinNotAnnotation(t *testing.T) {
	// Composed pin over a store whose image manifest has NO annotation.
	stComposed := memory.New()
	composedPin, _ := seedComposed(t, stComposed, "v1abc", ociConfigBody(t, "cfg"))
	if freeze.BindingKind(composedPin) != freeze.BindingConfigContentDigest {
		t.Fatalf("composed pin binding = %q", freeze.BindingKind(composedPin))
	}
	got, err := PullImage(context.Background(), ImagePullOptions{
		Source: stComposed, Registry: testRegistry, VersionTag: "v1abc", Pin: composedPin,
	})
	if err != nil {
		t.Fatalf("composed PullImage: %v", err)
	}
	if got.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("composed binding = %q, want config-content-digest regardless of annotation", got.BindingKind)
	}

	// Image-only pin (LocalTag empty): must bind by manifest digest, even though
	// the exact same underlying image bytes were used above.
	stForeign := memory.New()
	manifest := seedImage(t, stForeign, ociConfigBody(t, "cfg"))
	foreignPin := freeze.ImagePin{Ref: "registry.example.com/runner:1.4", Digest: manifest.Digest.String()}
	if freeze.BindingKind(foreignPin) != freeze.BindingManifestDigest {
		t.Fatalf("foreign pin binding = %q", freeze.BindingKind(foreignPin))
	}
	got, err = PullImage(context.Background(), ImagePullOptions{
		Source: stForeign, Registry: testRegistry, VersionTag: "v1abc", Pin: foreignPin,
	})
	if err != nil {
		t.Fatalf("foreign PullImage: %v", err)
	}
	if got.BindingKind != freeze.BindingManifestDigest {
		t.Errorf("foreign binding = %q, want manifest-digest", got.BindingKind)
	}
}

func TestPullImage_NoDigestFailsClosed(t *testing.T) {
	_, err := PullImage(context.Background(), ImagePullOptions{
		Source:     memory.New(),
		Registry:   testRegistry,
		VersionTag: "v1abc",
		Pin:        freeze.ImagePin{Ref: "r", LocalTag: "t"}, // no Digest
	})
	if !errors.Is(err, ErrImageDigestMismatch) {
		t.Fatalf("err = %v, want ErrImageDigestMismatch for a digest-less pin", err)
	}
}

func TestPullImage_NoRegistryFailsClosed(t *testing.T) {
	_, err := PullImage(context.Background(), ImagePullOptions{
		Source:     memory.New(),
		Registry:   "",
		VersionTag: "v1abc",
		Pin:        freeze.ImagePin{Digest: "sha256:x", LocalTag: "t"},
	})
	if !errors.Is(err, ErrImagePullFailed) {
		t.Fatalf("err = %v, want ErrImagePullFailed for a missing registry", err)
	}
}

// resolveErrTarget resolves every reference with a non-not-found error, standing
// in for an unreachable/unauthenticated registry (NOT a missing image).
type resolveErrTarget struct {
	*memory.Store
	err error
}

func (r resolveErrTarget) Resolve(context.Context, string) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{}, r.err
}

func TestPullImage_ComposedResolveErrorIsPullFailed(t *testing.T) {
	boom := errors.New("dial tcp: connection refused")
	src := resolveErrTarget{Store: memory.New(), err: boom}
	pin := freeze.ImagePin{Ref: "r", Digest: "sha256:" + strings.Repeat("a", 64), LocalTag: "t"}
	_, err := PullImage(context.Background(), ImagePullOptions{
		Source: src, Registry: testRegistry, VersionTag: "v1abc", Pin: pin,
	})
	if !errors.Is(err, ErrImagePullFailed) {
		t.Fatalf("err = %v, want ErrImagePullFailed", err)
	}
	if errors.Is(err, ErrImageMissing) {
		t.Errorf("a network error must NOT be classified as ErrImageMissing: %v", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the underlying resolve error inspectable", err)
	}
}

// composedFetchFailTarget resolves the composed tag but fails to fetch the
// manifest bytes, standing in for a registry that serves a descriptor it then
// cannot back with content.
type composedFetchFailTarget struct {
	*memory.Store
	err error
}

func (c composedFetchFailTarget) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return nil, c.err
}

func TestPullImage_ComposedConfigReadErrorIsPullFailed(t *testing.T) {
	inner := memory.New()
	pin, _ := seedComposed(t, inner, "v1abc", ociConfigBody(t, "cfg"))
	boom := errors.New("manifest read reset")
	src := composedFetchFailTarget{Store: inner, err: boom}
	_, err := PullImage(context.Background(), ImagePullOptions{
		Source: src, Registry: testRegistry, VersionTag: "v1abc", Pin: pin,
	})
	if !errors.Is(err, ErrImagePullFailed) {
		t.Fatalf("err = %v, want ErrImagePullFailed", err)
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("err = %v, want a config-read message", err)
	}
}

func TestPullImage_ComposedManifestWithoutConfigDigestFailsClosed(t *testing.T) {
	// A composed image tag pointing at a manifest with an empty config digest
	// cannot be verified by config content digest; the read fails closed rather
	// than booting an unverifiable image.
	st := memory.New()
	ctx := context.Background()
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		// Config left zero: no digest.
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	md := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, mb)
	if err := st.Push(ctx, md, bytes.NewReader(mb)); err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	if err := st.Tag(ctx, md, freeze.ComposedImageTag("v1abc")); err != nil {
		t.Fatalf("tag: %v", err)
	}
	pin := freeze.ImagePin{Ref: "r", Digest: "sha256:" + strings.Repeat("a", 64), LocalTag: "t"}
	_, err = PullImage(ctx, ImagePullOptions{
		Source: st, Registry: testRegistry, VersionTag: "v1abc", Pin: pin,
	})
	if !errors.Is(err, ErrImagePullFailed) {
		t.Fatalf("err = %v, want ErrImagePullFailed for a config-less manifest", err)
	}
}

func TestPullImage_ManifestResolveErrorIsPullFailed(t *testing.T) {
	boom := errors.New("registry 500")
	src := resolveErrTarget{Store: memory.New(), err: boom}
	pin := freeze.ImagePin{Ref: "r", Digest: "sha256:" + strings.Repeat("a", 64)}
	_, err := PullImage(context.Background(), ImagePullOptions{
		Source: src, Registry: testRegistry, VersionTag: "v1abc", Pin: pin,
	})
	if !errors.Is(err, ErrImagePullFailed) {
		t.Fatalf("err = %v, want ErrImagePullFailed", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the underlying resolve error inspectable", err)
	}
}
