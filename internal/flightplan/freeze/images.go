package freeze

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"github.com/ALRubinger/aileron/internal/sandbox/composition"
)

// digestPattern is the schema's content-addressed digest shape. A
// resolution that does not produce this exact shape is a drift vector
// (a tag pin would let the underlying image move) and is a hard error.
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// DigestResolver resolves a pre-freeze image reference to a
// content-addressed `sha256:` digest. It is the seam that keeps the freeze
// core free of a container runtime: production wires a resolver over
// container.Runner / container.Builder, tests inject a fake.
type DigestResolver interface {
	// ResolveDigest resolves an OCI image reference (a tag: the runner base
	// or an environment custom base image) to its `sha256:` digest. The
	// returned digest MUST match digestPattern.
	ResolveDigest(ctx context.Context, ref string) (string, error)
}

// ComposeResult is what a FeatureComposer returns: the per-arch OCI-layout
// config digests plus the host-arch daemon-loaded image's config digest.
type ComposeResult struct {
	// PerArch is the per-arch set of serialization-agnostic config content
	// digests read from the multi-arch OCI layout, one entry per built platform
	// (e.g. linux/amd64 and linux/arm64). freeze records the whole set as the
	// composed pin's ConfigDigests, the publish/cross-machine pull identity.
	PerArch []PlatformDigest
	// LocalHostConfigDigest is the config content digest of the host-arch image
	// the composer loaded into the local daemon under the composed LocalTag,
	// resolved through the SAME localImageContentDigest the #1863 boot guard
	// uses. freeze records it as the pin's LocalHostConfigDigest so the local
	// no-publish boot compares apples-to-apples against the daemon image rather
	// than the OCI-layout child (which can legitimately differ, #2138). Empty
	// when the composer did not perform a daemon load (e.g. a fake in tests), in
	// which case the boot guard falls back to the host ConfigDigests entry.
	LocalHostConfigDigest string
}

// FeatureComposer composes a devcontainer Feature set onto a base image and
// returns the built image's `sha256:` digest. It is the seam over
// container.Builder + composition so freeze's composed-environment path is
// testable without Docker. base is a digest-pinned image reference
// (`<ref>@sha256:...`): freeze resolves the base digest BEFORE composing so
// the compose input can never drift under a floating tag. features are the
// catalog-resolved published Feature references (see
// composition.ResolveToolFeatureRef), never raw `<name>@<version>` tool refs.
type FeatureComposer interface {
	// ComposeDigest builds the image composed from the given Features onto base
	// for every supported architecture and returns the per-arch set of
	// serialization-agnostic config content digests (one PlatformDigest per built
	// platform, e.g. linux/amd64 and linux/arm64) plus the host-arch daemon-loaded
	// image's config content digest. The freeze producer records the per-arch set
	// as the composed pin's ConfigDigests, so a plan launches on either
	// architecture (ADR-0027, issue #2036), and the host-arch daemon digest as
	// LocalHostConfigDigest for the local no-publish boot compare (#2138). Every
	// PerArch entry's Digest MUST be a `sha256:` content digest; a tag is rejected.
	ComposeDigest(ctx context.Context, base string, features []string) (ComposeResult, error)
}

// DigestResolverFunc adapts a function to DigestResolver.
type DigestResolverFunc func(ctx context.Context, ref string) (string, error)

// ResolveDigest calls f, or errors when f is nil (a zero-valued adapter).
func (f DigestResolverFunc) ResolveDigest(ctx context.Context, ref string) (string, error) {
	if f == nil {
		return "", fmt.Errorf("freeze: nil digest resolver")
	}
	return f(ctx, ref)
}

// ComposerPreflighter is the optional capability a FeatureComposer may
// implement to validate the multi-architecture build toolchain BEFORE any
// image work. resolveImages runs it (when present) ahead of the base-image
// pull so a missing/unregistered multi-arch toolchain aborts the freeze with a
// guided remediation and no wasted pull — rather than surfacing only after the
// base image has been fetched. Preflight MUST be idempotent and side-effect
// free beyond the transparent build-environment provisioning ComposeDigest
// already performs, since ComposeDigest may run the same checks again. A
// composer that does not implement this interface is preflighted implicitly by
// ComposeDigest, matching the prior behavior.
type ComposerPreflighter interface {
	// Preflight validates that the multi-architecture composed build can run.
	// It returns a guided, actionable error on a miss (for example, the QEMU
	// emulators are not registered) and nil when the toolchain is ready.
	Preflight(ctx context.Context) error
}

