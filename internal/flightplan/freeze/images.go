package freeze

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
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
	// ResolveDigest resolves an OCI image reference (a tag, for rung-1) to
	// its `sha256:` digest. The returned digest MUST match digestPattern.
	ResolveDigest(ctx context.Context, ref string) (string, error)
}

// FeatureComposer composes a rung-2 capability-unit Feature set onto the
// Aileron base image and returns the built image's `sha256:` digest. It is
// the seam over container.Builder + composition so freeze's rung-2 path is
// testable without Docker.
type FeatureComposer interface {
	// ComposeDigest builds the image composed from the given devcontainer
	// Feature references and returns its `sha256:` digest.
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

// resolveImages resolves a manifest's execution environment to the pinned
// image set and resolved capability set the lockfile records. The cases:
//
//   - instruction-only / no executionEnvironment: empty pins, no error
//     (the skill still gets a contentHash and signature);
//   - rung-1 (rung1Image.ref): resolve the named image to a digest pin;
//   - rung-2 (rung2CapabilityUnits.features): compose the Features and pin
//     the built image's digest plus the resolved capability set;
//   - rung-3 (rung3PerStepImages.steps[].image): resolve each per-step
//     sibling image to a digest pin, one ImagePin per step (ADR-0027);
//   - more than one image rung present, or an executionEnvironment naming
//     no rung: a malformed manifest the schema already rejects; guarded here
//     defensively.
//
// Every resolved digest is checked against digestPattern: a resolver that
// yields a tag rather than a digest is rejected (pin by digest, never tag).
func resolveImages(ctx context.Context, m *manifest.Manifest, dr DigestResolver, fc FeatureComposer) (pins []ImagePin, capSet []string, err error) {
	if m == nil || m.InstructionOnly {
		return nil, nil, nil
	}
	env := m.Aileron.Requires.ExecutionEnvironment
	if len(env) == 0 {
		// No execution environment declared: an instruction/composition-only
		// skill freezes with an empty resolvedImages set.
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
		refs, err := rung3StepImages(rung3)
		if err != nil {
			return nil, nil, err
		}
		if dr == nil {
			return nil, nil, fmt.Errorf("freeze: rung-3 per-step images require a digest resolver")
		}
		out := make([]ImagePin, 0, len(refs))
		for _, ref := range refs {
			digest, err := dr.ResolveDigest(ctx, ref)
			if err != nil {
				return nil, nil, fmt.Errorf("freeze: resolve rung-3 image %q: %w", ref, err)
			}
			if err := requireDigest(ref, digest); err != nil {
				return nil, nil, err
			}
			out = append(out, ImagePin{Ref: ref, Digest: digest})
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
// environment block. The ref MUST be a real string: a non-string scalar
// coerced into an image reference would silently produce a bad pin, so a
// non-string ref is rejected here rather than stringified.
func rung1ImageRef(v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", fmt.Errorf("freeze: rung1Image is not a mapping")
	}
	ref, ok := m["ref"].(string)
	if !ok || strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("freeze: rung1Image.ref is missing, empty, or not a string")
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

// rung3StepImages extracts each rung3PerStepImages.steps[].image from the
// untyped execution environment block, returning the per-step image
// references in declared order. Each step MUST be a mapping carrying a
// non-empty string image; a non-mapping step, a missing/empty steps list, or
// a non-string/empty image is rejected rather than stringified so a bad step
// never silently produces a wrong pin. It mirrors rung2Features.
func rung3StepImages(v any) ([]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("freeze: rung3PerStepImages is not a mapping")
	}
	rawList, ok := m["steps"].([]any)
	if !ok || len(rawList) == 0 {
		return nil, fmt.Errorf("freeze: rung3PerStepImages.steps is missing or empty")
	}
	refs := make([]string, 0, len(rawList))
	for _, s := range rawList {
		step, ok := s.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("freeze: rung3PerStepImages.steps contains a non-mapping entry")
		}
		img, ok := step["image"].(string)
		if !ok || strings.TrimSpace(img) == "" {
			return nil, fmt.Errorf("freeze: rung3PerStepImages.steps contains a step with an empty or non-string image")
		}
		refs = append(refs, strings.TrimSpace(img))
	}
	return refs, nil
}

// composedRef renders a stable pre-freeze reference for a composed rung-2
// image so the lock entry records what was composed.
func composedRef(features []string) string {
	return "aileron-base+features(" + joinFeatures(features) + ")"
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
