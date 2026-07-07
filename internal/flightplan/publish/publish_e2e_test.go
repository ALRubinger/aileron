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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"

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

// buildOCILayout builds a single-arch image as a real OCI image layout with
// `docker buildx build --output type=oci,dest=<dir>,tar=false` (no QEMU, no daemon
// load) — byte-for-byte the exporter flags freeze's multi-arch build uses (see
// container.BuildOptions.OCILayoutDest), so the e2e opens exactly the artifact
// shape production writes. It returns the per-arch config content digests read
// back from the layout (the values the signed lock attests).
func buildOCILayout(t *testing.T, layoutDir string) []ociremote.PlatformConfigDigest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("aileron-e2e-layout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM scratch\nADD hello.txt /hello.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, "docker", "buildx", "build",
		"--platform", "linux/"+goarch(),
		"--provenance=false",
		"--output", "type=oci,dest="+layoutDir+",tar=false",
		src,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("buildx build oci layout: %v\n%s", err, out)
	}
	digs, err := ociremote.ConfigContentDigestsFromOCILayout(ctx, layoutDir)
	if err != nil {
		t.Fatalf("read layout per-arch digests: %v", err)
	}
	return digs
}

// goarch returns the host arch in the linux/<arch> vocabulary the layout is built
// for, so the e2e never emulates a foreign architecture.
func goarch() string { return runtime.GOARCH }

func TestPublishE2EComposed(t *testing.T) {
	ctx := e2eContext(t)
	host := registryHost(t)
	registry := host + "/e2e/composed-plan"

	layoutDir := t.TempDir()
	perArch := buildOCILayout(t, layoutDir)
	cds := make([]freeze.PlatformDigest, 0, len(perArch))
	for _, p := range perArch {
		cds = append(cds, freeze.PlatformDigest{OS: p.OS, Arch: p.Arch, Digest: p.Digest})
	}

	pin := freeze.ImagePin{Ref: "aileron/sandbox-tools", ConfigDigests: cds, LocalTag: "aileron/sandbox-tools:e2e"}
	opts := Options{
		Name: "e2e", VersionID: "v1", Registry: registry,
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
		// Point the layout seam at the just-built single-arch OCI layout.
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
