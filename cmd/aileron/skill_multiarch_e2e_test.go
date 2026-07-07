//go:build integration_sandbox_multiarch

// Real, emulated cross-arch end-to-end regression for the multi-arch Flight Plan
// freeze -> publish -> launch chain (umbrella #2034, sub-issue #2038). It is the
// #2025 repro the S2-S4 contract tests deliberately cannot cover: those prove the
// per-arch pin schema, the multi-arch freeze, and the manifest-list publish +
// per-arch re-verify against SYNTHETIC layouts and in-memory stores, so they
// never run real `docker buildx --platform linux/amd64,linux/arm64`, real QEMU
// emulation, or a genuine cross-arch consumer selection. This test does, in one
// dedicated CI job (`integration-multiarch`) that provisions buildx + binfmt/QEMU
// + a local registry.
//
// The #2025 bug is a publisher on one arch and a consumer on another: the
// consumer must select ITS arch's child from the published manifest list, read
// that child's serialization-agnostic config content digest, and match it to the
// signed lock's per-arch entry. On a single-arch (amd64) runner the publisher
// arch is always amd64, so the consumer is driven to the FOREIGN arch (arm64) via
// the freeze.hostPlatform() override seam AILERON_FLIGHTPLAN_HOST_ARCH (#2038's
// only production change). Running an arm64 aileron binary under QEMU is out of
// scope; selection + boot re-check SUCCESS is the assertion, not running the
// arm64 workload to completion.
//
// Real path, no fakes: the genuine buildx+QEMU multi-arch freeze (through the
// CLI's production newDigestResolver/newFeatureComposer), a real manifest-list
// push (publishRun = publish.Run, reading the freeze-produced OCI layout via the
// production composedLayoutOpener), and the real pull+verify (through the
// production launchRegistryImageResolver -> pull.PullImage -> ConfigContentDigest
// index-unwrap). Two negative guards prove the job catches regressions: an arch
// the artifact was NOT built for fails closed, and a tampered per-arch config is
// refused. Per repo policy this test is FAIL-FAST (no t.Skip): absent Docker,
// buildx, QEMU, the registry, or the multi-arch base is a job-config FAILURE.
//
// Run with (in the provisioned job):
//
//	go test -tags=integration_sandbox_multiarch -run TestFlightPlanCrossArchMultiArchE2E ./cmd/aileron/...
package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"
	"github.com/ALRubinger/aileron/internal/flightplan/publish"
	"github.com/ALRubinger/aileron/internal/flightplan/pull"
	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// hostArchOverrideEnv mirrors freeze's unexported const: the consumer-arch
// override the e2e drives so an amd64 runner selects the foreign arm64 child.
const hostArchOverrideEnv = "AILERON_FLIGHTPLAN_HOST_ARCH"

// recordingImageRunner is a runtime.ImageRunner that records the boot spec and
// never boots, so the registry-origin boot re-check path runs end to end (load ->
// pull -> per-arch verify -> resolve boot ref) WITHOUT running the arm64 workload
// under QEMU. Capturing the resolved boot ref is the selection assertion.
type recordingImageRunner struct {
	spec runtime.ImageRunSpec
}

func (r *recordingImageRunner) Run(_ context.Context, spec runtime.ImageRunSpec) (runtime.ImageRunResult, error) {
	r.spec = spec
	return runtime.ImageRunResult{}, nil
}

// requireMultiArchToolchain fail-fasts (never skips) unless the emulated build +
// registry environment the dedicated CI job provisions is present.
func requireMultiArchToolchain(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker not on PATH; the integration-multiarch job must provision it: %v", err)
	}
	// A docker-container buildx builder is required for a real multi-platform
	// build; `docker buildx inspect` failing means the driver is not provisioned.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "buildx", "inspect").CombinedOutput(); err != nil {
		t.Fatalf("docker buildx not usable; the job must `docker buildx create --driver docker-container --use`: %v\n%s", err, out)
	}
	registry := strings.TrimSpace(os.Getenv("AILERON_TEST_REGISTRY"))
	if registry == "" {
		t.Fatalf("AILERON_TEST_REGISTRY must point at the job's local registry (e.g. localhost:5000); required, not skipped")
	}
	return registry
}

