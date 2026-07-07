package ociremote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// withHostGOARCH overrides the hostGOARCH selection seam for the duration of the
// test, so a test can exercise the foreign-arch child-selection branch
// deterministically regardless of the arch the test binary runs on. It mirrors
// freeze's withHostGOARCH helper.
func withHostGOARCH(t *testing.T, arch string) {
	t.Helper()
	prev := hostGOARCH
	hostGOARCH = arch
	t.Cleanup(func() { hostGOARCH = prev })
}

// ociConfigBody builds a valid OCI image config blob whose Entrypoint carries
// the given marker, so distinct markers yield distinct content digests. The
// returned bytes are what a registry serves as the config blob.
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
	img.RootFS.DiffIDs = nil
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal oci config: %v", err)
	}
	return b
}

// wantContentDigest is the content digest ConfigContentDigest must return for a
// config blob, computed through the same imgconfig contract.
func wantContentDigest(t *testing.T, configBody []byte) string {
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

// pushBytes pushes data under mediaType and returns its descriptor.
func pushBytes(t *testing.T, st content.Storage, mediaType string, data []byte) ocispec.Descriptor {
	t.Helper()
	desc := content.NewDescriptorFromBytes(mediaType, data)
	if err := st.Push(context.Background(), desc, bytes.NewReader(data)); err != nil {
		t.Fatalf("push %s: %v", mediaType, err)
	}
	return desc
}

// seedManifest pushes a minimal single-config image manifest (config blob +
// layer + manifest) and returns the manifest descriptor plus the config bytes.
func seedManifest(t *testing.T, st content.Storage, marker string) (manifest ocispec.Descriptor, configBody []byte) {
	t.Helper()
	configBody = ociConfigBody(t, marker)
	cfg := pushBytes(t, st, ocispec.MediaTypeImageConfig, configBody)
	layer := pushBytes(t, st, ocispec.MediaTypeImageLayerGzip, []byte("layer-"+marker))
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
	md := pushBytes(t, st, ocispec.MediaTypeImageManifest, mb)
	return md, configBody
}

// pushIndex pushes an OCI image index (mediaType selectable) wrapping the given
// child descriptors and returns the index descriptor.
func pushIndex(t *testing.T, st content.Storage, mediaType string, children ...ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: mediaType,
		Manifests: children,
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	return pushBytes(t, st, mediaType, ib)
}

// attestationManifest builds a child descriptor that mimics a buildkit
// attestation entry: unknown/unknown platform + docker attestation annotation.
func attestationManifest(t *testing.T, st content.Storage) ocispec.Descriptor {
	t.Helper()
	// The attestation manifest content is irrelevant to selection; seed a body so
	// a store fetch would succeed if (incorrectly) selected.
	md, _ := seedManifest(t, st, "attestation-body")
	md.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	md.Annotations = map[string]string{dockerReferenceTypeAnnotation: dockerAttestationRefType}
	return md
}

func TestConfigContentDigest_ClassicSingleManifest(t *testing.T) {
	st := memory.New()
	manifest, configBody := seedManifest(t, st, "classic")
	got, err := ConfigContentDigest(context.Background(), st, manifest)
	if err != nil {
		t.Fatalf("ConfigContentDigest: %v", err)
	}
	if want := wantContentDigest(t, configBody); got != want {
		t.Errorf("content digest = %q, want %q", got, want)
	}
}

func TestConfigContentDigest_OCIIndexUnwrapsToImageManifest(t *testing.T) {
	// The containerd store behind Docker Desktop emits an OCI index wrapping the
	// single-platform image manifest plus a buildkit attestation. The content
	// digest must resolve to the image manifest's config, not error out.
	st := memory.New()
	image, configBody := seedManifest(t, st, "composed")
	image.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	att := attestationManifest(t, st)
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, image, att)

	got, err := ConfigContentDigest(context.Background(), st, idx)
	if err != nil {
		t.Fatalf("ConfigContentDigest over index: %v", err)
	}
	if want := wantContentDigest(t, configBody); got != want {
		t.Errorf("content digest = %q, want the wrapped image manifest's config %q", got, want)
	}
}

func TestConfigContentDigest_DockerManifestListUnwraps(t *testing.T) {
	// The Docker schema2 manifest-list media type must be treated as an index too.
	st := memory.New()
	image, configBody := seedManifest(t, st, "docker-list")
	att := attestationManifest(t, st)
	idx := pushIndex(t, st, dockerManifestListMediaType, image, att)

	got, err := ConfigContentDigest(context.Background(), st, idx)
	if err != nil {
		t.Fatalf("ConfigContentDigest over docker manifest list: %v", err)
	}
	if want := wantContentDigest(t, configBody); got != want {
		t.Errorf("content digest = %q, want %q", got, want)
	}
}

