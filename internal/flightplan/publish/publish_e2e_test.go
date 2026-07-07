//go:build integration_publish

// Package-level e2e for `skill publish` against a REAL OCI registry and a REAL
// docker daemon. Build-tagged (integration_publish) so it never runs in the
// normal unit shard; the dedicated CI job provisions a registry:2 service and
// runs `task test:go:publish-e2e`. Per the repo-family rule these fail fast and
// never t.Skip: a missing prerequisite is a hard failure in the job that is
// supposed to provide it, and every docker/registry call is deadline-bounded so
// a hung daemon fails fast rather than blocking on the outer CI timeout.
package publish

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

// e2eTimeout bounds each publish run and its assertions so a hung registry
// fails fast instead of waiting on the CI job timeout.
const e2eTimeout = 3 * time.Minute

func e2eContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	t.Cleanup(cancel)
	return ctx
}

// registryHost is the real registry under test (e.g. localhost:5000).
func registryHost(t *testing.T) string {
	t.Helper()
	h := os.Getenv("AILERON_TEST_REGISTRY")
	if h == "" {
		t.Fatal("AILERON_TEST_REGISTRY must be set (e.g. localhost:5000) for the publish e2e")
	}
	return h
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// buildLocalImage builds a tiny scratch image locally and returns its tag,
// mirroring how a foreign base is seeded into the registry.
func buildLocalImage(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("aileron-e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	df := "FROM scratch\nADD hello.txt /hello.txt\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	docker(t, "build", "-t", tag, dir)
	return tag
}

// writeSyntheticMultiArchLayout writes a hand-built multi-arch OCI image layout
// into layoutDir: an OCI image-index (manifest list) over two platform children,
// linux/amd64 and linux/arm64, each a real image manifest with its own config and
// layer blob. This is the S4 contract fixture — it is byte-for-byte the artifact
// shape freeze's multi-arch `docker buildx build --output type=oci` writes, but is
// constructed in-process so the push + per-arch re-verify contract is proven
// WITHOUT real buildx, QEMU, the docker-container driver, or cross-arch emulation
// (the real buildx path is reserved for the S5 job that provisions binfmt). It
// mirrors the synthetic-layout helpers #2036 introduced in ociremote's
// ocilayout_test.go (writeBlob / archImageManifest / writeOCILayoutRoot). It
// returns the per-arch config content digests read back from the layout via the
// same production reader freeze uses, so the caller pins exactly what publish will
// re-verify against the registry.
func writeSyntheticMultiArchLayout(t *testing.T, layoutDir string) []ociremote.PlatformConfigDigest {
	t.Helper()
	amd := writeArchImageManifest(t, layoutDir, "linux", "amd64", "amd")
	arm := writeArchImageManifest(t, layoutDir, "linux", "arm64", "arm")
	list := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd, arm},
	}
	lb, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal manifest list: %v", err)
	}
	root := writeLayoutBlob(t, layoutDir, ocispec.MediaTypeImageIndex, lb)
	writeOCILayoutRoot(t, layoutDir, root)

	digs, err := ociremote.ConfigContentDigestsFromOCILayout(context.Background(), layoutDir)
	if err != nil {
		t.Fatalf("read layout per-arch digests: %v", err)
	}
	return digs
}

// writeLayoutBlob writes data into the OCI layout's content-addressed blob store
// (blobs/<algo>/<hex>) and returns its descriptor, the same on-disk shape a real
// OCI image layout uses.
func writeLayoutBlob(t *testing.T, dir, mediaType string, data []byte) ocispec.Descriptor {
	t.Helper()
	desc := content.NewDescriptorFromBytes(mediaType, data)
	path := filepath.Join(dir, "blobs", desc.Digest.Algorithm().String(), desc.Digest.Encoded())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir blob dir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return desc
}

// writeArchImageManifest stages one platform child of the synthetic manifest list:
// a config blob declaring (os, arch) plus a layer and an image manifest. The
// marker perturbs the config so each arch's serialization-agnostic content digest
// differs, exactly as two real per-arch builds would. It returns the
// platform-stamped manifest descriptor for the parent index.
func writeArchImageManifest(t *testing.T, dir, os, arch, marker string) ocispec.Descriptor {
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
		t.Fatalf("marshal config: %v", err)
	}
	cfg := writeLayoutBlob(t, dir, ocispec.MediaTypeImageConfig, configBody)
	layer := writeLayoutBlob(t, dir, ocispec.MediaTypeImageLayerGzip, []byte("layer-"+marker))
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
	md := writeLayoutBlob(t, dir, ocispec.MediaTypeImageManifest, mb)
	md.Platform = &ocispec.Platform{OS: os, Architecture: arch}
	return md
}

// writeOCILayoutRoot writes the oci-layout marker and the top-level index.json
// pointing at the single root descriptor, completing a valid OCI image layout the
// production OpenOCILayout / ConfigContentDigestsFromOCILayout readers accept.
func writeOCILayoutRoot(t *testing.T, dir string, root ocispec.Descriptor) {
	t.Helper()
	layout, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		t.Fatalf("marshal oci-layout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageLayoutFile), layout, 0o644); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{root},
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageIndexFile), ib, 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}
}