// TestFlightPlanCrossArchMultiArchE2E freezes a composed-tools plan as a genuine
// linux/amd64+linux/arm64 image, publishes it as a manifest list to a real
// registry, installs it as a registry-origin plan, and proves an amd64 consumer
// overridden to arm64 selects, verifies, and would boot the arm64 child (#2025),
// plus two fail-closed negative guards.
func TestFlightPlanCrossArchMultiArchE2E(t *testing.T) {
	registryHost := requireMultiArchToolchain(t)
	registry := registryHost + "/e2e/multiarch-plan"

	// Persist the freeze into a fresh store the launch reads back from.
	origStore := skillStoreDir
	skillStoreDir = t.TempDir()
	t.Cleanup(func() { skillStoreDir = origStore })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// --- FREEZE (real multi-arch buildx + QEMU) -----------------------------
	// The production composer routes the plan's environment tools through
	// container.Builder, which runs `docker buildx build --platform
	// linux/amd64,linux/arm64 --output type=oci`, reads each arch's config content
	// digest back from the built OCI layout, and pins a per-arch config-digest set.
	raw, err := os.ReadFile(exampleManifestPath(t))
	if err != nil {
		t.Fatalf("read composed-tools fixture: %v", err)
	}
	res, err := freeze.Run(ctx, raw, freeze.Options{
		Version:        "1.0.0",
		SigningKeyPath: writeSigningKey(t),
		Resolver:       newDigestResolver(),
		Composer:       newFeatureComposer(),
	})
	if err != nil {
		t.Fatalf("multi-arch freeze through the production composer: %v", err)
	}
	if len(res.Lock.ResolvedImages) != 1 {
		t.Fatalf("expected exactly one composed pin, got %+v", res.Lock.ResolvedImages)
	}
	pin := res.Lock.ResolvedImages[0]
	if pin.LocalTag == "" {
		t.Fatalf("composed pin must carry a bootable LocalTag; got %+v", pin)
	}
	// The pin must carry BOTH arches: a single-arch freeze would make the whole
	// cross-arch premise vacuous, so this is the load-bearing S3 precondition.
	amdDigest, amdOK := pin.ConfigDigestFor("linux", "amd64")
	armDigest, armOK := pin.ConfigDigestFor("linux", "arm64")
	if !amdOK || !armOK {
		t.Fatalf("composed pin must carry both linux/amd64 and linux/arm64 config digests; got %+v", pin.ConfigDigests)
	}
	if amdDigest == armDigest {
		t.Fatalf("the two arches must have distinct config content digests (a real per-arch build); both were %q", amdDigest)
	}

	// Persist the signed frozen version.
	s := store.New(skillStoreDir)
	versionID := frozenVersionID(res.ContentHash)
	if err := s.WriteFrozen(res.Name, store.FrozenVersion{
		ID:        versionID,
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}); err != nil {
		t.Fatalf("persist frozen composed unit: %v", err)
	}

	// --- PUBLISH (real manifest-list push) ----------------------------------
	// publishRun = publish.Run with NO ComposedLayout override, so the production
	// composedLayoutOpener reads composition.OCILayoutDir(pin.LocalTag) — the exact
	// layout the freeze above wrote. This is the production publish path.
	fv, err := s.ReadFrozen(res.Name, versionID)
	if err != nil {
		t.Fatalf("read back frozen version: %v", err)
	}
	pubRes, err := publishRun(ctx, publish.Options{
		Name:      res.Name,
		VersionID: versionID,
		Registry:  registry,
		Frozen:    fv,
		Lock:      res.Lock,
	})
	if err != nil {
		t.Fatalf("publish the multi-arch composed image: %v", err)
	}
	if pubRes.BindingKind != freeze.BindingConfigContentDigest {
		t.Errorf("published binding = %q, want %q (a composed pin binds by config content digest)", pubRes.BindingKind, freeze.BindingConfigContentDigest)
	}

	// The published tag must resolve to a manifest LIST carrying both platform
	// children (an index), not a single-arch manifest. The production reader
	// enumerates every runnable platform, so len==2 proves both arches shipped.
	repo, err := ociremote.NewRepository(registry)
	if err != nil {
		t.Fatalf("connect published registry: %v", err)
	}
	composedTag := freeze.ComposedImageTag(versionID)
	indexDesc, err := repo.Resolve(ctx, composedTag)
	if err != nil {
		t.Fatalf("resolve published composed image tag %q: %v", composedTag, err)
	}
	published, err := ociremote.AllPlatformConfigContentDigests(ctx, repo, indexDesc)
	if err != nil {
		t.Fatalf("read published per-arch config digests: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("published artifact has %d runnable platforms, want the two-arch manifest list", len(published))
	}

	// --- INSTALL as a registry-origin plan ----------------------------------
	// WriteOrigin makes LoadVerified report ImageOrigin.Present, so the launch
	// takes the #1903 registry pull path rather than the local-tag boot path.
	if err := s.WriteOrigin(res.Name, versionID, store.Origin{Registry: registry, VersionTag: versionID}); err != nil {
		t.Fatalf("record registry install origin: %v", err)
	}
	origin := runtime.RegistryImageOrigin{Registry: registry, VersionTag: versionID, Present: true}

	// The boot ref stays anchored to the resolved (index) MANIFEST digest — the
	// per-arch config-content check binds the arch identity, while the boot ref is
	// content-addressed to the index so the runner cannot boot a repointed tag.
	wantBootRef := registry + "@" + indexDesc.Digest.String()

	// --- POSITIVE: foreign-arch selection + verify --------------------------
	t.Run("amd64 consumer overridden to arm64 selects and verifies the arm64 child", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "arm64")

		// Drive pull.PullImage directly (the seam launchRegistryImageResolver wraps)
		// so the per-arch selection is assertable: it must resolve the manifest list,
		// unwrap to the arm64 child, read its config content digest, and match the
		// lock's arm64 entry — never the amd64 entry the publisher ran on.
		imgRes, err := pull.PullImage(ctx, pull.ImagePullOptions{Registry: registry, VersionTag: versionID, Pin: pin})
		if err != nil {
			t.Fatalf("cross-arch pull+verify (arm64 consumer of an amd64-published plan): %v", err)
		}
		if imgRes.BindingKind != freeze.BindingConfigContentDigest {
			t.Errorf("binding = %q, want %q", imgRes.BindingKind, freeze.BindingConfigContentDigest)
		}
		if imgRes.ImageDigest != armDigest {
			t.Errorf("verified config digest = %q, want the lock's arm64 entry %q (the arm64 child was selected)", imgRes.ImageDigest, armDigest)
		}
		if imgRes.ImageDigest == amdDigest {
			t.Errorf("verified config digest is the amd64 entry %q; the consumer wrongly selected the publisher arch", amdDigest)
		}
		if imgRes.BootRef != wantBootRef {
			t.Errorf("boot ref = %q, want the index-anchored %q", imgRes.BootRef, wantBootRef)
		}

		// The production resolver the runtime wires must return the same boot ref.
		gotRef, err := launchRegistryImageResolver{}.Resolve(ctx, origin, pin)
		if err != nil {
			t.Fatalf("launchRegistryImageResolver.Resolve: %v", err)
		}
		if gotRef != wantBootRef {
			t.Errorf("resolver boot ref = %q, want %q", gotRef, wantBootRef)
		}
	})

	// --- POSITIVE: the registry-origin boot re-check selects the arm64 child --
	t.Run("registry-origin boot re-check reaches the pull path and resolves the arm64 boot ref", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "arm64")
		rec := &recordingImageRunner{}
		if _, err := runtime.Run(ctx, runtime.Options{
			Store:                 s,
			Name:                  res.Name,
			Version:               versionID,
			ImageRunner:           rec,
			RegistryImageResolver: launchRegistryImageResolver{},
		}); err != nil {
			t.Fatalf("boot the registry-origin plan (must pull+verify the arm64 child): %v", err)
		}
		if rec.spec.Image != wantBootRef {
			t.Errorf("boot spec image = %q, want the verified arm64 boot ref %q", rec.spec.Image, wantBootRef)
		}
	})

	// --- NEGATIVE (a): an arch the artifact was NOT built for fails closed ---
	t.Run("a consumer arch absent from the artifact fails closed", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "ppc64le")
		_, err := pull.PullImage(ctx, pull.ImagePullOptions{Registry: registry, VersionTag: versionID, Pin: pin})
		if err == nil {
			t.Fatal("pulling for an unbuilt arch must fail closed, never boot an unattested image")
		}
		if !errors.Is(err, pull.ErrImageDigestMismatch) {
			t.Fatalf("err = %v, want ErrImageDigestMismatch", err)
		}
		if !strings.Contains(err.Error(), "not published for") {
			t.Errorf("err = %q, want the 'not published for' fail-closed message", err.Error())
		}
	})

	// --- NEGATIVE (b): a tampered per-arch config is refused ----------------
	t.Run("a tampered arm64 config digest is refused", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "arm64")
		tampered := pin
		// Replace the arm64 entry with a digest the published image does not carry;
		// the registry serves the honest image, but the (signed-lock) attestation
		// for the selected arch no longer matches, so the per-arch verify must refuse.
		tampered.ConfigDigests = append([]freeze.PlatformDigest(nil), pin.ConfigDigests...)
		for i := range tampered.ConfigDigests {
			if tampered.ConfigDigests[i].Arch == "arm64" {
				tampered.ConfigDigests[i].Digest = "sha256:" + strings.Repeat("f", 64)
			}
		}
		_, err := pull.PullImage(ctx, pull.ImagePullOptions{Registry: registry, VersionTag: versionID, Pin: tampered})
		if err == nil {
			t.Fatal("a pin attesting a different arm64 config digest must be refused")
		}
		if !errors.Is(err, pull.ErrImageDigestMismatch) {
			t.Fatalf("err = %v, want ErrImageDigestMismatch", err)
		}
	})
}