// TestConfigContentDigest_ReserializedConfigStable is the #2014 core: a config
// blob re-serialized on push (different blob bytes, byte-identical runtime
// fields) yields the SAME content digest, so publish/pull do not spuriously
// report tampering.
func TestConfigContentDigest_ReserializedConfigStable(t *testing.T) {
	st := memory.New()
	// Build two config blobs with the same execution-relevant fields but with
	// serialization-only noise added to one, mimicking the containerd re-encode.
	base := ociConfigBody(t, "same")
	var img ocispec.Image
	if err := json.Unmarshal(base, &img); err != nil {
		t.Fatalf("unmarshal base: %v", err)
	}
	img.Author = "buildkit"
	img.History = []ocispec.History{{CreatedBy: "RUN noise"}}
	reserialized, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal reserialized: %v", err)
	}
	if bytes.Equal(base, reserialized) {
		t.Fatal("test bug: re-serialized bytes are identical to the base")
	}

	seed := func(body []byte, layerTag string) ocispec.Descriptor {
		cfg := pushBytes(t, st, ocispec.MediaTypeImageConfig, body)
		layer := pushBytes(t, st, ocispec.MediaTypeImageLayerGzip, []byte("layer-"+layerTag))
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
		return pushBytes(t, st, ocispec.MediaTypeImageManifest, mb)
	}

	gotBase, err := ConfigContentDigest(context.Background(), st, seed(base, "base"))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	gotReser, err := ConfigContentDigest(context.Background(), st, seed(reserialized, "reser"))
	if err != nil {
		t.Fatalf("reserialized: %v", err)
	}
	if gotBase != gotReser {
		t.Errorf("re-serialized config produced a different content digest: %q vs %q", gotBase, gotReser)
	}
}

func TestConfigContentDigest_IndexWithOnlyAttestationErrors(t *testing.T) {
	// An index carrying no runnable image manifest cannot yield a content digest;
	// the helper must fail closed with an actionable message.
	st := memory.New()
	att := attestationManifest(t, st)
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, att)

	_, err := ConfigContentDigest(context.Background(), st, idx)
	if err == nil {
		t.Fatal("want an error for an index with no runnable image manifest")
	}
	if !strings.Contains(err.Error(), "runnable image manifest") {
		t.Errorf("err = %v, want an actionable no-runnable-manifest message", err)
	}
}

func TestConfigContentDigest_MultiPlatformMatchesHost(t *testing.T) {
	st := memory.New()
	// A foreign-platform manifest plus the host-platform manifest; the host one wins.
	foreign, _ := seedManifest(t, st, "foreign-arch")
	foreign.Platform = &ocispec.Platform{OS: "plan9", Architecture: "mips"}
	host, hostBody := seedManifest(t, st, "host-arch")
	host.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, foreign, host)

	got, err := ConfigContentDigest(context.Background(), st, idx)
	if err != nil {
		t.Fatalf("ConfigContentDigest: %v", err)
	}
	if want := wantContentDigest(t, hostBody); got != want {
		t.Errorf("content digest = %q, want the host-platform config %q", got, want)
	}
}

