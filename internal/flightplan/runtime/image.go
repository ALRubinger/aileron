package runtime

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
)

// hasWholePlanImage reports whether the verified pins carry a whole-plan-boot
// image. Every pin is a whole-plan pin: the plan runs in ONE composed
// container (#1839), and tool steps execute as subprocesses inside it
// (#1829), so there is no per-step pin shape anymore.
func hasWholePlanImage(pins []freeze.ImagePin) bool {
	return len(pins) > 0
}

// runInImage boots the verified pinned environment image and runs the frozen
// plan inside it (#1731). It is reached only when the loaded plan carries a
// non-empty ResolvedImages set, so the pin is always present here.
//
// The image booted is taken straight from the verified lock (pins[0]), never a
// re-parse of untrusted bytes: that is the load-bearing security property. A
// plan that pins an image but supplies no ImageRunner is an explicit error, not
// a silent in-process fallback. An environment was declared and pinned; running the
// plan in-process would enter an environment the attestation never certified.
func runInImage(ctx context.Context, lp LoadedPlan, opts Options) (RunResult, error) {
	// The environment resolves exactly one image pin, and the attestation certifies
	// that single environment. Guard the invariant so a future multi-pin environment
	// can never silently boot only pins[0] while ignoring the rest.
	if len(lp.ResolvedImages) != 1 {
		return RunResult{}, fmt.Errorf(
			"flightplan: expected exactly one resolved image pin, got %d", len(lp.ResolvedImages))
	}
	pin := lp.ResolvedImages[0]
	if opts.ImageRunner == nil {
		return RunResult{}, fmt.Errorf(
			"flightplan: frozen unit pins image %q but no image runner is configured", pin.Ref)
	}

	// Registry-origin boot path (#1903): a plan installed by OCI reference has no
	// local build, so its bootable image lives in the recorded registry, not the
	// local daemon. Pull it and verify it against the signed lock pin per the
	// pin's binding kind (config digest for a composed pin, manifest digest for a
	// foreign-base pin) through the RegistryImageResolver seam, then boot the
	// returned reference. This bypasses the local-tag imageRef/#1863 local-daemon
	// guard, which does not apply to a registry pull: the resolver already
	// verified the pulled bytes against the signature-covered pin, which is the
	// same fail-closed property the local guard provides for a locally-built tag.
	// A registry-origin plan with no resolver is a fail-closed error (there is no
	// local tag to boot instead), mirroring the nil-ImageRunner discipline.
	if lp.ImageOrigin.Present {
		if opts.RegistryImageResolver == nil {
			return RunResult{}, fmt.Errorf(
				"flightplan: plan was installed from registry %q but no registry image resolver is configured; cannot pull the published image to boot",
				lp.ImageOrigin.Registry)
		}
		bootRef, err := opts.RegistryImageResolver.Resolve(ctx, lp.ImageOrigin, pin)
		if err != nil {
			return RunResult{}, fmt.Errorf(
				"flightplan: refusing to boot: cannot pull and verify the published image for plan installed from %q: %w",
				lp.ImageOrigin.Registry, err)
		}
		return runBooted(ctx, opts, ImageRunSpec{
			Image:   bootRef,
			Name:    opts.Name,
			Version: opts.Version,
			Inputs:  opts.Inputs,
			OutDir:  opts.OutDir,
		})
	}

	spec := ImageRunSpec{
		Image:   imageRef(pin.Ref, pin.Digest, pin.LocalTag),
		Name:    opts.Name,
		Version: opts.Version,
		Inputs:  opts.Inputs,
		OutDir:  opts.OutDir,
	}
	// Boot-time Id-vs-Digest guard (#1863). A composed-tools pin is booted by
	// its mutable LocalTag (its Digest is a locally-built image Id, not a
	// registry digest, so `ref@digest` would not resolve; see imageRef). The
	// boot therefore trusts the LocalTag string; nothing else re-checks that the
	// daemon image behind that tag STILL resolves to the attested Digest. When a
	// resolver is wired, re-inspect the tag in the local daemon and fail closed
	// unless it resolves to the attested host config digest: a mismatch means the
	// local tag was rebuilt or repointed since freeze, and a resolve error means
	// the attested image is gone. Either way the boot must refuse rather than
	// enter an unverified environment, and the error names `aileron skill freeze`
	// as the recovery verb. Freeze re-attests the host pin from this same
	// daemon-loaded image (#2138), so a fresh freeze->launch always satisfies the
	// guard; only a genuine post-freeze mutation trips it. The guard is scoped to
	// composed pins: for an image-only / custom-base pin the boot target is
	// content-addressed (`ref@digest`) and the daemon resolves the digest itself, so no local-Id
	// comparison applies. A nil resolver skips the guard (backward-compatible).
	//
	// The boot target stays the LocalTag (imageRef's LocalTag-wins semantics,
	// #1856): a locally-built image is addressed by its tag through the CLI's
	// tag-based re-entry and container naming. This guard narrows, but does not
	// eliminate, the trust window: it re-inspects the tag immediately before the
	// boot rather than trusting it unconditionally. A residual TOCTOU (the tag
	// repointed between this Resolve and the runner's boot) is inherent to
	// Docker's mutable-tag model; closing it would require booting by image Id,
	// which is a change to imageRef's boot-target semantics outside this issue.
	if pin.LocalTag != "" && opts.ImageDigestResolver != nil {
		// Select the host platform's config content digest from the composed
		// pin's per-arch set. A plan not built for this host's platform fails
		// closed rather than booting an unattested image: the signed-lock
		// counterpart to the docker-level cross-arch launch message (#2039).
		want, platform, ok := pin.HostConfigDigest()
		if !ok {
			return RunResult{}, fmt.Errorf(
				"flightplan: refusing to boot composed local tag %q: this artifact was not published for %s",
				pin.LocalTag, platform)
		}
		observed, err := opts.ImageDigestResolver.Resolve(ctx, pin.LocalTag)
		if err != nil {
			return RunResult{}, fmt.Errorf(
				"flightplan: refusing to boot composed local tag %q: cannot resolve its digest in the local daemon (attested %s); the frozen image may have been removed — re-run `aileron skill freeze` for this plan to rebuild and re-attest it: %w",
				pin.LocalTag, want, err)
		}
		if observed != want {
			return RunResult{}, fmt.Errorf(
				"flightplan: refusing to boot composed local tag %q: the local image now resolves to config digest %s but the signed lock attested %s, so the tag no longer points at the frozen environment; re-run `aileron skill freeze` for this plan to re-attest the current local image",
				pin.LocalTag, observed, want)
		}
	}
	return runBooted(ctx, opts, spec)
}

