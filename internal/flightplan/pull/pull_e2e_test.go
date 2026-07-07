//go:build integration_publish

// Package-level e2e for `skill install <oci-ref>` against a REAL OCI registry
// and a REAL docker daemon. It shares the integration_publish build tag (and
// therefore the same CI job + registry:2 service as the publish e2e), so the
// clean-machine install is proven end to end: publish a frozen version to the
// registry, then pull.Run it back, verify the signature, WriteFrozen into a
// temp store with NO local freeze, and confirm the pulled version id and bytes
// match what was published. Per the repo-family rule these fail fast and never
// t.Skip: a missing AILERON_TEST_REGISTRY is a hard failure, and every
// registry call is deadline-bounded.
package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"
	"github.com/ALRubinger/aileron/internal/flightplan/publish"
	"github.com/ALRubinger/aileron/internal/flightplan/store"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

const e2eTimeout = 3 * time.Minute

func e2eContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	t.Cleanup(cancel)
	return ctx
}

func registryHost(t *testing.T) string {
	t.Helper()
	h := os.Getenv("AILERON_TEST_REGISTRY")
	if h == "" {
		t.Fatal("AILERON_TEST_REGISTRY must be set (e.g. localhost:5000) for the pull e2e")
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
// same way the production ImageSource / launch resolver do (docker inspect ->
// imgconfig).
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

// stageComposedLayout turns the real, daemon-built image `tag` into an on-disk OCI
// image layout the publish ComposedLayout seam opens, WITHOUT `docker buildx` or
// the docker-container driver (real buildx + QEMU is reserved for S5, #2038). It
// pushes the image to a scratch repo in the local registry, then oras-copies the
// pushed graph back into a fresh on-disk OCI layout carrying the REAL config and
// layer blobs. That is the difference from the synthetic publish-unit fixtures: the
// composed image publish pushes is a genuinely pullable image, so the round-trip's
// `docker pull` of the boot ref actually boots. It returns the layout directory.
//
// oci.Store records every pushed manifest in its index.json (a multi-root index the
// OpenOCILayout reader rejects); like publish's stageProductionLayout, the helper
// overwrites index.json with the single manifest-list root the copy resolved.
func stageComposedLayout(t *testing.T, ctx context.Context, host, tag string) string {
	t.Helper()
	_, variant, _ := strings.Cut(tag, ":")
	src := host + "/e2e/layout-src-" + variant
	docker(t, "tag", tag, src+":latest")
	docker(t, "push", src+":latest")

	repo, err := ociremote.NewRepository(src)
	if err != nil {
		t.Fatalf("connect layout source registry: %v", err)
	}
	layoutDir := t.TempDir()
	st, err := oci.New(layoutDir)
	if err != nil {
		t.Fatalf("oci.New layout: %v", err)
	}
	root, err := oras.Copy(ctx, repo, "latest", st, "latest", oras.DefaultCopyOptions)
	if err != nil {
		t.Fatalf("copy image into OCI layout: %v", err)
	}
	singleRoot := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{root},
	}
	ib, err := json.Marshal(singleRoot)
	if err != nil {
		t.Fatalf("marshal single-root index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, ocispec.ImageIndexFile), ib, 0o644); err != nil {
		t.Fatalf("write single-root index.json: %v", err)
	}
	return layoutDir
}

// e2eFrozen mints a REAL signed frozen version (no-environment manifest, no
// image resolver needed) so the pulled artifact actually verifies against the
// launch-time gate. It returns the freeze result and the version-id slug used
// as the artifact tag.
func e2eFrozen(t *testing.T) (freeze.Result, string) {
	t.Helper()
	const md = `---
name: e2e-plan
description: A signed no-environment plan for the pull e2e.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields:
              - result
  inputs: []
  outputs: []
---

# E2E Plan
`
	res, err := freeze.Run(context.Background(), []byte(md), freeze.Options{SigningKeyPath: genSigningKeyPath(t)})
	if err != nil {
		t.Fatalf("freeze.Run: %v", err)
	}
	return res, "v" + res.ContentHash[len("sha256:"):][:12]
}

// TestPullE2ERoundTrip publishes a frozen version's image + signed artifact to
// a real registry, then installs it on a clean store via pull.Run + verify +
// WriteFrozen, asserting the store lists the skill and the read-back artifacts
// are byte-identical to the published ones, all without any local freeze step.
func TestPullE2ERoundTrip(t *testing.T) {
	ctx := e2eContext(t)
	host := registryHost(t)

	// Build a composed image so publish has a config-content-digest subject, then
	// stage it as a REAL on-disk OCI layout (no buildx) the ComposedLayout seam
	// opens — the same seam production wires to composition.OCILayoutDir.
	tag, configContentDigest := buildLocalImage(t, "aileron/sandbox-tools:e2e-pull")
	layoutDir := stageComposedLayout(t, ctx, host, tag)
	res, versionID := e2eFrozen(t)
	registry := host + "/e2e/pull-plan"
	pin := freeze.ImagePin{Ref: "aileron/sandbox-tools", ConfigDigests: hostConfigDigests(configContentDigest), LocalTag: tag}

	if _, err := publish.Run(ctx, publish.Options{
		Name: "e2e-plan", VersionID: versionID, Registry: registry,
		Frozen: store.FrozenVersion{
			ID:        versionID,
			SkillMD:   res.FrozenManifest,
			Lockfile:  res.Lockfile,
			Signature: res.Signature,
			PublicKey: res.PublicKey,
		},
		Lock: freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}},
		ComposedLayout: func(ctx context.Context, _ freeze.ImagePin) (oras.ReadOnlyTarget, ocispec.Descriptor, error) {
			return ociremote.OpenOCILayout(ctx, layoutDir)
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Clean-machine install: pull the signed artifact back by <registry>:<id>.
	pulled, err := Run(ctx, Options{Ref: registry + ":" + versionID})
	if err != nil {
		t.Fatalf("pull.Run: %v", err)
	}
	if pulled.Name != "e2e-plan" {
		t.Errorf("name = %q, want e2e-plan", pulled.Name)
	}
	if pulled.Frozen.ID != versionID {
		t.Errorf("frozen id = %q, want %q", pulled.Frozen.ID, versionID)
	}
	if !bytes.Equal(pulled.Frozen.SkillMD, res.FrozenManifest) {
		t.Error("pulled SKILL.md differs from published")
	}

	// Write into a fresh store with NO prior freeze, then read it back.
	st := store.New(t.TempDir())
	if err := st.WriteFrozen(pulled.Name, pulled.Frozen); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	names, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "e2e-plan" {
		t.Errorf("List = %v, want [e2e-plan]", names)
	}
	readBack, err := st.ReadFrozen("e2e-plan", versionID)
	if err != nil {
		t.Fatalf("ReadFrozen: %v", err)
	}
	if !bytes.Equal(readBack.SkillMD, res.FrozenManifest) ||
		!bytes.Equal(readBack.Lockfile, res.Lockfile) ||
		!bytes.Equal(readBack.Signature, res.Signature) ||
		!bytes.Equal(readBack.PublicKey, res.PublicKey) {
		t.Error("stored artifacts are not byte-identical to what was published")
	}

	// The clean-store artifacts must still verify (defense: end-to-end integrity).
	if _, err := freeze.VerifyFrozen(readBack.SkillMD, readBack.Lockfile, readBack.Signature, readBack.PublicKey); err != nil {
		t.Fatalf("verify pulled artifact: %v", err)
	}

	// Sharing the registry with #1901's artifact copy: connect and confirm the
	// tag resolves (the referrer the install consumed).
	repo, err := ociremote.NewRepository(registry)
	if err != nil {
		t.Fatalf("connect registry: %v", err)
	}
	if _, err := repo.Resolve(ctx, versionID); err != nil {
		t.Fatalf("resolve published artifact tag: %v", err)
	}

	// Cross-machine launch (#1903): pull and verify the PUBLISHED composed image
	// against the signed lock pin and confirm PullImage returns a bootable ref.
	// This is the read side the clean-machine launch takes after WriteOrigin.
	imgRes, err := PullImage(ctx, ImagePullOptions{
		Registry:   registry,
		VersionTag: versionID,
		Pin:        pin,
	})
	if err != nil {
		t.Fatalf("PullImage (composed): %v", err)
	}
	if imgRes.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("binding = %q, want %q", imgRes.BindingKind, freeze.BindingConfigContentDigest)
	}
	// The boot ref is content-addressed to the composed image's MANIFEST digest
	// (TOCTOU-closing), not the mutable tag. Resolve the published tag to learn
	// the manifest digest and assert the boot ref matches.
	composedDesc, err := repo.Resolve(ctx, freeze.ComposedImageTag(versionID))
	if err != nil {
		t.Fatalf("resolve published composed image tag: %v", err)
	}
	wantRef := registry + "@" + composedDesc.Digest.String()
	if imgRes.BootRef != wantRef {
		t.Errorf("boot ref = %q, want the content-addressed manifest ref %q", imgRes.BootRef, wantRef)
	}
	if imgRes.ImageDigest != configContentDigest {
		t.Errorf("verified image digest = %q, want the config content digest %q", imgRes.ImageDigest, configContentDigest)
	}
	// The daemon must be able to boot the returned reference: pull it and confirm
	// the local config CONTENT digest still equals the attested pin (the same
	// identity launch's runner relies on). The config-blob sha256 may differ after
	// the registry round-trip under the containerd store; the content digest is
	// what is stable, which is the whole point of #2014.
	docker(t, "pull", imgRes.BootRef)
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", imgRes.BootRef).Run() })
	pulledContentDigest := localContentDigest(t, imgRes.BootRef)
	if pulledContentDigest != configContentDigest {
		t.Errorf("pulled image content digest = %q, want the attested config content digest %q", pulledContentDigest, configContentDigest)
	}
}

// TestPullImageE2ERefusesTamperedImage proves the fail-closed property against a
// real registry: publishing a mismatched image under the composed tag and then
// pulling with a pin that attests a different config digest is refused, never
// booted. Here the "tamper" is a lock pin that attests a config digest the
// published image does not carry; PullImage must return ErrImageDigestMismatch.
func TestPullImageE2ERefusesTamperedImage(t *testing.T) {
	ctx := e2eContext(t)
	host := registryHost(t)

	// Publish a real composed image + artifact honestly, then pull with a pin
	// whose Digest is a config content digest the pushed image does NOT carry: the
	// registry served the honest image, but the signed lock attests a different
	// identity, so the binding check must refuse.
	tag, configContentDigest := buildLocalImage(t, "aileron/sandbox-tools:e2e-tamper")
	layoutDir := stageComposedLayout(t, ctx, host, tag)
	res, versionID := e2eFrozen(t)
	registry := host + "/e2e/tamper-plan"
	honestPin := freeze.ImagePin{Ref: "aileron/sandbox-tools", ConfigDigests: hostConfigDigests(configContentDigest), LocalTag: tag}

	if _, err := publish.Run(ctx, publish.Options{
		Name: "e2e-plan", VersionID: versionID, Registry: registry,
		Frozen: store.FrozenVersion{
			ID:        versionID,
			SkillMD:   res.FrozenManifest,
			Lockfile:  res.Lockfile,
			Signature: res.Signature,
			PublicKey: res.PublicKey,
		},
		Lock: freeze.Lockfile{ResolvedImages: []freeze.ImagePin{honestPin}},
		ComposedLayout: func(ctx context.Context, _ freeze.ImagePin) (oras.ReadOnlyTarget, ocispec.Descriptor, error) {
			return ociremote.OpenOCILayout(ctx, layoutDir)
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	tamperedPin := honestPin
	tamperedPin.ConfigDigests = hostConfigDigests("sha256:" + strings.Repeat("f", 64))
	_, err := PullImage(ctx, ImagePullOptions{
		Registry:   registry,
		VersionTag: versionID,
		Pin:        tamperedPin,
	})
	if err == nil {
		t.Fatal("a pin attesting a different config digest must be refused")
	}
	if !errors.Is(err, ErrImageDigestMismatch) {
		t.Fatalf("err = %v, want ErrImageDigestMismatch", err)
	}
}