// TestConfigContentDigest_SelectsForeignArchChild pins the #2025 contract: the
// single-child selection picks the manifest for the running arch (the hostGOARCH
// seam), not some other child. A cross-arch consumer's per-arch verify reads the
// child it actually attests, matching freeze.hostPlatform()'s arch vocabulary.
// Both children share the host GOOS (composed images are linux-only) so only the
// arch distinguishes them. Production sets hostGOARCH from runtime.GOARCH; a
// genuinely foreign consumer (e.g. an arm64 host) selects its own child naturally.
// The e2e runs host-native (the orchestrator must exec native-arch build tooling),
// so this seam is where the foreign-arch selection is driven deterministically —
// including arches no test runner or emulator provides.
func TestConfigContentDigest_SelectsForeignArchChild(t *testing.T) {
	st := memory.New()
	native, nativeBody := seedManifest(t, st, "native-arch")
	native.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	const foreignArch = "s390x" // a stable arch distinct from any test runner's GOARCH
	foreign, foreignBody := seedManifest(t, st, "foreign-arch")
	foreign.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: foreignArch}
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, native, foreign)

	t.Run("a foreign-arch consumer selects the foreign child", func(t *testing.T) {
		withHostGOARCH(t, foreignArch)
		got, err := ConfigContentDigest(context.Background(), st, idx)
		if err != nil {
			t.Fatalf("ConfigContentDigest as %s: %v", foreignArch, err)
		}
		if want := wantContentDigest(t, foreignBody); got != want {
			t.Errorf("content digest = %q, want the foreign %s child's config %q", got, foreignArch, want)
		}
	})
	t.Run("the native-arch consumer selects the native child", func(t *testing.T) {
		withHostGOARCH(t, runtime.GOARCH)
		got, err := ConfigContentDigest(context.Background(), st, idx)
		if err != nil {
			t.Fatalf("ConfigContentDigest as the native arch: %v", err)
		}
		if want := wantContentDigest(t, nativeBody); got != want {
			t.Errorf("content digest = %q, want the native child's config %q", got, want)
		}
	})
	t.Run("an arch absent from the index fails closed", func(t *testing.T) {
		withHostGOARCH(t, "ppc64le")
		_, err := ConfigContentDigest(context.Background(), st, idx)
		if err == nil {
			t.Fatal("want an error when the running arch is not in the index")
		}
		if !strings.Contains(err.Error(), "ppc64le") {
			t.Errorf("err = %v, want the running arch named in the fail-closed message", err)
		}
	})
}

func TestConfigContentDigest_MultiPlatformNoHostMatchErrors(t *testing.T) {
	st := memory.New()
	a, _ := seedManifest(t, st, "arch-a")
	a.Platform = &ocispec.Platform{OS: "plan9", Architecture: "mips"}
	b, _ := seedManifest(t, st, "arch-b")
	b.Platform = &ocispec.Platform{OS: "solaris", Architecture: "sparc64"}
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, a, b)

	_, err := ConfigContentDigest(context.Background(), st, idx)
	if err == nil {
		t.Fatal("want an error when no manifest matches the host platform")
	}
	if !strings.Contains(err.Error(), "host platform") {
		t.Errorf("err = %v, want a host-platform mismatch message", err)
	}
}

func TestConfigContentDigest_ManifestWithoutConfigErrors(t *testing.T) {
	st := memory.New()
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		// Config left zero.
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	md := pushBytes(t, st, ocispec.MediaTypeImageManifest, mb)
	if _, err := ConfigContentDigest(context.Background(), st, md); err == nil || !strings.Contains(err.Error(), "no config digest") {
		t.Fatalf("err = %v, want a no-config-digest error", err)
	}
}

func TestConfigContentDigest_InvalidConfigBlobErrors(t *testing.T) {
	// A manifest whose config blob is not valid image-config JSON fails closed
	// rather than hashing garbage.
	st := memory.New()
	cfg := pushBytes(t, st, ocispec.MediaTypeImageConfig, []byte("not json"))
	layer := pushBytes(t, st, ocispec.MediaTypeImageLayerGzip, []byte("layer"))
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfg,
		Layers:    []ocispec.Descriptor{layer},
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	md := pushBytes(t, st, ocispec.MediaTypeImageManifest, mb)
	if _, err := ConfigContentDigest(context.Background(), st, md); err == nil || !strings.Contains(err.Error(), "image config") {
		t.Fatalf("err = %v, want an image-config decode error", err)
	}
}

// fetchFailStore fails every Fetch, standing in for a store that serves a
// descriptor it cannot back with content.
type fetchFailStore struct {
	*memory.Store
	err error
}

func (f fetchFailStore) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return nil, f.err
}

func TestConfigContentDigest_FetchErrorPropagates(t *testing.T) {
	boom := errors.New("connection reset")
	st := fetchFailStore{Store: memory.New(), err: boom}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, []byte("{}"))
	if _, err := ConfigContentDigest(context.Background(), st, desc); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the underlying fetch error", err)
	}
}

// indexFetchFailStore serves the index bytes but fails to fetch the wrapped
// child manifest, standing in for a partial/corrupt index push.
type indexFetchFailStore struct {
	*memory.Store
	failDigest string
	err        error
}

func (s indexFetchFailStore) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.Digest.String() == s.failDigest {
		return nil, s.err
	}
	return s.Store.Fetch(ctx, desc)
}

