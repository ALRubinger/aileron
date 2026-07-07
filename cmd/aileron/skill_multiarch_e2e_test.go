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
// signed lock's per-arch entry. The cross-arch consumer is made GENUINE, not
// simulated: the dedicated CI job COMPILES this test binary for linux/arm64 and
// runs it under the binfmt/QEMU emulators it provisions, so runtime.GOARCH is
// authentically arm64 while the runner published both arches. There is no env
// override; the arch the consumer selects is the arch it actually runs as, read
// through freeze.hostPlatform() and ociremote's manifest-list child selection
// (both keyed on the same hostGOARCH = runtime.GOARCH seam). Run natively (amd64)
// the same test proves same-arch selection; run emulated (arm64) it proves the
// foreign-arch #2025 path. Running the arm64 workload to completion is out of
// scope; selection + boot re-check SUCCESS is the assertion.
//
// Real path, no fakes: the genuine buildx+QEMU multi-arch freeze (through the
// CLI's production newDigestResolver/newFeatureComposer), a real manifest-list
// push (publishRun = publish.Run, reading the freeze-produced OCI layout via the
// production composedLayoutOpener), and the real pull+verify (through the
// production launchRegistryImageResolver -> pull.PullImage -> ConfigContentDigest
// index-unwrap). A negative guard proves the job catches regressions: a tampered
// per-arch config is refused. (The fail-closed behavior for an arch the artifact
// was NOT built for is covered by the in-process arch-seam unit tests in freeze
// and ociremote, which can drive an unbuilt arch deterministically; a genuine
// runtime.GOARCH cannot be pointed at an arch no runner or emulator provides.)
// Per repo policy this test is FAIL-FAST (no t.Skip): absent Docker, buildx,
// QEMU, the registry, or the multi-arch base is a job-config FAILURE.
//
// Run with (in the provisioned job, compiled for linux/arm64 and executed under
// QEMU so runtime.GOARCH is arm64):
//
//	GOOS=linux GOARCH=arm64 go test -c -tags=integration_sandbox_multiarch -o multiarch-arm64.test ./cmd/aileron
//	./multiarch-arm64.test -test.run TestFlightPlanCrossArchMultiArchE2E
package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	goruntime "runtime"
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
// registry, installs it as a registry-origin plan, and proves the consumer
// selects, verifies, and would boot the child matching its own runtime.GOARCH
// (#2025). Compiled for arm64 and run under QEMU in the dedicated job, that is the
// genuine cross-arch case; run natively it is same-arch. A fail-closed negative
// guard (a tampered per-arch config) proves the job catches regressions.
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

	// The consumer selects the child matching its own genuine runtime.GOARCH: arm64
	// when this binary is the arm64 build run under QEMU (the cross-arch #2025 case),
	// amd64 when run natively. The build produced both arches above, so the running
	// arch must be present in the pin.
	hostArch := goruntime.GOARCH
	wantArchDigest, wantArchOK := pin.ConfigDigestFor("linux", hostArch)
	if !wantArchOK {
		t.Fatalf("the multi-arch build did not include the runner arch linux/%s; got %+v", hostArch, pin.ConfigDigests)
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

	// --- POSITIVE: running-arch selection + verify --------------------------
	t.Run("the consumer selects and verifies its own arch's child", func(t *testing.T) {
		// Drive pull.PullImage directly (the seam launchRegistryImageResolver wraps)
		// so the per-arch selection is assertable: it must resolve the manifest list,
		// unwrap to the running arch's child, read its config content digest, and
		// match the lock's entry for that arch. Under QEMU (arm64) this is the arm64
		// child, never the amd64 entry — the #2025 cross-arch case.
		imgRes, err := pull.PullImage(ctx, pull.ImagePullOptions{Registry: registry, VersionTag: versionID, Pin: pin})
		if err != nil {
			t.Fatalf("pull+verify for the running arch linux/%s: %v", hostArch, err)
		}
		if imgRes.BindingKind != freeze.BindingConfigContentDigest {
			t.Errorf("binding = %q, want %q", imgRes.BindingKind, freeze.BindingConfigContentDigest)
		}
		if imgRes.ImageDigest != wantArchDigest {
			t.Errorf("verified config digest = %q, want the lock's linux/%s entry %q (the running arch's child was selected)", imgRes.ImageDigest, hostArch, wantArchDigest)
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

	// --- POSITIVE: the registry-origin boot re-check selects the running-arch child
	t.Run("registry-origin boot re-check reaches the pull path and resolves the boot ref", func(t *testing.T) {
		rec := &recordingImageRunner{}
		if _, err := runtime.Run(ctx, runtime.Options{
			Store:                 s,
			Name:                  res.Name,
			Version:               versionID,
			ImageRunner:           rec,
			RegistryImageResolver: launchRegistryImageResolver{},
		}); err != nil {
			t.Fatalf("boot the registry-origin plan (must pull+verify the linux/%s child): %v", hostArch, err)
		}
		if rec.spec.Image != wantBootRef {
			t.Errorf("boot spec image = %q, want the verified boot ref %q", rec.spec.Image, wantBootRef)
		}
	})

	// --- NEGATIVE: a tampered per-arch config is refused --------------------
	t.Run("a tampered config digest for the running arch is refused", func(t *testing.T) {
		tampered := pin
		// Replace the running arch's entry with a digest the published image does not
		// carry; the registry serves the honest image, but the (signed-lock)
		// attestation for the selected arch no longer matches, so the per-arch verify
		// must refuse.
		tampered.ConfigDigests = append([]freeze.PlatformDigest(nil), pin.ConfigDigests...)
		for i := range tampered.ConfigDigests {
			if tampered.ConfigDigests[i].Arch == hostArch {
				tampered.ConfigDigests[i].Digest = "sha256:" + strings.Repeat("f", 64)
			}
		}
		_, err := pull.PullImage(ctx, pull.ImagePullOptions{Registry: registry, VersionTag: versionID, Pin: tampered})
		if err == nil {
			t.Fatalf("a pin attesting a different linux/%s config digest must be refused", hostArch)
		}
		if !errors.Is(err, pull.ErrImageDigestMismatch) {
			t.Fatalf("err = %v, want ErrImageDigestMismatch", err)
		}
	})
}
