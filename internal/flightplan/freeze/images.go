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
	// ResolveDigest resolves an OCI image reference (a tag, for an
	// environment custom base image) to its `sha256:` digest. The returned
	// digest MUST match digestPattern.
	ResolveDigest(ctx context.Context, ref string) (string, error)
}

// FeatureComposer composes a declared reference set onto the Aileron base
// image and returns the built image's `sha256:` digest. It is the seam over
// container.Builder + composition so freeze's composed-environment path is
// testable without Docker.
//
// INTERIM (#1827): the environment bridge passes raw `<name>@<version>` tool
// refs through this seam; the successor resolves them against the curated
// catalog to real devcontainer Feature recipes before composing.
type FeatureComposer interface {
	// ComposeDigest builds the image composed from the given references and
	// returns its `sha256:` digest.
	ComposeDigest(ctx context.Context, features []string) (string, error)
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
type FeatureComposerFunc func(ctx context.Context, features []string) (string, error)

// ComposeDigest calls f, or errors when f is nil (a zero-valued adapter).
func (f FeatureComposerFunc) ComposeDigest(ctx context.Context, features []string) (string, error) {
	if f == nil {
		return "", fmt.Errorf("freeze: nil feature composer")
	}
	return f(ctx, features)
}

// rung3Name is the execution-environment key for rung-3 (per-step
// sibling-image dispatch).
const rung3Name = "rung3PerStepImages"

// resolveImages resolves a manifest's declared environment to the pinned
// image set and resolved capability set the lockfile records. The cases:
//
//   - instruction-only / no environment block: empty pins, no error
//     (the skill still gets a contentHash and signature);
//   - environment.image alone: resolve the custom base image to a digest pin;
//   - environment.tools alone: compose the declared tools and pin the built
//     image's digest plus the resolved capability set;
//   - both declared: refused until #1827 lands tools-onto-custom-base
//     composition (never silently drop declared tools from a signed pin).
//
// Every resolved digest is checked against digestPattern: a resolver that
// yields a tag rather than a digest is rejected (pin by digest, never tag).
// cliVersion is the Aileron CLI build version; the retained legacy rung
// branches below use it to resolve the default runner image
// (composition.BaseImage). It is distinct from the skill's semver label
// recorded in the lock (Options.Version).
//
// The rung1Image/rung2CapabilityUnits/rung3PerStepImages branches below are
// DORMANT: the schema no longer admits requires.executionEnvironment, so
// manifest.Parse can never populate the field they read. They are retained,
// with their direct-construct tests, for #1827 (freeze) and #1829 (runtime)
// to rewire or delete.
func resolveImages(ctx context.Context, m *manifest.Manifest, dr DigestResolver, fc FeatureComposer, cliVersion string) (pins []ImagePin, capSet []string, err error) {
	if m == nil || m.InstructionOnly {
		return nil, nil, nil
	}

	// INTERIM (#1827): the environment block is bridged onto the existing
	// resolver/composer seams so the tree stays green while the real
	// catalog-resolved single-composition path lands. Tool refs pass through
	// RAW (no catalog resolution to Feature recipes, no runner-base
	// inclusion) and a custom base with declared tools is refused.
	if env := m.Aileron.Environment; env != nil {
		if env.Image != "" {
			// INTERIM (#1827): composing declared tools onto a custom base is
			// the successor's job. Until it lands, a block declaring both is
			// refused outright: silently freezing a signed artifact that
			// omits declared tools would be capability drift.
			if len(env.Tools) > 0 {
				return nil, nil, fmt.Errorf("freeze: environment declares both image and tools; composing tools onto a custom base is not yet supported (#1827)")
			}
			if dr == nil {
				return nil, nil, fmt.Errorf("freeze: environment image %q requires a digest resolver", env.Image)
			}
			digest, err := dr.ResolveDigest(ctx, env.Image)
			if err != nil {
				return nil, nil, fmt.Errorf("freeze: resolve environment image %q: %w", env.Image, err)
			}
			if err := requireDigest(env.Image, digest); err != nil {
				return nil, nil, err
			}
			return []ImagePin{{Ref: env.Image, Digest: digest}}, nil, nil
		}
		tools := append([]string(nil), env.Tools...)
		if len(tools) == 0 {
			// The schema requires tools or image on a present block; this is
			// the freeze-time defensive backstop for a direct construct.
			return nil, nil, fmt.Errorf("freeze: environment declares neither tools nor image")
		}
		// INTERIM (#1827): the raw <name>@<version> refs go straight through
		// the composer seam and the resolved capability set records the
		// declared tools verbatim.
		if fc == nil {
			return nil, nil, fmt.Errorf("freeze: environment tools require a feature composer")
		}
		digest, err := fc.ComposeDigest(ctx, tools)
		if err != nil {
			return nil, nil, fmt.Errorf("freeze: compose environment tools: %w", err)
		}
		if err := requireDigest("environment:"+joinFeatures(tools), digest); err != nil {
			return nil, nil, err
		}
		return []ImagePin{{Ref: composedToolsRef(tools), Digest: digest}}, tools, nil
	}

	env := m.Aileron.Requires.ExecutionEnvironment
	if len(env) == 0 {
		// No environment declared: an instruction/composition-only skill
		// freezes with an empty resolvedImages set.
		return nil, nil, nil
	}

	rung1, hasRung1 := env["rung1Image"]
	rung2, hasRung2 := env["rung2CapabilityUnits"]
	rung3, hasRung3 := env[rung3Name]
	switch {
	case moreThanOne(hasRung1, hasRung2, hasRung3):
		return nil, nil, fmt.Errorf("freeze: executionEnvironment declares more than one of rung1Image, rung2CapabilityUnits, rung3PerStepImages; exactly one is permitted")
	case hasRung1:
		ref, err := rung1ImageRef(rung1)
		if err != nil {
			return nil, nil, err
		}
		if ref == "" {
			// No ref declared: resolve the Aileron-provided runner image for the
			// freezing CLI's version (#1808). The default flows through the same
			// named-ref resolution below (resolver, requireDigest, digest pin), so
			// the lock records the concrete resolved ref plus digest exactly as a
			// named ref does.
			ref = composition.BaseImage(cliVersion)
		}
		if dr == nil {
			return nil, nil, fmt.Errorf("freeze: rung-1 image %q requires a digest resolver", ref)
		}
		digest, err := dr.ResolveDigest(ctx, ref)
		if err != nil {
			return nil, nil, fmt.Errorf("freeze: resolve rung-1 image %q: %w", ref, err)
		}
		if err := requireDigest(ref, digest); err != nil {
			return nil, nil, err
		}
		return []ImagePin{{Ref: ref, Digest: digest}}, nil, nil
	case hasRung2:
		features, err := rung2Features(rung2)
		if err != nil {
			return nil, nil, err
		}
		if fc == nil {
			return nil, nil, fmt.Errorf("freeze: rung-2 capability units require a feature composer")
		}
		digest, err := fc.ComposeDigest(ctx, features)
		if err != nil {
			return nil, nil, fmt.Errorf("freeze: compose rung-2 capability units: %w", err)
		}
		if err := requireDigest("rung2:"+joinFeatures(features), digest); err != nil {
			return nil, nil, err
		}
		// The pre-freeze "ref" for a composed image is the feature set; the
		// resolved capability set is the same features, pinned.
		return []ImagePin{{Ref: composedRef(features), Digest: digest}}, append([]string(nil), features...), nil
	case hasRung3:
		// Rung-3 (per-step sibling-image dispatch) is a built image rung
		// (ADR-0027): freeze resolves each step's sibling image to a digest
		// and pins one ImagePin per step. The pins flow into resolvedImages
		// alongside the rung-1/rung-2 pins, so the plan content hash and
		// signature cover them.
		steps, err := rung3StepImages(rung3)
		if err != nil {
			return nil, nil, err
		}
		if dr == nil {
			return nil, nil, fmt.Errorf("freeze: rung-3 per-step images require a digest resolver")
		}
		out := make([]ImagePin, 0, len(steps))
		for _, s := range steps {
			digest, err := dr.ResolveDigest(ctx, s.image)
			if err != nil {
				return nil, nil, fmt.Errorf("freeze: resolve rung-3 image %q: %w", s.image, err)
			}
			if err := requireDigest(s.image, digest); err != nil {
				return nil, nil, err
			}
			// Stamp the step's declared id (or its positional fallback) onto the
			// pin so the runtime associates a step to its pin by id, never by the
			// shared image tag (the #1739 tag-collision ambiguity). The declared
			// trust-contract hosts are sealed onto the pin so the reach is covered
			// by the content hash and signature (#1775).
			out = append(out, ImagePin{Ref: s.image, Digest: digest, StepID: s.id, Hosts: s.hosts})
		}
		return out, nil, nil
	default:
		return nil, nil, fmt.Errorf("freeze: executionEnvironment declares no image rung (rung1Image, rung2CapabilityUnits, or rung3PerStepImages)")
	}
}

// moreThanOne reports whether more than one of the given booleans is true. It
// is the exactly-one-rung guard: the schema already forbids two image rungs,
// so this is the freeze-time defensive backstop.
func moreThanOne(bs ...bool) bool {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n > 1
}

// requireDigest enforces the pin-by-digest invariant: a resolved value that
// is not a `sha256:` digest is rejected with a clear error.
func requireDigest(ref, digest string) error {
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("freeze: resolved %q to %q which is not a sha256: digest (freeze pins by digest, never by tag)", ref, digest)
	}
	return nil
}