func TestConfigContentDigest_IndexChildFetchErrorPropagates(t *testing.T) {
	inner := memory.New()
	image, _ := seedManifest(t, inner, "child")
	image.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	idx := pushIndex(t, inner, ocispec.MediaTypeImageIndex, image)
	boom := errors.New("child manifest read reset")
	st := indexFetchFailStore{Store: inner, failDigest: image.Digest.String(), err: boom}

	_, err := ConfigContentDigest(context.Background(), st, idx)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the child fetch error", err)
	}
	if !strings.Contains(err.Error(), "platform image manifest") {
		t.Errorf("err = %v, want a platform-image-manifest fetch message", err)
	}
}

// configFetchFailStore serves the manifest but fails to fetch the config blob.
type configFetchFailStore struct {
	*memory.Store
	failDigest string
	err        error
}

func (s configFetchFailStore) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.Digest.String() == s.failDigest {
		return nil, s.err
	}
	return s.Store.Fetch(ctx, desc)
}

func TestConfigContentDigest_ConfigBlobFetchErrorPropagates(t *testing.T) {
	inner := memory.New()
	manifest, configBody := seedManifest(t, inner, "cfg-fetch")
	configDigest := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, configBody).Digest.String()
	boom := errors.New("config blob read reset")
	st := configFetchFailStore{Store: inner, failDigest: configDigest, err: boom}

	_, err := ConfigContentDigest(context.Background(), st, manifest)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the config-blob fetch error", err)
	}
	if !strings.Contains(err.Error(), "image config blob") {
		t.Errorf("err = %v, want an image-config-blob fetch message", err)
	}
}

// rawFetcher returns fixed bytes for any descriptor, letting a test drive
// ConfigContentDigest with content a validating store (memory.Store parses index
// JSON on Push) would reject.
type rawFetcher struct{ data []byte }

func (r rawFetcher) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(r.data)), nil
}

func TestConfigContentDigest_CorruptIndexErrors(t *testing.T) {
	bad := []byte("not json")
	// Describe the bytes as an index so ConfigContentDigest takes the unwrap path,
	// then serve them raw; content.FetchAll verifies the digest, and the decode fails.
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, bad)
	if _, err := ConfigContentDigest(context.Background(), rawFetcher{bad}, desc); err == nil || !strings.Contains(err.Error(), "decode image index") {
		t.Fatalf("err = %v, want a decode-image-index error", err)
	}
}

// seedArchManifest pushes a single-config image manifest whose config declares
// the given (os, arch) and returns the manifest descriptor (platform-stamped)
// plus its config bytes, so a test can build a multi-arch index by hand.
func seedArchManifest(t *testing.T, st content.Storage, os, arch, marker string) (manifest ocispec.Descriptor, configBody []byte) {
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
	configBody, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal arch config: %v", err)
	}
	cfg := pushBytes(t, st, ocispec.MediaTypeImageConfig, configBody)
	layer := pushBytes(t, st, ocispec.MediaTypeImageLayerGzip, []byte("layer-"+marker))
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfg,
		Layers:    []ocispec.Descriptor{layer},
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal arch manifest: %v", err)
	}
	md := pushBytes(t, st, ocispec.MediaTypeImageManifest, mb)
	md.Platform = &ocispec.Platform{OS: os, Architecture: arch}
	return md, configBody
}

// TestAllPlatformConfigContentDigests_TwoArchIndex proves the all-arch walk
// returns one content digest per runnable platform from a hand-built two-arch
// index, keyed by each config's own os/arch, with attestation children skipped.
func TestAllPlatformConfigContentDigests_TwoArchIndex(t *testing.T) {
	st := memory.New()
	amd, amdBody := seedArchManifest(t, st, "linux", "amd64", "amd")
	arm, armBody := seedArchManifest(t, st, "linux", "arm64", "arm")
	att := attestationManifest(t, st)
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, amd, arm, att)

	got, err := AllPlatformConfigContentDigests(context.Background(), st, idx)
	if err != nil {
		t.Fatalf("AllPlatformConfigContentDigests: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 platform digests (attestation skipped), got %d: %+v", len(got), got)
	}
	byArch := map[string]PlatformConfigDigest{}
	for _, p := range got {
		if p.OS != "linux" {
			t.Errorf("os = %q, want linux", p.OS)
		}
		byArch[p.Arch] = p
	}
	if want := wantContentDigest(t, amdBody); byArch["amd64"].Digest != want {
		t.Errorf("amd64 digest = %q, want %q", byArch["amd64"].Digest, want)
	}
	if want := wantContentDigest(t, armBody); byArch["arm64"].Digest != want {
		t.Errorf("arm64 digest = %q, want %q", byArch["arm64"].Digest, want)
	}
}