func TestPublishE2EComposed(t *testing.T) {
	ctx := e2eContext(t)
	host := registryHost(t)
	registry := host + "/e2e/composed-plan"

	// The artifact under test is a SYNTHETIC multi-arch OCI layout (linux/amd64 +
	// linux/arm64) written on disk, not a real buildx build: S4 proves the
	// manifest-list push + per-arch re-verify contract against the local registry
	// without buildx/QEMU/the docker-container driver (that path is S5, #2038).
	layoutDir := t.TempDir()
	perArch := writeSyntheticMultiArchLayout(t, layoutDir)
	if len(perArch) != 2 {
		t.Fatalf("synthetic layout has %d arches, want the two-arch (amd64+arm64) manifest list", len(perArch))
	}
	cds := make([]freeze.PlatformDigest, 0, len(perArch))
	for _, p := range perArch {
		cds = append(cds, freeze.PlatformDigest{OS: p.OS, Arch: p.Arch, Digest: p.Digest})
	}

	pin := freeze.ImagePin{Ref: "aileron/sandbox-tools", ConfigDigests: cds, LocalTag: "aileron/sandbox-tools:e2e"}
	opts := Options{
		Name: "e2e", VersionID: "v1", Registry: registry,
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
		// Point the layout seam at the just-written synthetic multi-arch OCI layout,
		// opened through the same production reader freeze's real layout flows.
		ComposedLayout: func(ctx context.Context, _ freeze.ImagePin) (oras.ReadOnlyTarget, ocispec.Descriptor, error) {
			return ociremote.OpenOCILayout(ctx, layoutDir)
		},
	}
	res, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("publish composed: %v", err)
	}
	if res.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want config-content-digest", res.BindingKind)
	}
	// Re-publish must be byte-stable (idempotency) against the real registry.
	res2, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("re-publish composed: %v", err)
	}
	if res.ArtifactDigest != res2.ArtifactDigest {
		t.Errorf("re-publish artifact digest drift: %q vs %q", res.ArtifactDigest, res2.ArtifactDigest)
	}
	assertPublishedArtifact(t, ctx, registry, "v1", res.ImageDigest, freeze.BindingConfigContentDigest)
}

func TestPublishE2EForeignBase(t *testing.T) {
	ctx := e2eContext(t)
	host := registryHost(t)

	// Seed a "foreign base" in the registry: build locally and push it, then
	// read back its manifest digest — the digest a freeze foreign-base pin holds.
	srcTag := buildLocalImage(t, "aileron/sandbox-tools:e2e-base")
	srcRef := host + "/e2e/base"
	docker(t, "tag", srcTag, srcRef+":latest")
	docker(t, "push", srcRef+":latest")
	manifestDigest := docker(t, "image", "inspect", "--format", "{{index (split (index .RepoDigests 0) \"@\") 1}}", srcRef+":latest")
	if !strings.HasPrefix(manifestDigest, "sha256:") {
		t.Fatalf("unexpected manifest digest %q", manifestDigest)
	}

	registry := host + "/e2e/foreign-plan"
	pin := freeze.ImagePin{Ref: srcRef, Digest: manifestDigest} // no LocalTag => foreign-base
	res, err := Run(ctx, Options{
		Name: "e2e", VersionID: "v1", Registry: registry,
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
	})
	if err != nil {
		t.Fatalf("publish foreign-base: %v", err)
	}
	if res.BindingKind != freeze.BindingManifestDigest {
		t.Errorf("binding = %q, want manifest-digest", res.BindingKind)
	}
	if res.ImageDigest != manifestDigest {
		t.Errorf("copied manifest digest %q != source %q (oras.Copy must preserve it)", res.ImageDigest, manifestDigest)
	}
	assertPublishedArtifact(t, ctx, registry, "v1", manifestDigest, freeze.BindingManifestDigest)
}

// assertPublishedArtifact resolves the published artifact from the real
// registry and verifies subject, layers, and binding-kind.
func assertPublishedArtifact(t *testing.T, ctx context.Context, registry, tag, wantSubject, wantBinding string) {
	t.Helper()
	repo, err := newRemoteRepository(registry)
	if err != nil {
		t.Fatalf("connect registry: %v", err)
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		t.Fatalf("resolve %s:%s: %v", registry, tag, err)
	}
	raw, err := content.FetchAll(ctx, repo, desc)
	if err != nil {
		t.Fatalf("fetch artifact: %v", err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	if m.Subject == nil || m.Subject.Digest.String() != wantSubject {
		t.Errorf("subject = %v, want %q", m.Subject, wantSubject)
	}
	if len(m.Layers) != 4 {
		t.Errorf("layers = %d, want 4", len(m.Layers))
	}
	if got := m.Annotations[freeze.AnnotationBindingKind]; got != wantBinding {
		t.Errorf("binding-kind = %q, want %q", got, wantBinding)
	}
}
