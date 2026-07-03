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

// mWithEnvironment builds a manifest directly with the given typed
// environment, for interim-branch cases that have no parseable fixture (the
// schema rejects them before resolveImages could see them).
func mWithEnvironment(env *manifest.Environment) *manifest.Manifest {
	return &manifest.Manifest{
		Name:    "x",
		Aileron: manifest.AileronBlock{Environment: env},
	}
}

// TestResolveImages_EnvironmentImageResolvesToDigest proves the
// environment.image path: the named custom base resolves to exactly one
// digest pin with no capability set.
func TestResolveImages_EnvironmentImageResolvesToDigest(t *testing.T) {
	m := parseM(t, []byte(envImageMD))
	var gotRef string
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		gotRef = ref
		return fakeDigest, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, dr, nil, "")
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
		t.Errorf("an image-only environment must have no capability set, got %v", capSet)
	}
}

// TestResolveImages_EnvironmentToolsComposesAndPins proves the
// environment.tools path over the committed worked example: the declared
// tool refs pass through the composer seam (raw, per the #1827 interim
// bridge), the built image pins to one digest, and the resolved capability
// set records the declared tools.
func TestResolveImages_EnvironmentToolsComposesAndPins(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	var gotTools []string
	fc := FeatureComposerFunc(func(_ context.Context, tools []string) (string, error) {
		gotTools = tools
		return fakeDigest, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, nil, fc, "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	wantTools := []string{"aws-cli@2.x"}
	if strings.Join(gotTools, ",") != strings.Join(wantTools, ",") {
		t.Errorf("composer got tools %v, want %v", gotTools, wantTools)
	}
	if len(pins) != 1 || pins[0].Digest != fakeDigest {
		t.Errorf("pins = %+v", pins)
	}
	if strings.Join(capSet, ",") != strings.Join(wantTools, ",") {
		t.Errorf("resolved capability set = %v, want %v", capSet, wantTools)
	}
}

// TestResolveImages_EnvironmentImageWithToolsRefusedInterim documents the
// INTERIM (#1827) behavior for a block declaring both image and tools: the
// schema accepts the combination (the target contract composes the tools
// onto the custom base), but until #1827 lands that composition, freeze
// refuses rather than silently pinning a signed artifact that omits the
// declared tools. This test is expected to change with #1827.
func TestResolveImages_EnvironmentImageWithToolsRefusedInterim(t *testing.T) {
	m := mWithEnvironment(&manifest.Environment{
		Image: "registry.example.com/base:1",
		Tools: []string{"gh@2"},
	})
	resolverHit, composerHit := false, false
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		resolverHit = true
		return fakeDigest, nil
	})
	fc := FeatureComposerFunc(func(_ context.Context, _ []string) (string, error) {
		composerHit = true
		return fakeDigest2, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, dr, fc, "")
	if err == nil {
		t.Fatal("INTERIM: image+tools must be refused until #1827 composes tools onto a custom base")
	}
	if !strings.Contains(err.Error(), "#1827") {
		t.Errorf("error should name the successor issue, got: %v", err)
	}
	if resolverHit || composerHit {
		t.Errorf("a refused environment must touch no container seam (resolver=%v composer=%v)", resolverHit, composerHit)
	}
	if pins != nil || capSet != nil {
		t.Errorf("a refused environment must pin nothing, got pins=%v cap=%v", pins, capSet)
	}
}

// TestResolveImages_EnvironmentEmptyDirectConstruct proves the defensive
// backstop for a direct-construct empty environment (the schema rejects
// environment: {} before this code runs on any parsed manifest).
func TestResolveImages_EnvironmentEmptyDirectConstruct(t *testing.T) {
	m := mWithEnvironment(&manifest.Environment{})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), fakeComposer(fakeDigest), ""); err == nil {
		t.Error("an environment with neither tools nor image must error")
	}
}