// TestAllPlatformConfigContentDigests_SingleManifest proves a non-index root
// (a plain image manifest) yields exactly one entry keyed by its config's os/arch.
func TestAllPlatformConfigContentDigests_SingleManifest(t *testing.T) {
	st := memory.New()
	md, body := seedArchManifest(t, st, "linux", "arm64", "solo")
	got, err := AllPlatformConfigContentDigests(context.Background(), st, md)
	if err != nil {
		t.Fatalf("AllPlatformConfigContentDigests: %v", err)
	}
	if len(got) != 1 || got[0].OS != "linux" || got[0].Arch != "arm64" {
		t.Fatalf("want a single linux/arm64 entry, got %+v", got)
	}
	if want := wantContentDigest(t, body); got[0].Digest != want {
		t.Errorf("digest = %q, want %q", got[0].Digest, want)
	}
}

// TestAllPlatformConfigContentDigests_OnlyAttestationErrors proves an index with
// no runnable child fails closed with the shared no-runnable-manifest message.
func TestAllPlatformConfigContentDigests_OnlyAttestationErrors(t *testing.T) {
	st := memory.New()
	att := attestationManifest(t, st)
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, att)
	if _, err := AllPlatformConfigContentDigests(context.Background(), st, idx); err == nil || !strings.Contains(err.Error(), "runnable image manifest") {
		t.Fatalf("err = %v, want a no-runnable-manifest error", err)
	}
}

// TestAllPlatformConfigContentDigests_RootIndexFetchError proves a failure
// fetching the root manifest list propagates rather than yielding a partial set.
func TestAllPlatformConfigContentDigests_RootIndexFetchError(t *testing.T) {
	boom := errors.New("index read reset")
	st := fetchFailStore{Store: memory.New(), err: boom}
	idx := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, []byte("{}"))
	if _, err := AllPlatformConfigContentDigests(context.Background(), st, idx); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the root index fetch error", err)
	}
}

// TestAllPlatformConfigContentDigests_ChildManifestFetchError proves a child
// manifest fetch failure surfaces with the platform-image-manifest context.
func TestAllPlatformConfigContentDigests_ChildManifestFetchError(t *testing.T) {
	inner := memory.New()
	amd, _ := seedArchManifest(t, inner, "linux", "amd64", "amd")
	arm, _ := seedArchManifest(t, inner, "linux", "arm64", "arm")
	idx := pushIndex(t, inner, ocispec.MediaTypeImageIndex, amd, arm)
	boom := errors.New("child read reset")
	st := indexFetchFailStore{Store: inner, failDigest: arm.Digest.String(), err: boom}
	_, err := AllPlatformConfigContentDigests(context.Background(), st, idx)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "platform image manifest") {
		t.Fatalf("err = %v, want a platform-image-manifest fetch error", err)
	}
}

// TestAllPlatformConfigContentDigests_InvalidChildConfigErrors proves a child
// whose config blob is not valid image-config JSON fails closed.
func TestAllPlatformConfigContentDigests_InvalidChildConfigErrors(t *testing.T) {
	st := memory.New()
	badCfg := pushBytes(t, st, ocispec.MediaTypeImageConfig, []byte("not json"))
	layer := pushBytes(t, st, ocispec.MediaTypeImageLayerGzip, []byte("layer"))
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    badCfg,
		Layers:    []ocispec.Descriptor{layer},
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	md := pushBytes(t, st, ocispec.MediaTypeImageManifest, mb)
	md.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, md)
	if _, err := AllPlatformConfigContentDigests(context.Background(), st, idx); err == nil || !strings.Contains(err.Error(), "image config") {
		t.Fatalf("err = %v, want an image-config decode error", err)
	}
}

// TestSelectPlatformManifest_HostOnlyUnchanged is the regression guard that
// refactoring selectPlatformManifest into a thin wrapper over
// enumeratePlatformManifests kept the host-only single-pick behavior: a two-arch
// index still resolves ConfigContentDigest to the host-platform config.
func TestSelectPlatformManifest_HostOnlyUnchanged(t *testing.T) {
	st := memory.New()
	foreign, _ := seedArchManifest(t, st, "plan9", "mips", "foreign")
	host, hostBody := seedArchManifest(t, st, runtime.GOOS, runtime.GOARCH, "host")
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, foreign, host)
	got, err := ConfigContentDigest(context.Background(), st, idx)
	if err != nil {
		t.Fatalf("ConfigContentDigest: %v", err)
	}
	if want := wantContentDigest(t, hostBody); got != want {
		t.Errorf("content digest = %q, want the host-platform config %q", got, want)
	}
}