// runBooted hands the resolved boot spec to the ImageRunner and maps its result
// onto the public RunResult. Both the local-tag path and the registry-origin
// path funnel through here so the runner contract (boot spec.Image verbatim) and
// the result mapping live in exactly one place.
func runBooted(ctx context.Context, opts Options, spec ImageRunSpec) (RunResult, error) {
	res, err := opts.ImageRunner.Run(ctx, spec)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{
		ContentHash:    res.ContentHash,
		ResolvedInputs: res.ResolvedInputs,
		StepOutputs:    res.StepOutputs,
		Artifacts:      res.Artifacts,
		AuditIDs:       res.AuditIDs,
	}, nil
}

// imageRef composes the image identity the runner boots, reading only from the
// verified lock pin so the boot target is never a re-parse of untrusted bytes.
//
// A composed-tools pin carries a localTag: the local-daemon tag
// (`aileron/sandbox-tools:<hex>`) the composed image actually carries in the
// daemon. That image was built locally, so its recorded digest is the image Id
// rather than a registry digest; `tag@digest` would not resolve. imageRef
// therefore boots the localTag verbatim when present. The tag is a value from
// the signed lock (covered by the content hash and signature), so booting it
// preserves the boot-straight-from-the-verified-lock property.
//
// Otherwise (an image-only or custom-base pin) the ref is a real registry ref
// and the digest a real registry digest, so `ref@digest` is a correct
// content-addressed boot reference: the runner boots the exact image the
// signature attested rather than resolving the mutable tag. An empty digest
// degrades to the bare ref rather than a dangling `@`.
func imageRef(ref, digest, localTag string) string {
	if localTag != "" {
		return localTag
	}
	if digest == "" {
		return ref
	}
	return ref + "@" + digest
}