func TestResolveImages_InstructionOnlyEmpty(t *testing.T) {
	m := parseM(t, []byte(instructionOnlyMD))
	pins, capSet, err := resolveImages(context.Background(), m, nil, nil, "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if len(pins) != 0 || len(capSet) != 0 {
		t.Errorf("instruction-only must resolve to empty, got pins=%v cap=%v", pins, capSet)
	}
}

func TestResolveImages_NoEnvironmentEmpty(t *testing.T) {
	m := parseM(t, []byte(noEnvironmentMD))
	pins, _, err := resolveImages(context.Background(), m, nil, nil, "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if len(pins) != 0 {
		t.Errorf("no-environment skill must resolve to empty, got %v", pins)
	}
}

func TestResolveImages_RejectsTagNotDigest(t *testing.T) {
	m := parseM(t, []byte(envImageMD))
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "registry.example.com/runner:1.4", nil // a tag, not a digest
	})
	_, _, err := resolveImages(context.Background(), m, dr, nil, "")
	if err == nil {
		t.Fatal("a resolver returning a tag must be rejected")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error should explain the pin-by-digest rule, got: %v", err)
	}
}

// TestResolveImages_ToolsComposerTagRejected proves the pin-by-digest guard
// covers the composed-tools path: a composer that yields a tag rather than a
// digest is rejected.
func TestResolveImages_ToolsComposerTagRejected(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	fc := FeatureComposerFunc(func(_ context.Context, _ []string) (string, error) {
		return "aileron-composed:latest", nil // a tag, not a digest
	})
	_, _, err := resolveImages(context.Background(), m, nil, fc, "")
	if err == nil {
		t.Fatal("a composer returning a tag must be rejected")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error should explain the pin-by-digest rule, got: %v", err)
	}
}

func TestResolveImages_ResolverError(t *testing.T) {
	m := parseM(t, []byte(envImageMD))
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("image not found")
	})
	if _, _, err := resolveImages(context.Background(), m, dr, nil, ""); err == nil {
		t.Error("an unresolvable image must error")
	}
}

// TestResolveImages_ToolsComposerError proves a composer failure surfaces as
// an error rather than a silent empty pin set.
func TestResolveImages_ToolsComposerError(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	fc := FeatureComposerFunc(func(_ context.Context, _ []string) (string, error) {
		return "", errors.New("compose failed")
	})
	if _, _, err := resolveImages(context.Background(), m, nil, fc, ""); err == nil {
		t.Error("a failing composer must error")
	}
}

func TestResolveImages_EnvironmentImageNeedsResolver(t *testing.T) {
	m := parseM(t, []byte(envImageMD))
	if _, _, err := resolveImages(context.Background(), m, nil, nil, ""); err == nil {
		t.Error("an image environment with no resolver must error")
	}
}

