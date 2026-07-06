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

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// pushBytes pushes data under mediaType and returns its descriptor.
func pushBytes(t *testing.T, st content.Storage, mediaType string, data []byte) ocispec.Descriptor {
	t.Helper()
	desc := content.NewDescriptorFromBytes(mediaType, data)
	if err := st.Push(context.Background(), desc, bytes.NewReader(data)); err != nil {
		t.Fatalf("push %s: %v", mediaType, err)
	}
	return desc
}

// seedManifest pushes a minimal single-config image manifest and returns its
// descriptor plus its config descriptor.
func seedManifest(t *testing.T, st content.Storage, configBody string) (manifest, config ocispec.Descriptor) {
	t.Helper()
	cfg := pushBytes(t, st, ocispec.MediaTypeImageConfig, []byte(configBody))
	layer := pushBytes(t, st, ocispec.MediaTypeImageLayerGzip, []byte("layer-"+configBody))
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
	return md, cfg
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

func TestConfigDigest_ClassicSingleManifest(t *testing.T) {
	st := memory.New()
	manifest, cfg := seedManifest(t, st, "classic-config")
	got, err := ConfigDigest(context.Background(), st, manifest)
	if err != nil {
		t.Fatalf("ConfigDigest: %v", err)
	}
	if got != cfg.Digest.String() {
		t.Errorf("config digest = %q, want %q", got, cfg.Digest)
	}
}

func TestConfigDigest_OCIIndexUnwrapsToImageManifest(t *testing.T) {
	// The containerd store behind Docker Desktop emits an OCI index wrapping the
	// single-platform image manifest plus a buildkit attestation. The config
	// digest must resolve to the image manifest's config, not error out.
	st := memory.New()
	image, cfg := seedManifest(t, st, "composed-config")
	image.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	att := attestationManifest(t, st)
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, image, att)

	got, err := ConfigDigest(context.Background(), st, idx)
	if err != nil {
		t.Fatalf("ConfigDigest over index: %v", err)
	}
	if got != cfg.Digest.String() {
		t.Errorf("config digest = %q, want the wrapped image manifest's config %q", got, cfg.Digest)
	}
}

func TestConfigDigest_DockerManifestListUnwraps(t *testing.T) {
	// The Docker schema2 manifest-list media type must be treated as an index too.
	st := memory.New()
	image, cfg := seedManifest(t, st, "docker-list-config")
	att := attestationManifest(t, st)
	idx := pushIndex(t, st, dockerManifestListMediaType, image, att)

	got, err := ConfigDigest(context.Background(), st, idx)
	if err != nil {
		t.Fatalf("ConfigDigest over docker manifest list: %v", err)
	}
	if got != cfg.Digest.String() {
		t.Errorf("config digest = %q, want %q", got, cfg.Digest)
	}
}

func TestConfigDigest_IndexWithOnlyAttestationErrors(t *testing.T) {
	// An index carrying no runnable image manifest cannot yield a config digest;
	// the helper must fail closed with an actionable message.
	st := memory.New()
	att := attestationManifest(t, st)
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, att)

	_, err := ConfigDigest(context.Background(), st, idx)
	if err == nil {
		t.Fatal("want an error for an index with no runnable image manifest")
	}
	if !strings.Contains(err.Error(), "runnable image manifest") {
		t.Errorf("err = %v, want an actionable no-runnable-manifest message", err)
	}
}

func TestConfigDigest_MultiPlatformMatchesHost(t *testing.T) {
	st := memory.New()
	// A foreign-platform manifest plus the host-platform manifest; the host one wins.
	foreign, _ := seedManifest(t, st, "foreign-arch-config")
	foreign.Platform = &ocispec.Platform{OS: "plan9", Architecture: "mips"}
	host, hostCfg := seedManifest(t, st, "host-arch-config")
	host.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, foreign, host)

	got, err := ConfigDigest(context.Background(), st, idx)
	if err != nil {
		t.Fatalf("ConfigDigest: %v", err)
	}
	if got != hostCfg.Digest.String() {
		t.Errorf("config digest = %q, want the host-platform config %q", got, hostCfg.Digest)
	}
}

func TestConfigDigest_MultiPlatformNoHostMatchErrors(t *testing.T) {
	st := memory.New()
	a, _ := seedManifest(t, st, "arch-a")
	a.Platform = &ocispec.Platform{OS: "plan9", Architecture: "mips"}
	b, _ := seedManifest(t, st, "arch-b")
	b.Platform = &ocispec.Platform{OS: "solaris", Architecture: "sparc64"}
	idx := pushIndex(t, st, ocispec.MediaTypeImageIndex, a, b)

	_, err := ConfigDigest(context.Background(), st, idx)
	if err == nil {
		t.Fatal("want an error when no manifest matches the host platform")
	}
	if !strings.Contains(err.Error(), "host platform") {
		t.Errorf("err = %v, want a host-platform mismatch message", err)
	}
}

func TestConfigDigest_ManifestWithoutConfigErrors(t *testing.T) {
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
	if _, err := ConfigDigest(context.Background(), st, md); err == nil || !strings.Contains(err.Error(), "no config digest") {
		t.Fatalf("err = %v, want a no-config-digest error", err)
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

func TestConfigDigest_FetchErrorPropagates(t *testing.T) {
	boom := errors.New("connection reset")
	st := fetchFailStore{Store: memory.New(), err: boom}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, []byte("{}"))
	if _, err := ConfigDigest(context.Background(), st, desc); !errors.Is(err, boom) {
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

func TestConfigDigest_IndexChildFetchErrorPropagates(t *testing.T) {
	inner := memory.New()
	image, _ := seedManifest(t, inner, "child-config")
	image.Platform = &ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	idx := pushIndex(t, inner, ocispec.MediaTypeImageIndex, image)
	boom := errors.New("child manifest read reset")
	st := indexFetchFailStore{Store: inner, failDigest: image.Digest.String(), err: boom}

	_, err := ConfigDigest(context.Background(), st, idx)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the child fetch error", err)
	}
	if !strings.Contains(err.Error(), "platform image manifest") {
		t.Errorf("err = %v, want a platform-image-manifest fetch message", err)
	}
}

// rawFetcher returns fixed bytes for any descriptor, letting a test drive
// ConfigDigest with content a validating store (memory.Store parses index JSON
// on Push) would reject.
type rawFetcher struct{ data []byte }

func (r rawFetcher) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(r.data)), nil
}

func TestConfigDigest_CorruptIndexErrors(t *testing.T) {
	bad := []byte("not json")
	// Describe the bytes as an index so ConfigDigest takes the unwrap path, then
	// serve them raw; content.FetchAll verifies the digest, and the decode fails.
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, bad)
	if _, err := ConfigDigest(context.Background(), rawFetcher{bad}, desc); err == nil || !strings.Contains(err.Error(), "decode image index") {
		t.Fatalf("err = %v, want a decode-image-index error", err)
	}
}
