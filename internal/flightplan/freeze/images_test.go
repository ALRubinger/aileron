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