// rung1ImageRef extracts rung1Image.ref from the untyped execution
// environment block. An absent ref key is the default-runner contract
// (#1808): it returns ("", nil), meaning "resolve the Aileron-provided
// runner image for this CLI version". A ref key that IS present but is not a
// real, non-empty string (non-string scalar, null, empty, or whitespace-only)
// is a hard error, not a default: coercing it into an image reference would
// silently produce a bad pin, and an explicit empty-string ref stays rejected
// exactly as the schema rejects it.
func rung1ImageRef(v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", fmt.Errorf("freeze: rung1Image is not a mapping")
	}
	raw, present := m["ref"]
	if !present {
		// No ref declared: use the default Aileron runner image (#1808).
		return "", nil
	}
	ref, ok := raw.(string)
	if !ok || strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("freeze: rung1Image.ref is present but empty or not a string")
	}
	return strings.TrimSpace(ref), nil
}

// rung2Features extracts rung2CapabilityUnits.features from the untyped
// execution environment block. Each Feature reference MUST be a real,
// non-empty string; a non-string entry is rejected rather than stringified.
func rung2Features(v any) ([]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("freeze: rung2CapabilityUnits is not a mapping")
	}
	rawList, ok := m["features"].([]any)
	if !ok || len(rawList) == 0 {
		return nil, fmt.Errorf("freeze: rung2CapabilityUnits.features is missing or empty")
	}
	features := make([]string, 0, len(rawList))
	for _, f := range rawList {
		s, ok := f.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("freeze: rung2CapabilityUnits.features contains an empty or non-string entry")
		}
		features = append(features, strings.TrimSpace(s))
	}
	return features, nil
}