func TestResolveImages_EnvironmentToolsNeedComposer(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	if _, _, err := resolveImages(context.Background(), m, nil, nil, ""); err == nil {
		t.Error("a tools environment with no composer must error")
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

// TestResolveImages_DistinctToolSetsDistinctPins is the composition
// divergence contract on the live environment path: two different declared
// tool sets must produce two different pinned image digests, and the same
// set must reproduce the same pin (determinism). The composer is a fake
// mapping each distinct tool set to a distinct digest, proving the contract
// at the freeze layer without a container runtime.
func TestResolveImages_DistinctToolSetsDistinctPins(t *testing.T) {
	digestFor := map[string]string{
		"aws-cli@2.x": fakeDigest,
		"gh@2":        fakeDigest2,
	}
	fc := FeatureComposerFunc(func(_ context.Context, tools []string) (string, error) {
		return digestFor[joinFeatures(tools)], nil
	})

	mWithTools := func(tools ...string) *manifest.Manifest {
		return mWithEnvironment(&manifest.Environment{Tools: tools})
	}

	pinsA, capA, err := resolveImages(context.Background(), mWithTools("aws-cli@2.x"), nil, fc, "")
	if err != nil {
		t.Fatalf("compose tool set A: %v", err)
	}
	pinsB, _, err := resolveImages(context.Background(), mWithTools("gh@2"), nil, fc, "")
	if err != nil {
		t.Fatalf("compose tool set B: %v", err)
	}
	if len(pinsA) != 1 || len(pinsB) != 1 {
		t.Fatalf("each composition must pin exactly one image, got A=%+v B=%+v", pinsA, pinsB)
	}
	if pinsA[0].Digest == pinsB[0].Digest {
		t.Errorf("distinct tool sets must produce distinct pinned digests, both pinned %q", pinsA[0].Digest)
	}
	if strings.Join(capA, ",") != "aws-cli@2.x" {
		t.Errorf("resolved capability set must record the declared tools, got %v", capA)
	}
	// Same tool set must reproduce the same pin (determinism).
	pinsA2, _, err := resolveImages(context.Background(), mWithTools("aws-cli@2.x"), nil, fc, "")
	if err != nil {
		t.Fatalf("recompose tool set A: %v", err)
	}
	if pinsA2[0].Digest != pinsA[0].Digest {
		t.Errorf("the same tool set must reproduce the same pin: %q vs %q", pinsA2[0].Digest, pinsA[0].Digest)
	}
}

// ---------------------------------------------------------------------------
// DORMANT legacy rung coverage (#1827/#1829). The schema no longer admits
// requires.executionEnvironment, so the fixtures below are built directly
// (never parsed). They keep the retained rung branches in resolveImages
// covered until their owners rewire or delete them.
// ---------------------------------------------------------------------------

// legacyRung1Default is a direct-construct rung-1 manifest that declares no
// ref (the retained default-runner path, #1808).
func legacyRung1Default() *manifest.Manifest {
	return mWithExecEnv(map[string]any{"rung1Image": map[string]any{}})
}

// TestResolveImages_LegacyRung1DefaultResolvesBaseImage keeps the dormant
// default-runner branch covered: a rung-1 declaration with no ref resolves
// the Aileron-provided runner image for the CLI version (a dev version
// selects the :edge tag).
func TestResolveImages_LegacyRung1DefaultResolvesBaseImage(t *testing.T) {
	var gotRef string
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		gotRef = ref
		return fakeDigest, nil
	})
	pins, capSet, err := resolveImages(context.Background(), legacyRung1Default(), dr, nil, "dev")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	wantRef := "ghcr.io/alrubinger/aileron-sandbox-base:edge"
	if gotRef != wantRef {
		t.Errorf("resolver got ref %q, want the default runner image %q", gotRef, wantRef)
	}
	if len(pins) != 1 || pins[0].Ref != wantRef || pins[0].Digest != fakeDigest {
		t.Errorf("default rung-1 must pin the concrete default ref + digest, got %+v", pins)
	}
	if capSet != nil {
		t.Errorf("rung-1 must have no capability set, got %v", capSet)
	}
}

// TestResolveImages_LegacyRung1DefaultReleaseUsesLatest keeps the CLI-version
// tag split covered on the dormant branch: a release version resolves the
// default runner image at :latest, matching composition.imageTag.
func TestResolveImages_LegacyRung1DefaultReleaseUsesLatest(t *testing.T) {
	var gotRef string
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		gotRef = ref
		return fakeDigest, nil
	})
	if _, _, err := resolveImages(context.Background(), legacyRung1Default(), dr, nil, "0.0.3"); err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if want := "ghcr.io/alrubinger/aileron-sandbox-base:latest"; gotRef != want {
		t.Errorf("a release CLI version must resolve the default at %q, got %q", want, gotRef)
	}
}

