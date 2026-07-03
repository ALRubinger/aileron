package freeze

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"github.com/ALRubinger/aileron/internal/sandbox/composition"
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
// environment, for defensive-backstop cases that have no parseable fixture
// (the schema rejects them before resolveImages could see them) and for
// direct-construct composition cases.
func mWithEnvironment(env *manifest.Environment) *manifest.Manifest {
	return &manifest.Manifest{
		Name:    "x",
		Aileron: manifest.AileronBlock{Environment: env},
	}
}

// resolverReturning returns a DigestResolver that records the ref it was
// asked for into *gotRef and returns digest.
func resolverReturning(digest string, gotRef *string) DigestResolver {
	return DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		if gotRef != nil {
			*gotRef = ref
		}
		return digest, nil
	})
}

// TestResolveImages_EnvironmentImageResolvesToDigest proves the image-only
// path: the named custom base resolves to exactly one digest pin with no
// capability set and the composer seam is never touched.
func TestResolveImages_EnvironmentImageResolvesToDigest(t *testing.T) {
	m := parseM(t, []byte(envImageMD))
	var gotRef string
	composerHit := false
	fc := FeatureComposerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
		composerHit = true
		return fakeDigest2, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, resolverReturning(fakeDigest, &gotRef), fc, "")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if gotRef != "registry.example.com/runner:1.4" {
		t.Errorf("resolver got ref %q", gotRef)
	}
	if len(pins) != 1 || pins[0].Digest != fakeDigest || pins[0].Ref != "registry.example.com/runner:1.4" {
		t.Errorf("pins = %+v", pins)
	}
	// An image-only pin has no composed local tag: it boots ref@digest.
	if pins[0].LocalTag != "" {
		t.Errorf("an image-only pin must carry no LocalTag, got %q", pins[0].LocalTag)
	}
	if capSet != nil {
		t.Errorf("an image-only environment must have no capability set, got %v", capSet)
	}
	if composerHit {
		t.Error("an image-only environment must never touch the composer seam")
	}
}

