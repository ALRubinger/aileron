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
	// platform, e.g. linux/amd64 and linux/arm64). The freeze producer records the
	// whole set as the composed pin's ConfigDigests, so a plan launches on either
	// architecture (ADR-0027, issue #2036). The host-platform entry is attested
	// from the image the daemon-load build actually loaded under LocalTag, which is
	// the exact image the local (no-publish) launch boots and re-checks, so the
	// boot guard's observed digest equals the recorded pin by construction (issue
	// #2138). Every entry's Digest MUST be a `sha256:` content digest; a tag is
	// rejected.
	ComposeDigest(ctx context.Context, base string, features []string) ([]PlatformDigest, error)
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

// FeatureComposerFunc adapts a function to FeatureComposer.
type FeatureComposerFunc func(ctx context.Context, base string, features []string) ([]PlatformDigest, error)

// ComposeDigest calls f, or errors when f is nil (a zero-valued adapter).
func (f FeatureComposerFunc) ComposeDigest(ctx context.Context, base string, features []string) ([]PlatformDigest, error) {
	if f == nil {
		return nil, fmt.Errorf("freeze: nil feature composer")
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
	configDigests, err := fc.ComposeDigest(ctx, pinnedBase, featureRefs)
	if err != nil {
		return nil, nil, fmt.Errorf("freeze: compose environment tools: %w", err)
	}
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
	// The composed pin carries three identities: the descriptive Ref (what was
	// composed onto which pinned base, for errors/audit), the attesting per-arch
	// ConfigDigests set (the composed image's serialization-agnostic config
	// content digest, keyed by platform), and the bootable LocalTag (the
	// local-daemon tag the composed image actually carries, so the runtime can
	// boot it). The composer builds a real multi-arch image and returns one entry
	// per built platform (linux/amd64 and linux/arm64); the whole set is recorded
	// so a plan launches on either host architecture (ADR-0027). LocalTag is
	// computed from the SAME pinnedBase and catalog-resolved featureRefs the
	// composer received, so it is byte-identical to
	// composition.ToolsPlan(pinnedBase, featureRefs).Image; LocalToolsImageTag
	// sorts internally, so declaration order does not matter.
	return []ImagePin{{
		Ref:           composedToolsRef(pinnedBase, tools),
		ConfigDigests: sortedConfigDigests(configDigests),
		LocalTag:      composition.LocalToolsImageTag(pinnedBase, featureRefs),
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