// rung3StepImage is one rung-3 step's freeze-relevant fields: the sibling
// image reference, the id used to link the resolved pin back to the step, and
// the declared trust-contract hosts freeze seals onto the pin (nil when the
// step declares no trust contract).
type rung3StepImage struct {
	id    string
	image string
	hosts []string
}

// rung3StepImages extracts each rung3PerStepImages.steps[] entry from the
// untyped execution environment block, returning the per-step image reference
// and linking id in declared order. Each step MUST be a mapping carrying a
// non-empty string image; a non-mapping step, a missing/empty steps list, or a
// non-string/empty image is rejected rather than stringified so a bad step
// never silently produces a wrong pin. The `id` is optional in the schema: when
// a step declares one it is carried onto the pin; when absent, a positional
// fallback (`#<index>`) is used so every pin still traces to exactly one step
// and the runtime can associate positionally.
func rung3StepImages(v any) ([]rung3StepImage, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("freeze: rung3PerStepImages is not a mapping")
	}
	rawList, ok := m["steps"].([]any)
	if !ok || len(rawList) == 0 {
		return nil, fmt.Errorf("freeze: rung3PerStepImages.steps is missing or empty")
	}
	steps := make([]rung3StepImage, 0, len(rawList))
	for i, s := range rawList {
		step, ok := s.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("freeze: rung3PerStepImages.steps contains a non-mapping entry")
		}
		img, ok := step["image"].(string)
		if !ok || strings.TrimSpace(img) == "" {
			return nil, fmt.Errorf("freeze: rung3PerStepImages.steps contains a step with an empty or non-string image")
		}
		id := rung3StepID(step, i)
		hosts, err := rung3StepHosts(step)
		if err != nil {
			return nil, err
		}
		steps = append(steps, rung3StepImage{id: id, image: strings.TrimSpace(img), hosts: hosts})
	}
	return steps, nil
}