// TestResolveImages_ToolsComposeOntoDefaultBase proves the tools path over
// the committed worked example: the runner base for the CLI version is
// digest-resolved, the composer receives the DIGEST-PINNED base plus the
// CATALOG-RESOLVED Feature references (never raw tool refs), the built image
// pins to exactly one digest, and the resolved capability set records the
// declared tools verbatim.
func TestResolveImages_ToolsComposeOntoDefaultBase(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	var gotBaseRef string
	var gotComposeBase string
	var gotFeatures []string
	fc := FeatureComposerFunc(func(_ context.Context, base string, features []string) (string, error) {
		gotComposeBase = base
		gotFeatures = features
		return fakeDigest2, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, resolverReturning(fakeDigest, &gotBaseRef), fc, "dev")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	wantBase := "ghcr.io/alrubinger/aileron-sandbox-base:edge"
	if gotBaseRef != wantBase {
		t.Errorf("resolver got base ref %q, want the default runner base %q", gotBaseRef, wantBase)
	}
	if gotComposeBase != wantBase+"@"+fakeDigest {
		t.Errorf("composer got base %q, want the digest-pinned base %q", gotComposeBase, wantBase+"@"+fakeDigest)
	}
	wantFeatures := "ghcr.io/alrubinger/aileron-features/aws-cli:0"
	if strings.Join(gotFeatures, ",") != wantFeatures {
		t.Errorf("composer got features %v, want the catalog-resolved %q", gotFeatures, wantFeatures)
	}
	if len(pins) != 1 || pins[0].Digest != fakeDigest2 {
		t.Fatalf("a tools environment must pin exactly one image with the composed digest, got %+v", pins)
	}
	if !strings.Contains(pins[0].Ref, wantBase+"@"+fakeDigest) || !strings.Contains(pins[0].Ref, "aws-cli@2.x") {
		t.Errorf("the pin ref must record the pinned base and the composed tools, got %q", pins[0].Ref)
	}
	// The composed pin carries the bootable local-daemon tag the composed image
	// carries in the daemon: byte-identical to LocalToolsImageTag over the same
	// pinnedBase and catalog-resolved feature refs the composer received.
	wantTag := composition.LocalToolsImageTag(wantBase+"@"+fakeDigest, gotFeatures)
	if !strings.HasPrefix(wantTag, "aileron/sandbox-tools:") {
		t.Fatalf("expected an aileron/sandbox-tools: tag, got %q", wantTag)
	}
	if pins[0].LocalTag != wantTag {
		t.Errorf("composed pin LocalTag = %q, want %q", pins[0].LocalTag, wantTag)
	}
	if strings.Join(capSet, ",") != "aws-cli@2.x" {
		t.Errorf("resolved capability set must record the declared tools verbatim, got %v", capSet)
	}
}

// TestResolveImages_ToolsDefaultBaseReleaseUsesLatest proves the CLI-version
// tag split on the default composition base: a release version resolves the
// runner base at :latest, matching composition.BaseImage.
func TestResolveImages_ToolsDefaultBaseReleaseUsesLatest(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	var gotBaseRef string
	if _, _, err := resolveImages(context.Background(), m, resolverReturning(fakeDigest, &gotBaseRef), fakeComposer(fakeDigest2), "0.0.3"); err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if want := "ghcr.io/alrubinger/aileron-sandbox-base:latest"; gotBaseRef != want {
		t.Errorf("a release CLI version must compose onto %q, got %q", want, gotBaseRef)
	}
}

// TestResolveImages_ToolsComposeOntoCustomImage proves the both-declared
// contract (#1827): a block declaring image AND tools composes the declared
// tools onto the digest-resolved custom base, never silently dropping either.
func TestResolveImages_ToolsComposeOntoCustomImage(t *testing.T) {
	m := mWithEnvironment(&manifest.Environment{
		Image: "registry.example.com/base:1",
		Tools: []string{"gh@2", "aws-cli@2.x"},
	})
	var gotBaseRef string
	var gotComposeBase string
	var gotFeatures []string
	fc := FeatureComposerFunc(func(_ context.Context, base string, features []string) (string, error) {
		gotComposeBase = base
		gotFeatures = features
		return fakeDigest2, nil
	})
	pins, capSet, err := resolveImages(context.Background(), m, resolverReturning(fakeDigest, &gotBaseRef), fc, "dev")
	if err != nil {
		t.Fatalf("resolveImages: %v", err)
	}
	if gotBaseRef != "registry.example.com/base:1" {
		t.Errorf("resolver got %q, want the declared custom base", gotBaseRef)
	}
	if gotComposeBase != "registry.example.com/base:1@"+fakeDigest {
		t.Errorf("composer got base %q, want the digest-pinned custom base", gotComposeBase)
	}
	want := "ghcr.io/alrubinger/aileron-features/gh:0,ghcr.io/alrubinger/aileron-features/aws-cli:0"
	if strings.Join(gotFeatures, ",") != want {
		t.Errorf("composer got features %v, want %q (declared order, catalog-resolved)", gotFeatures, want)
	}
	if len(pins) != 1 || pins[0].Digest != fakeDigest2 {
		t.Fatalf("tools onto a custom base must pin exactly one image, got %+v", pins)
	}
	wantTag := composition.LocalToolsImageTag("registry.example.com/base:1@"+fakeDigest, gotFeatures)
	if !strings.HasPrefix(wantTag, "aileron/sandbox-tools:") {
		t.Fatalf("expected an aileron/sandbox-tools: tag, got %q", wantTag)
	}
	if pins[0].LocalTag != wantTag {
		t.Errorf("composed pin LocalTag = %q, want %q", pins[0].LocalTag, wantTag)
	}
	if strings.Join(capSet, ",") != "gh@2,aws-cli@2.x" {
		t.Errorf("resolved capability set = %v, want the declared tools verbatim", capSet)
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

// TestResolveImages_ToolsBaseTagNotDigestRejected proves the pin-by-digest
// guard covers the composition base: a resolver that yields a tag for the
// base is rejected BEFORE the composer runs, so a drifting base can never be
// composed onto.
func TestResolveImages_ToolsBaseTagNotDigestRejected(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		return ref, nil // echo the tag, not a digest
	})
	composerHit := false
	fc := FeatureComposerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
		composerHit = true
		return fakeDigest2, nil
	})
	if _, _, err := resolveImages(context.Background(), m, dr, fc, "dev"); err == nil {
		t.Fatal("a tag-resolved base must be rejected")
	}
	if composerHit {
		t.Error("the composer must never run on an unpinned base")
	}
}

// TestResolveImages_ToolsComposerTagRejected proves the pin-by-digest guard
// covers the composed image: a composer that yields a tag rather than a
// digest is rejected.
func TestResolveImages_ToolsComposerTagRejected(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	fc := FeatureComposerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
		return "aileron-composed:latest", nil // a tag, not a digest
	})
	_, _, err := resolveImages(context.Background(), m, dummyResolver(), fc, "dev")
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

// TestResolveImages_ToolsBaseResolverError proves a base-resolution failure
// on the tools path surfaces as an error rather than a silent unpinned
// composition.
func TestResolveImages_ToolsBaseResolverError(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("base not found")
	})
	if _, _, err := resolveImages(context.Background(), m, dr, fakeComposer(fakeDigest2), "dev"); err == nil {
		t.Error("an unresolvable composition base must error")
	}
}

// TestResolveImages_ToolsComposerError proves a composer failure surfaces as
// an error rather than a silent empty pin set.
func TestResolveImages_ToolsComposerError(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	fc := FeatureComposerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
		return "", errors.New("compose failed")
	})
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), fc, "dev"); err == nil {
		t.Error("a failing composer must error")
	}
}