// TestResolveImages_LegacyRung1NamedRef keeps the dormant named-ref rung-1
// branch covered: the declared ref resolves to a digest pin.
func TestResolveImages_LegacyRung1NamedRef(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung1Image": map[string]any{"ref": "registry.example.com/runner:1.4"}})
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		return fakeDigest, nil
	})
	pins, _, err := resolveImages(context.Background(), m, dr, nil, "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if len(pins) != 1 || pins[0].Ref != "registry.example.com/runner:1.4" || pins[0].Digest != fakeDigest {
		t.Errorf("pins = %+v", pins)
	}
	if pins[0].StepID != "" {
		t.Errorf("a rung-1 pin must carry no StepID, got %+v", pins[0])
	}
}

// TestResolveImages_LegacyRung1NeedsResolver keeps the dormant nil-resolver
// guard covered.
func TestResolveImages_LegacyRung1NeedsResolver(t *testing.T) {
	if _, _, err := resolveImages(context.Background(), legacyRung1Default(), nil, nil, "dev"); err == nil {
		t.Error("a rung-1 manifest with no resolver must error")
	}
}

// TestResolveImages_LegacyRung1RejectsTagNotDigest keeps the pin-by-digest
// guard covered on the dormant rung-1 branch.
func TestResolveImages_LegacyRung1RejectsTagNotDigest(t *testing.T) {
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		return ref, nil // echo the tag, not a digest
	})
	if _, _, err := resolveImages(context.Background(), legacyRung1Default(), dr, nil, "dev"); err == nil {
		t.Error("a rung-1 resolver returning a tag must be rejected")
	}
}

// TestResolveImages_LegacyRung1ResolverError keeps the resolver failure path
// covered on the dormant rung-1 branch.
func TestResolveImages_LegacyRung1ResolverError(t *testing.T) {
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("image not found")
	})
	if _, _, err := resolveImages(context.Background(), legacyRung1Default(), dr, nil, "dev"); err == nil {
		t.Error("an unresolvable rung-1 image must error")
	}
}

