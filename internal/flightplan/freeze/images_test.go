package freeze

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

func parseM(t *testing.T, raw []byte) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestResolveImages_Rung1ResolvesToDigest(t *testing.T) {
	m := parseM(t, []byte(rung1MD))
	var gotRef string
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		gotRef = ref
		return fakeDigest, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, dr, nil)
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if gotRef != "registry.example.com/runner:1.4" {
		t.Errorf("resolver got ref %q", gotRef)
	}
	if len(pins) != 1 || pins[0].Digest != fakeDigest || pins[0].Ref != "registry.example.com/runner:1.4" {
		t.Errorf("pins = %+v", pins)
	}
	if capSet != nil {
		t.Errorf("rung-1 must have no capability set, got %v", capSet)
	}
}

func TestResolveImages_Rung2ComposesAndPins(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	var gotFeatures []string
	fc := FeatureComposerFunc(func(_ context.Context, features []string) (string, error) {
		gotFeatures = features
		return fakeDigest, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, nil, fc)
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	wantFeatures := []string{
		"ghcr.io/example/aileron-feature-metrics-cli:1",
		"ghcr.io/example/aileron-feature-tracker-cli:1",
	}
	if strings.Join(gotFeatures, ",") != strings.Join(wantFeatures, ",") {
		t.Errorf("composer got features %v, want %v", gotFeatures, wantFeatures)
	}
	if len(pins) != 1 || pins[0].Digest != fakeDigest {
		t.Errorf("pins = %+v", pins)
	}
	if strings.Join(capSet, ",") != strings.Join(wantFeatures, ",") {
		t.Errorf("resolved capability set = %v, want %v", capSet, wantFeatures)
	}
}

func TestResolveImages_InstructionOnlyEmpty(t *testing.T) {
	m := parseM(t, []byte(instructionOnlyMD))
	pins, capSet, err := resolveImages(context.Background(), m, nil, nil)
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if len(pins) != 0 || len(capSet) != 0 {
		t.Errorf("instruction-only must resolve to empty, got pins=%v cap=%v", pins, capSet)
	}
}

func TestResolveImages_NoExecEnvEmpty(t *testing.T) {
	m := parseM(t, []byte(noExecEnvMD))
	pins, _, err := resolveImages(context.Background(), m, nil, nil)
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if len(pins) != 0 {
		t.Errorf("no-execution-environment skill must resolve to empty, got %v", pins)
	}
}

func TestResolveImages_RejectsTagNotDigest(t *testing.T) {
	m := parseM(t, []byte(rung1MD))
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "registry.example.com/runner:1.4", nil // a tag, not a digest
	})
	_, _, err := resolveImages(context.Background(), m, dr, nil)
	if err == nil {
		t.Fatal("a resolver returning a tag must be rejected")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error should explain the pin-by-digest rule, got: %v", err)
	}
}

func TestResolveImages_ResolverError(t *testing.T) {
	m := parseM(t, []byte(rung1MD))
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("image not found")
	})
	if _, _, err := resolveImages(context.Background(), m, dr, nil); err == nil {
		t.Error("an unresolvable image must error")
	}
}

func TestResolveImages_Rung1NeedsResolver(t *testing.T) {
	m := parseM(t, []byte(rung1MD))
	if _, _, err := resolveImages(context.Background(), m, nil, nil); err == nil {
		t.Error("a rung-1 manifest with no resolver must error")
	}
}

func TestResolveImages_Rung2NeedsComposer(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	if _, _, err := resolveImages(context.Background(), m, nil, nil); err == nil {
		t.Error("a rung-2 manifest with no composer must error")
	}
}

func TestRequireDigest(t *testing.T) {
	if err := requireDigest("ref", fakeDigest); err != nil {
		t.Errorf("valid digest rejected: %v", err)
	}
	for _, bad := range []string{"latest", "sha256:short", "sha256:" + strings.Repeat("g", 64), ""} {
		if err := requireDigest("ref", bad); err == nil {
			t.Errorf("bad digest %q accepted", bad)
		}
	}
}

// TestResolveImages_Rung3ResolvesPerStepDigests is the #1732 acceptance proof
// for rung-3: a manifest declaring rung-3 per-step images resolves each step's
// sibling image to a digest pin (one ImagePin per step), pins no capability
// set, and does not error. A fake resolver maps each step image to a distinct
// digest so the per-step pinning is observable.
func TestResolveImages_Rung3ResolvesPerStepDigests(t *testing.T) {
	m := parseM(t, []byte(rung3MultiStepMD))
	digestFor := map[string]string{
		"registry.example.com/tool-a:1": fakeDigest,
		"registry.example.com/tool-b:2": fakeDigest2,
	}
	var gotRefs []string
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		gotRefs = append(gotRefs, ref)
		return digestFor[ref], nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, dr, nil)
	if err != nil {
		t.Fatalf("rung-3 resolution must succeed: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("rung-3 must pin one image per step, got %+v", pins)
	}
	if pins[0].Ref != "registry.example.com/tool-a:1" || pins[0].Digest != fakeDigest {
		t.Errorf("first step pin = %+v", pins[0])
	}
	if pins[1].Ref != "registry.example.com/tool-b:2" || pins[1].Digest != fakeDigest2 {
		t.Errorf("second step pin = %+v", pins[1])
	}
	if len(capSet) != 0 {
		t.Errorf("rung-3 must pin no capability set, got %v", capSet)
	}
	if strings.Join(gotRefs, ",") != "registry.example.com/tool-a:1,registry.example.com/tool-b:2" {
		t.Errorf("resolver saw refs %v in the wrong order or set", gotRefs)
	}
}

