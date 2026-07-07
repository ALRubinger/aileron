//go:build integration_sandbox

// Real-container end-to-end coverage for the composed-tools boot linkage and
// the boot-time Id-vs-Digest guard (#1863).
//
// This test proves the load-bearing #1863 contract end to end on a real daemon:
// freeze a tools-plan unit through the REAL freeze path (the CLI's production
// newDigestResolver / newFeatureComposer -> builderFeatureComposer ->
// container.Builder + composition.ToolsPlan), which builds a genuine composed
// image in the local daemon and pins it with a bootable LocalTag plus the
// image's attested Id as the pin Digest. Then boot that composed pin through
// the production containerImageRunner.Run with the production digest resolver
// wired (via runtime.Options), so the boot re-inspects the just-built local tag
// and the guard resolves it to the EXACT attested Id before delegating to the
// container boot. The guard passing on the genuine daemon image is the
// load-bearing assertion: the freeze-built image, its LocalTag, its attested
// Digest, and the boot-time re-check all line up on one real image.
//
// Unlike the runtime unit tests (which drive runInImage with a fake resolver
// and no Docker), this test builds a real image and boots it against a live
// daemon, so a break anywhere in the freeze -> build -> boot-by-LocalTag ->
// guard linkage FAILS here rather than passing on a fake.
//
// The wiring is env-driven so one body serves the CI job and a local run:
//
//	AILERON_API_URL   live host daemon API base (with /v1)
//	AILERON_TOKEN     host daemon auth token
//
// The base image the tools compose onto is the worked example's
// catalog-resolved Aileron runner base (composition.BaseImage), which must be
// pullable from the host running this test.
//
// Per repo policy (no skip-when-unsupported) this test is FAIL-FAST: absence of
// Docker, the env, the build toolchain, or a reachable daemon is a
// job-configuration FAILURE (t.Fatalf), never a t.Skip. CI's integration_sandbox
// job provisions the toolchain, boots a host daemon, and sets the env.
//
// Run with:
//
//	go test -tags=integration_sandbox -run TestFlightPlanComposedToolsBootGuard ./cmd/aileron/...
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// TestFlightPlanComposedToolsBootGuard freezes a real tools-plan unit (building
// a genuine composed image through container.Builder), then boots the produced
// composed pin through containerImageRunner.Run with the production digest
// resolver, asserting the boot-time Id-vs-Digest guard resolves the just-built
// local image to the exact attested Id and the boot succeeds (#1863).
func TestFlightPlanComposedToolsBootGuard(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		// Fail-fast, not skip: repo policy forbids skip-when-unsupported. The CI
		// job documents `docker` as a prerequisite.
		t.Fatalf("docker not on PATH; install Docker to run this integration test: %v", err)
	}
	apiURL := strings.TrimSpace(os.Getenv("AILERON_API_URL"))
	token := strings.TrimSpace(os.Getenv("AILERON_TOKEN"))
	if apiURL == "" || token == "" {
		t.Fatalf("AILERON_API_URL and AILERON_TOKEN must point at a live host daemon (required, not skipped)")
	}

	// Point the process store seam at a fresh dir so the freeze persists the
	// composed unit where the boot reads it back.
	origStore := skillStoreDir
	skillStoreDir = t.TempDir()
	t.Cleanup(func() { skillStoreDir = origStore })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Freeze the worked example through the REAL freeze path. The worked example
	// declares tools (aws-cli@2.x) and no custom `image`, so freeze composes them
	// onto the catalog-resolved Aileron runner base (composition.BaseImage), which
	// CI must have pullable. newDigestResolver pulls+inspects that base for its
	// registry digest; newFeatureComposer routes the environment tools through
	// builderFeatureComposer -> container.Builder + composition.ToolsPlan, which
	// BUILDS a genuine composed image in the local daemon and attests its
	// serialization-agnostic config content digest (localImageContentDigest). The
	// produced pin carries the bootable LocalTag and that content digest as its
	// Digest.
	raw, err := os.ReadFile(exampleManifestPath(t))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	keyPath := writeSigningKey(t)
	res, err := freeze.Run(ctx, raw, freeze.Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       newDigestResolver(),
		Composer:       newFeatureComposer(),
	})
	if err != nil {
		t.Fatalf("freeze the composed-tools unit through the real build path: %v", err)
	}

	// Persist the frozen unit into the store the boot mounts.
	s := store.New(skillStoreDir)
	id := frozenVersionID(res.ContentHash)
	if err := s.WriteFrozen(res.Name, store.FrozenVersion{
		ID:        id,
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}); err != nil {
		t.Fatalf("persist frozen composed unit: %v", err)
	}

	// The boot bind-mounts this store read-only into a container whose process
	// runs under a different UID than the test. t.TempDir() is 0700, so the mount
	// root is not traversable by that UID and the in-container read of SKILL.md
	// fails with "permission denied". Make the whole store tree world-readable and
	// traversable (the sibling boot test avoids this by pointing skillStoreDir at
	// the CI-provisioned AILERON_SANDBOX_FLIGHTPLAN_STORE instead of a TempDir).
	if err := filepath.Walk(skillStoreDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.IsDir() {
			mode = 0o755
		}
		return os.Chmod(p, mode)
	}); err != nil {
		t.Fatalf("make frozen store container-readable: %v", err)
	}

	// Read back the composed pin and assert its shape: a bootable LocalTag and a
	// per-arch configDigests set carrying a sha256: content digest for the host
	// platform (the freeze-built image's attested Id). This is the exact input the
	// boot-time guard re-checks against the daemon.
	if len(res.Lock.ResolvedImages) != 1 {
		t.Fatalf("expected exactly one composed pin, got %+v", res.Lock.ResolvedImages)
	}
	pin := res.Lock.ResolvedImages[0]
	if pin.LocalTag == "" {
		t.Fatalf("the composed pin must carry a bootable LocalTag; got %+v", pin)
	}
	attested, _, ok := pin.HostConfigDigest()
	if !ok {
		t.Fatalf("the composed pin must carry a config digest for the host platform; got %+v", pin)
	}
	if !strings.HasPrefix(attested, "sha256:") {
		t.Fatalf("the composed pin's host config digest must be a sha256: attested Id, got %q", attested)
	}

	// Boot the produced composed pin through the production runner with the
	// production digest resolver wired. The runtime consults the resolver on the
	// composed boot path: it re-inspects pin.LocalTag in the daemon and MUST
	// resolve it to the host platform's attested config digest or fail closed. A
	// successful boot is the proof that the guard resolves the genuine daemon
	// image to the attested Id and lets the linkage through.
	// The plan materializes report.html into the out-dir, which is bind-mounted
	// into the container at /aileron/out. t.TempDir() is 0700 and the in-container
	// process runs under a different UID, so make the out-dir world-writable or
	// the artifact write fails with "permission denied".
	outDir := t.TempDir()
	if err := os.Chmod(outDir, 0o777); err != nil {
		t.Fatalf("make out-dir writable: %v", err)
	}

	res2, err := runtime.Run(ctx, runtime.Options{
		Store:               s,
		Name:                res.Name,
		Version:             id,
		OutDir:              outDir,
		ImageRunner:         containerImageRunner{daemonEnv: daemonImageEnv{}},
		ImageDigestResolver: containerImageDigestResolver{},
	})
	if err != nil {
		t.Fatalf("boot the composed pin (guard must resolve the just-built local tag %q to the attested %s): %v", pin.LocalTag, attested, err)
	}
	_ = res2
}

// exampleManifestPath returns the absolute path to the dedicated tools-declaring
// fixture this test freezes. It is a minimal offline plan (one curated tool, a
// literal input, and a single html-render transform, with no source input,
// action-call, or llm-seam) so the in-container launch runs to completion without
// a live connector. The shared docs example (weekly-metrics-digest) reads a
// source input from a live metrics API, which returns 401 in CI, so it cannot be
// used to prove the freeze->build->boot linkage end to end.
func exampleManifestPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootForTest(t), "cmd", "aileron", "testdata", "composed-tools-boot.skill.md")
}