// TestResolveImages_LegacyRung2ComposesAndPins keeps the dormant rung-2
// branch covered: the declared Features compose to one pin plus the resolved
// capability set.
func TestResolveImages_LegacyRung2ComposesAndPins(t *testing.T) {
	features := []any{"ghcr.io/example/feature-a:1", "ghcr.io/example/feature-b:1"}
	m := mWithExecEnv(map[string]any{"rung2CapabilityUnits": map[string]any{"features": features}})
	var gotFeatures []string
	fc := FeatureComposerFunc(func(_ context.Context, fs []string) (string, error) {
		gotFeatures = fs
		return fakeDigest, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, nil, fc, "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if strings.Join(gotFeatures, ",") != "ghcr.io/example/feature-a:1,ghcr.io/example/feature-b:1" {
		t.Errorf("composer got features %v", gotFeatures)
	}
	if len(pins) != 1 || pins[0].Digest != fakeDigest {
		t.Errorf("pins = %+v", pins)
	}
	if strings.Join(capSet, ",") != "ghcr.io/example/feature-a:1,ghcr.io/example/feature-b:1" {
		t.Errorf("resolved capability set = %v", capSet)
	}
}

// TestResolveImages_LegacyRung2NeedsComposer keeps the dormant nil-composer
// guard covered.
func TestResolveImages_LegacyRung2NeedsComposer(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung2CapabilityUnits": map[string]any{"features": []any{"f"}}})
	if _, _, err := resolveImages(context.Background(), m, nil, nil, ""); err == nil {
		t.Error("a rung-2 manifest with no composer must error")
	}
}

// TestResolveImages_LegacyRung2ComposerError keeps the composer failure path
// covered on the dormant rung-2 branch.
func TestResolveImages_LegacyRung2ComposerError(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung2CapabilityUnits": map[string]any{"features": []any{"f"}}})
	fc := FeatureComposerFunc(func(_ context.Context, _ []string) (string, error) {
		return "", errors.New("compose failed")
	})
	if _, _, err := resolveImages(context.Background(), m, nil, fc, ""); err == nil {
		t.Error("a failing rung-2 composer must error")
	}
}

// TestResolveImages_LegacyRung2RejectsTagNotDigest keeps the pin-by-digest
// guard covered on the dormant rung-2 branch.
func TestResolveImages_LegacyRung2RejectsTagNotDigest(t *testing.T) {
	m := mWithExecEnv(map[string]any{"rung2CapabilityUnits": map[string]any{"features": []any{"f"}}})
	fc := FeatureComposerFunc(func(_ context.Context, _ []string) (string, error) {
		return "composed:latest", nil // a tag, not a digest
	})
	if _, _, err := resolveImages(context.Background(), m, nil, fc, ""); err == nil {
		t.Error("a rung-2 composer returning a tag must be rejected")
	}
}

// legacyRung3 builds a direct-construct rung-3 manifest from the given step
// maps.
func legacyRung3(steps ...map[string]any) *manifest.Manifest {
	list := make([]any, len(steps))
	for i, s := range steps {
		list[i] = s
	}
	return mWithExecEnv(map[string]any{"rung3PerStepImages": map[string]any{"steps": list}})
}

// TestResolveImages_LegacyRung3ResolvesPerStepDigests keeps the dormant
// rung-3 branch covered: each step's sibling image resolves to its own pin
// carrying the declared step id, in declared order, with no capability set.
func TestResolveImages_LegacyRung3ResolvesPerStepDigests(t *testing.T) {
	m := legacyRung3(
		map[string]any{"id": "extract", "image": "registry.example.com/tool-a:1"},
		map[string]any{"id": "convert", "image": "registry.example.com/tool-b:2"},
	)
	digestFor := map[string]string{
		"registry.example.com/tool-a:1": fakeDigest,
		"registry.example.com/tool-b:2": fakeDigest2,
	}
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		return digestFor[ref], nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, dr, nil, "")
	if err != nil {
		t.Fatalf("rung-3 resolution must succeed: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("rung-3 must pin one image per step, got %+v", pins)
	}
	if pins[0].Ref != "registry.example.com/tool-a:1" || pins[0].Digest != fakeDigest || pins[0].StepID != "extract" {
		t.Errorf("first step pin = %+v", pins[0])
	}
	if pins[1].Ref != "registry.example.com/tool-b:2" || pins[1].Digest != fakeDigest2 || pins[1].StepID != "convert" {
		t.Errorf("second step pin = %+v", pins[1])
	}
	if len(capSet) != 0 {
		t.Errorf("rung-3 must pin no capability set, got %v", capSet)
	}
}

// TestResolveImages_LegacyRung3SharedTagPinsDistinctlyByID keeps the #1739
// tag-collision regression covered on the dormant branch: two steps naming
// the SAME image tag pin distinctly by their declared step ids.
func TestResolveImages_LegacyRung3SharedTagPinsDistinctlyByID(t *testing.T) {
	m := legacyRung3(
		map[string]any{"id": "first", "image": "registry.example.com/shared-tool:1"},
		map[string]any{"id": "second", "image": "registry.example.com/shared-tool:1"},
	)
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return fakeDigest, nil
	})
	pins, _, err := resolveImages(context.Background(), m, dr, nil, "")
	if err != nil {
		t.Fatalf("shared-tag rung-3 resolution must succeed: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("two steps sharing a tag must still pin one image per step, got %+v", pins)
	}
	if pins[0].StepID != "first" || pins[1].StepID != "second" {
		t.Errorf("pins must carry the declared step ids, got [%q, %q]", pins[0].StepID, pins[1].StepID)
	}
}