// TestResolveImages_Rung3RejectsTagNotDigest proves the pin-by-digest guard
// covers rung-3: a resolver that yields a tag rather than a digest is a hard
// error, so no tag-shaped ref ever survives into a rung-3 pin.
func TestResolveImages_Rung3RejectsTagNotDigest(t *testing.T) {
	m := parseM(t, []byte(rung3MD))
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "registry.example.com/per-step-tool:1", nil // a tag, not a digest
	})
	_, _, err := resolveImages(context.Background(), m, dr, nil)
	if err == nil {
		t.Fatal("a rung-3 resolver returning a tag must be rejected")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error should explain the pin-by-digest rule, got: %v", err)
	}
}

// TestResolveImages_Rung3ResolverError proves a per-step resolver failure
// surfaces as a Run-level error rather than a silent skip.
func TestResolveImages_Rung3ResolverError(t *testing.T) {
	m := parseM(t, []byte(rung3MD))
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("image not found")
	})
	if _, _, err := resolveImages(context.Background(), m, dr, nil); err == nil {
		t.Error("an unresolvable rung-3 image must error")
	}
}

// TestResolveImages_Rung3NeedsResolver proves rung-3 requires a digest
// resolver: a rung-3 manifest with no resolver errors rather than silently
// pinning nothing (mirrors the rung-1 nil-resolver contract).
func TestResolveImages_Rung3NeedsResolver(t *testing.T) {
	m := parseM(t, []byte(rung3MD))
	if _, _, err := resolveImages(context.Background(), m, nil, nil); err == nil {
		t.Error("a rung-3 manifest with no resolver must error")
	}
}

// TestResolveImages_DistinctFeatureSetsDistinctPins is the #1510 acceptance
// proof for operator-specific composition divergence: two different
// capability-unit Feature sets must produce two different pinned image
// digests from the same base. The composer is a fake mapping each distinct
// feature set to a distinct digest, proving the contract (different inputs →
// different pins) at the freeze layer without a container runtime.
func TestResolveImages_DistinctFeatureSetsDistinctPins(t *testing.T) {
	// A composer that returns a digest deterministically keyed to the feature
	// set: identical inputs yield identical pins, distinct inputs yield
	// distinct pins.
	digestFor := map[string]string{
		"a": fakeDigest,
		"b": fakeDigest2,
	}
	fc := FeatureComposerFunc(func(_ context.Context, features []string) (string, error) {
		return digestFor[joinFeatures(features)], nil
	})

	mWithFeatures := func(features ...string) *manifest.Manifest {
		list := make([]any, len(features))
		for i, f := range features {
			list[i] = f
		}
		return &manifest.Manifest{
			Name: "x",
			Aileron: manifest.AileronBlock{
				Requires: manifest.Requires{ExecutionEnvironment: map[string]any{
					"rung2CapabilityUnits": map[string]any{"features": list},
				}},
			},
		}
	}

	pinsA, _, err := resolveImages(context.Background(), mWithFeatures("a"), nil, fc)
	if err != nil {
		t.Fatalf("compose feature set A: %v", err)
	}
	pinsB, _, err := resolveImages(context.Background(), mWithFeatures("b"), nil, fc)
	if err != nil {
		t.Fatalf("compose feature set B: %v", err)
	}
	if len(pinsA) != 1 || len(pinsB) != 1 {
		t.Fatalf("each composition must pin exactly one image, got A=%+v B=%+v", pinsA, pinsB)
	}
	if pinsA[0].Digest == pinsB[0].Digest {
		t.Errorf("distinct Feature sets must produce distinct pinned digests, both pinned %q", pinsA[0].Digest)
	}
	// Same feature set must reproduce the same pin (determinism).
	pinsA2, _, err := resolveImages(context.Background(), mWithFeatures("a"), nil, fc)
	if err != nil {
		t.Fatalf("recompose feature set A: %v", err)
	}
	if pinsA2[0].Digest != pinsA[0].Digest {
		t.Errorf("the same Feature set must reproduce the same pin: %q vs %q", pinsA2[0].Digest, pinsA[0].Digest)
	}
}

// TestFreeze_Rung1WorkedExample is the #1510 rung-1 worked-example acceptance:
// the committed rung-1 example skill freezes to a single image digest pin
// (the named image resolved by the fake resolver) and an empty capability
// set, with nothing deferred. It doubles as an executable check that the
// worked example stays valid.
func TestFreeze_Rung1WorkedExample(t *testing.T) {
	m := parseM(t, exampleRung1SkillMD(t))
	var gotRef string
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		gotRef = ref
		return fakeDigest, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, dr, nil)
	if err != nil {
		t.Fatalf("freeze rung-1 worked example: %v", err)
	}
	if gotRef != "registry.example.com/log-rollup-runner:2.3" {
		t.Errorf("resolver got ref %q, want the example's rung1Image.ref", gotRef)
	}
	if len(pins) != 1 || pins[0].Digest != fakeDigest {
		t.Errorf("rung-1 example must pin exactly the named image's digest, got %+v", pins)
	}
	if len(capSet) != 0 {
		t.Errorf("rung-1 has no capability set, got %v", capSet)
	}
}