func TestResolveImages_EnvironmentImageNeedsResolver(t *testing.T) {
	m := parseM(t, []byte(envImageMD))
	if _, _, err := resolveImages(context.Background(), m, nil, nil, ""); err == nil {
		t.Error("an image environment with no resolver must error")
	}
}

// TestResolveImages_EnvironmentToolsNeedResolver proves the tools path
// requires the digest resolver: the composition base must be digest-pinned
// before composing, so a nil resolver is a hard error.
func TestResolveImages_EnvironmentToolsNeedResolver(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	if _, _, err := resolveImages(context.Background(), m, nil, fakeComposer(fakeDigest2), "dev"); err == nil {
		t.Error("a tools environment with no resolver must error")
	}
}

func TestResolveImages_EnvironmentToolsNeedComposer(t *testing.T) {
	m := parseM(t, exampleSkillMD(t))
	if _, _, err := resolveImages(context.Background(), m, dummyResolver(), nil, "dev"); err == nil {
		t.Error("a tools environment with no composer must error")
	}
}

// TestResolveImages_UnknownToolRejected proves the freeze-time defensive
// backstop for a direct-construct unknown tool (the schema's closed tool
// pattern rejects it before this code runs on any parsed manifest): the
// catalog resolution fails before any seam is touched.
func TestResolveImages_UnknownToolRejected(t *testing.T) {
	m := mWithEnvironment(&manifest.Environment{Tools: []string{"nmap@7.1"}})
	resolverHit, composerHit := false, false
	dr := DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
		resolverHit = true
		return fakeDigest, nil
	})
	fc := FeatureComposerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
		composerHit = true
		return fakeDigest2, nil
	})
	if _, _, err := resolveImages(context.Background(), m, dr, fc, "dev"); err == nil {
		t.Fatal("an unknown tool name must be rejected")
	}
	if resolverHit || composerHit {
		t.Errorf("a rejected tool must touch no container seam (resolver=%v composer=%v)", resolverHit, composerHit)
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
// divergence contract: two different declared tool sets must produce two
// different pinned image digests, and the same set must reproduce the same
// pin (determinism). The composer is a fake mapping each distinct feature set
// to a distinct digest, proving the contract at the freeze layer without a
// container runtime.
func TestResolveImages_DistinctToolSetsDistinctPins(t *testing.T) {
	digestFor := map[string]string{
		"ghcr.io/alrubinger/aileron-features/aws-cli:0": fakeDigest,
		"ghcr.io/alrubinger/aileron-features/gh:0":      fakeDigest2,
	}
	fc := FeatureComposerFunc(func(_ context.Context, _ string, features []string) (string, error) {
		return digestFor[strings.Join(features, ",")], nil
	})

	mWithTools := func(tools ...string) *manifest.Manifest {
		return mWithEnvironment(&manifest.Environment{Tools: tools})
	}

	pinsA, capA, err := resolveImages(context.Background(), mWithTools("aws-cli@2.x"), dummyResolver(), fc, "dev")
	if err != nil {
		t.Fatalf("compose tool set A: %v", err)
	}
	pinsB, _, err := resolveImages(context.Background(), mWithTools("gh@2"), dummyResolver(), fc, "dev")
	if err != nil {
		t.Fatalf("compose tool set B: %v", err)
	}
	if len(pinsA) != 1 || len(pinsB) != 1 {
		t.Fatalf("each composition must pin exactly one image, got A=%+v B=%+v", pinsA, pinsB)
	}
	if pinsA[0].Digest == pinsB[0].Digest {
		t.Errorf("distinct tool sets must produce distinct pinned digests, both pinned %q", pinsA[0].Digest)
	}
	// Tag collision-freedom carries through freeze: distinct tool sets get
	// distinct bootable local tags.
	if pinsA[0].LocalTag == "" || pinsB[0].LocalTag == "" {
		t.Fatalf("each composed pin must carry a LocalTag, got A=%q B=%q", pinsA[0].LocalTag, pinsB[0].LocalTag)
	}
	if pinsA[0].LocalTag == pinsB[0].LocalTag {
		t.Errorf("distinct tool sets must produce distinct LocalTags, both %q", pinsA[0].LocalTag)
	}
	if strings.Join(capA, ",") != "aws-cli@2.x" {
		t.Errorf("resolved capability set must record the declared tools, got %v", capA)
	}
	// Same tool set must reproduce the same pin (determinism).
	pinsA2, _, err := resolveImages(context.Background(), mWithTools("aws-cli@2.x"), dummyResolver(), fc, "dev")
	if err != nil {
		t.Fatalf("recompose tool set A: %v", err)
	}
	if pinsA2[0].Digest != pinsA[0].Digest {
		t.Errorf("the same tool set must reproduce the same pin: %q vs %q", pinsA2[0].Digest, pinsA[0].Digest)
	}
	if pinsA2[0].LocalTag != pinsA[0].LocalTag {
		t.Errorf("the same tool set must reproduce the same LocalTag: %q vs %q", pinsA2[0].LocalTag, pinsA[0].LocalTag)
	}
}