// FeatureComposerFunc adapts a function to FeatureComposer.
type FeatureComposerFunc func(ctx context.Context, base string, features []string) (ComposeResult, error)

// ComposeDigest calls f, or errors when f is nil (a zero-valued adapter).
func (f FeatureComposerFunc) ComposeDigest(ctx context.Context, base string, features []string) (ComposeResult, error) {
	if f == nil {
		return ComposeResult{}, fmt.Errorf("freeze: nil feature composer")
	}
	return f(ctx, base, features)
}

// resolveImages resolves a manifest's declared environment to the pinned
// image and resolved capability set the lockfile records. The plan runs in
// one container, so every case yields at most ONE pin:
//
//   - instruction-only / no environment block: empty pins, no error
//     (the skill still gets a contentHash and signature);
//   - environment.image alone: resolve the custom base image to a digest pin;
//   - environment.tools declared (with or without a custom base): resolve the
//     base — the declared custom image, or the Aileron runner base for
//     cliVersion (composition.BaseImage) — to its digest, resolve each tool
//     ref against the curated catalog to its published Feature reference,
//     compose the Features onto the digest-pinned base, and pin the built
//     image's digest. The resolved capability set records the declared tools
//     verbatim (reproducibility comes from the composed digest pin, not the
//     Feature tag).
//
// Every resolved digest is checked against digestPattern: a resolver or
// composer that yields a tag rather than a digest is rejected (pin by digest,
// never tag). cliVersion is the Aileron CLI build version; it selects the
// default runner-base tag the tools path composes onto when no custom image
// is declared. It is distinct from the skill's semver label recorded in the
// lock (Options.Version).
func resolveImages(ctx context.Context, m *manifest.Manifest, dr DigestResolver, fc FeatureComposer, cliVersion string) (pins []ImagePin, capSet []string, err error) {
	if m == nil || m.InstructionOnly {
		return nil, nil, nil
	}
	env := m.Aileron.Environment
	if env == nil {
		// No environment declared: an instruction/composition-only skill
		// freezes with an empty resolvedImages set.
		return nil, nil, nil
	}

	tools := append([]string(nil), env.Tools...)
	if len(tools) == 0 && env.Image == "" {
		// The schema requires tools or image on a present block; this is
		// the freeze-time defensive backstop for a direct construct.
		return nil, nil, fmt.Errorf("freeze: environment declares neither tools nor image")
	}

	// Resolve declared tool refs against the curated catalog and check the
	// seams FIRST, so a bad manifest or a missing adapter fails before any
	// container work.
	featureRefs := make([]string, 0, len(tools))
	for _, tool := range tools {
		featureRef, err := composition.ResolveToolFeatureRef(tool)
		if err != nil {
			return nil, nil, fmt.Errorf("freeze: resolve environment tool: %w", err)
		}
		featureRefs = append(featureRefs, featureRef)
	}
	if len(tools) > 0 && fc == nil {
		return nil, nil, fmt.Errorf("freeze: environment tools require a feature composer")
	}

	// Preflight the multi-arch build toolchain BEFORE pulling the base image, so a
	// missing/unregistered emulator set aborts the freeze fast with a guided
	// remediation and no wasted base-image pull. Only the tools path composes a
	// multi-arch image, so only it preflights; the image-only path never reaches
	// here. The composer implicitly re-checks inside ComposeDigest, so a composer
	// that does not implement ComposerPreflighter still fails closed — it just
	// surfaces after the pull, as before.
	if len(tools) > 0 {
		if pf, ok := fc.(ComposerPreflighter); ok {
			if err := pf.Preflight(ctx); err != nil {
				return nil, nil, fmt.Errorf("freeze: preflight multi-arch build: %w", err)
			}
		}
	}

	// Resolve the base image to its digest. Both paths need it: the
	// image-only path pins it directly, and the tools path composes onto the
	// digest-pinned base so the compose input can never drift under a
	// floating tag.
	base := env.Image
	if base == "" {
		base = composition.BaseImage(cliVersion)
	}
	if dr == nil {
		return nil, nil, fmt.Errorf("freeze: environment base image %q requires a digest resolver", base)
	}
	baseDigest, err := dr.ResolveDigest(ctx, base)
	if err != nil {
		return nil, nil, fmt.Errorf("freeze: resolve environment base image %q: %w", base, err)
	}
	if err := requireDigest(base, baseDigest); err != nil {
		return nil, nil, err
	}

	if len(tools) == 0 {
		// environment.image alone: the digest-resolved custom base IS the
		// plan image.
		return []ImagePin{{Ref: env.Image, Digest: baseDigest}}, nil, nil
	}

	// Compose the catalog-resolved Feature references onto the digest-pinned
	// base for every supported architecture.
	pinnedBase := base + "@" + baseDigest
	composed, err := fc.ComposeDigest(ctx, pinnedBase, featureRefs)
	if err != nil {
		return nil, nil, fmt.Errorf("freeze: compose environment tools: %w", err)
	}
	configDigests := composed.PerArch
	if len(configDigests) == 0 {
		return nil, nil, fmt.Errorf("freeze: compose environment tools: composer returned no per-arch config digests")
	}
	// Every per-arch entry must pin by content digest, never a tag; a drifting
	// entry is rejected before the pin is recorded.
	for _, cd := range configDigests {
		if err := requireDigest("environment:"+strings.Join(tools, ",")+" ("+cd.OS+"/"+cd.Arch+")", cd.Digest); err != nil {
			return nil, nil, err
		}
	}
	// The host-arch daemon-loaded image's config digest is the local boot
	// target. When the composer recorded it (a real daemon load), enforce the
	// same pin-by-digest invariant so a drifting value is rejected before the
	// pin is recorded. An empty value is allowed (a fake composer that did not
	// load), in which case the boot guard falls back to the host ConfigDigests
	// entry.
	if composed.LocalHostConfigDigest != "" {
		if err := requireDigest("environment:"+strings.Join(tools, ",")+" (local host daemon image)", composed.LocalHostConfigDigest); err != nil {
			return nil, nil, err
		}
	}
	// The composed pin carries four identities: the descriptive Ref (what was
	// composed onto which pinned base, for errors/audit), the attesting per-arch
	// ConfigDigests set (the multi-arch OCI-layout config content digests, the
	// publish/cross-machine pull identity), the bootable LocalTag (the local-daemon
	// tag the composed image actually carries, so the runtime can boot it), and the
	// LocalHostConfigDigest (the host-arch DAEMON-loaded image's config digest, the
	// local no-publish boot compare target — see #2138: the daemon-load and
	// OCI-layout builds can legitimately diverge). The composer builds a real
	// multi-arch image and returns one PerArch entry per built platform
	// (linux/amd64 and linux/arm64); the whole set is recorded so a plan launches
	// on either host architecture (ADR-0027). LocalTag is computed from the SAME
	// pinnedBase and catalog-resolved featureRefs the composer received, so it is
	// byte-identical to composition.ToolsPlan(pinnedBase, featureRefs).Image;
	// LocalToolsImageTag sorts internally, so declaration order does not matter.
	return []ImagePin{{
		Ref:                   composedToolsRef(pinnedBase, tools),
		ConfigDigests:         sortedConfigDigests(configDigests),
		LocalTag:              composition.LocalToolsImageTag(pinnedBase, featureRefs),
		LocalHostConfigDigest: composed.LocalHostConfigDigest,
	}}, tools, nil
}

// requireDigest enforces the pin-by-digest invariant: a resolved value that
// is not a `sha256:` digest is rejected with a clear error.
func requireDigest(ref, digest string) error {
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("freeze: resolved %q to %q which is not a sha256: digest (freeze pins by digest, never by tag)", ref, digest)
	}
	return nil
}

// composedToolsRef renders a stable, descriptive pre-freeze reference for an
// image composed from declared environment tools, so the lock entry records
// what was composed and onto which digest-pinned base. This Ref is descriptive
// and not itself bootable: the composed pin carries a separate LocalTag with
// the daemon-resolvable local-daemon tag the runtime boots.
func composedToolsRef(pinnedBase string, tools []string) string {
	return pinnedBase + "+tools(" + strings.Join(tools, ",") + ")"
}