// rung3StepHosts extracts the declared trust-contract hosts from a rung-3
// step's untyped map. It returns nil when the step declares no trustContract
// (an optional block: absent means the step declares no reach). A declared
// trustContract whose hosts are missing, empty, or carry a non-string/empty
// entry is rejected rather than sealing a partial reach: the same guard the
// image extraction applies, so a malformed reach never silently produces a
// wrong pin. The schema's hosts minItems:1 rejects an empty declared list
// before freeze runs; this is the freeze-time defensive backstop.
func rung3StepHosts(step map[string]any) ([]string, error) {
	raw, ok := step["trustContract"]
	if !ok || raw == nil {
		return nil, nil
	}
	tc, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("freeze: rung3PerStepImages.steps contains a step whose trustContract is not a mapping")
	}
	hostsRaw, ok := tc["hosts"]
	if !ok || hostsRaw == nil {
		return nil, fmt.Errorf("freeze: rung3PerStepImages.steps declares a trustContract with no hosts (access scope is the boundary)")
	}
	list, ok := hostsRaw.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("freeze: rung3PerStepImages.steps declares a trustContract with an empty or non-array hosts")
	}
	hosts := make([]string, 0, len(list))
	for _, h := range list {
		s, ok := h.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("freeze: rung3PerStepImages.steps declares a trustContract host that is empty or not a string")
		}
		hosts = append(hosts, strings.TrimSpace(s))
	}
	return hosts, nil
}

// rung3StepID returns a step's declared `id` when present and a non-empty
// string, or a positional fallback (`#<index>`) otherwise. The schema makes
// `id` optional; a non-string or empty id is treated as absent (the schema's
// minLength:1 rejects an empty string before freeze runs, so this is the
// defensive backstop) and the positional fallback keeps the pin traceable.
func rung3StepID(step map[string]any, index int) string {
	if raw, ok := step["id"].(string); ok {
		if id := strings.TrimSpace(raw); id != "" {
			return id
		}
	}
	return fmt.Sprintf("#%d", index)
}

// composedRef renders a stable pre-freeze reference for a composed rung-2
// image so the lock entry records what was composed.
func composedRef(features []string) string {
	return "aileron-base+features(" + joinFeatures(features) + ")"
}

// composedToolsRef renders a stable pre-freeze reference for an image
// composed from declared environment tools so the lock entry records what
// was composed.
func composedToolsRef(tools []string) string {
	return "aileron-base+tools(" + joinFeatures(tools) + ")"
}

func joinFeatures(features []string) string {
	out := ""
	for i, f := range features {
		if i > 0 {
			out += ","
		}
		out += f
	}
	return out
}