// TestResolveImages_LegacyRung3PositionalIDFallback keeps the positional
// fallback covered on the dormant branch: steps with no declared id still
// get traceable, distinct positional ids stamped onto their pins.
func TestResolveImages_LegacyRung3PositionalIDFallback(t *testing.T) {
	m := legacyRung3(
		map[string]any{"image": "registry.example.com/tool-a:1"},
		map[string]any{"image": "registry.example.com/tool-b:2"},
	)
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return fakeDigest, nil
	})
	pins, _, err := resolveImages(context.Background(), m, dr, nil, "")
	if err != nil {
		t.Fatalf("no-id rung-3 resolution must succeed: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("expected two pins, got %+v", pins)
	}
	if pins[0].StepID != "#0" || pins[1].StepID != "#1" {
		t.Errorf("positional fallback ids must be #0/#1, got [%q, %q]", pins[0].StepID, pins[1].StepID)
	}
}

// TestResolveImages_LegacyRung3SealsTrustContractHosts keeps the #1775
// sealed-reach behavior covered on the dormant branch: a step declaring a
// trustContract has its hosts sealed onto the pin; a step declaring none
// pins with nil hosts, which the marshaled lock omits.
func TestResolveImages_LegacyRung3SealsTrustContractHosts(t *testing.T) {
	m := legacyRung3(
		map[string]any{
			"id":    "reach",
			"image": "registry.example.com/tool-a:1",
			"trustContract": map[string]any{
				"hosts": []any{"api.upstream.example.com", "api.upstream.example.com:443"},
			},
		},
		map[string]any{"id": "noreach", "image": "registry.example.com/tool-b:2"},
	)
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return fakeDigest, nil
	})
	pins, _, err := resolveImages(context.Background(), m, dr, nil, "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("expected two pins, got %+v", pins)
	}
	if pins[0].StepID != "reach" {
		t.Fatalf("first pin must be the reach step, got %+v", pins[0])
	}
	if strings.Join(pins[0].Hosts, ",") != "api.upstream.example.com,api.upstream.example.com:443" {
		t.Errorf("reach pin must seal the declared hosts, got %v", pins[0].Hosts)
	}
	if pins[1].StepID != "noreach" || len(pins[1].Hosts) != 0 {
		t.Errorf("a step with no trust contract must pin with nil hosts, got %+v", pins[1])
	}

	// The nil-hosts pin omits `hosts` from the marshaled lock (byte-stability).
	lf, err := MarshalLockfile(Lockfile{ResolvedImages: pins[1:]})
	if err != nil {
		t.Fatalf("marshal lockfile: %v", err)
	}
	if strings.Contains(string(lf), "hosts") {
		t.Errorf("a no-reach pin must omit hosts from the lock, got:\n%s", lf)
	}
}

// TestResolveImages_LegacyRung3RejectsTagNotDigest keeps the pin-by-digest
// guard covered on the dormant rung-3 branch.
func TestResolveImages_LegacyRung3RejectsTagNotDigest(t *testing.T) {
	m := legacyRung3(map[string]any{"image": "registry.example.com/per-step-tool:1"})
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "registry.example.com/per-step-tool:1", nil // a tag, not a digest
	})
	_, _, err := resolveImages(context.Background(), m, dr, nil, "")
	if err == nil {
		t.Fatal("a rung-3 resolver returning a tag must be rejected")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error should explain the pin-by-digest rule, got: %v", err)
	}
}

// TestResolveImages_LegacyRung3ResolverError keeps the per-step resolver
// failure path covered on the dormant branch.
func TestResolveImages_LegacyRung3ResolverError(t *testing.T) {
	m := legacyRung3(map[string]any{"image": "registry.example.com/per-step-tool:1"})
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("image not found")
	})
	if _, _, err := resolveImages(context.Background(), m, dr, nil, ""); err == nil {
		t.Error("an unresolvable rung-3 image must error")
	}
}

// TestResolveImages_LegacyRung3NeedsResolver keeps the dormant rung-3
// nil-resolver guard covered.
func TestResolveImages_LegacyRung3NeedsResolver(t *testing.T) {
	m := legacyRung3(map[string]any{"image": "registry.example.com/per-step-tool:1"})
	if _, _, err := resolveImages(context.Background(), m, nil, nil, ""); err == nil {
		t.Error("a rung-3 manifest with no resolver must error")
	}
}
