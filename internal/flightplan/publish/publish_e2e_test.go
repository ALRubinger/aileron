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
	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

// buildLocalImage builds a tiny scratch image locally and returns its tag and
// serialization-agnostic config content digest, mirroring how freeze pins a
// composed image (localImageContentDigest).
func buildLocalImage(t *testing.T, tag string) (string, string) {
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
	return tag, localContentDigest(t, tag)
}

// localContentDigest computes the config content digest of a local image the
// same way the production ImageSource does (docker inspect -> imgconfig).
func localContentDigest(t *testing.T, ref string) string {
	t.Helper()
	inspect := docker(t, "image", "inspect", "--format", "{{json .}}", ref)
	cc, err := imgconfig.FromDockerInspect([]byte(inspect))
	if err != nil {
		t.Fatalf("canonicalize %s config: %v", ref, err)
	}
	d, err := cc.ContentDigest()
	if err != nil {
		t.Fatalf("content digest %s: %v", ref, err)
	}
	return d
}

func TestPublishE2EComposed(t *testing.T) {
	ctx := e2eContext(t)
	host := registryHost(t)
	tag, configContentDigest := buildLocalImage(t, "aileron/sandbox-tools:e2e-composed")
	registry := host + "/e2e/composed-plan"

	pin := freeze.ImagePin{Ref: "aileron/sandbox-tools", Digest: configContentDigest, LocalTag: tag}
	opts := Options{
		Name: "e2e", VersionID: "v1", Registry: registry,
		Frozen: testFrozen(),
		Lock:   freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
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
	srcTag, _ := buildLocalImage(t, "aileron/sandbox-tools:e2e-base")
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
