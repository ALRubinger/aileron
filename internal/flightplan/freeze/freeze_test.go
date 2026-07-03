package freeze

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

func fakeComposer(digest string) FeatureComposer {
	return FeatureComposerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
		return digest, nil
	})
}

func TestRun_EnvironmentToolsHappyPath(t *testing.T) {
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), exampleSkillMD(t), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dummyResolver(),
		Composer:       fakeComposer(fakeDigest),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Name != "weekly-metrics-digest" {
		t.Errorf("name = %q", res.Name)
	}
	if !sha256Pattern.MatchString(res.ContentHash) {
		t.Errorf("contentHash = %q", res.ContentHash)
	}
	if res.Version != "1.0.0" {
		t.Errorf("version = %q", res.Version)
	}
	if len(res.Lock.ResolvedImages) != 1 || res.Lock.ResolvedImages[0].Digest != fakeDigest {
		t.Errorf("resolvedImages = %+v", res.Lock.ResolvedImages)
	}
	if len(res.Lock.ResolvedCapabilitySet) != 1 || res.Lock.ResolvedCapabilitySet[0] != "aws-cli@2.x" {
		t.Errorf("resolvedCapabilitySet = %v", res.Lock.ResolvedCapabilitySet)
	}

	// The signature verifies against the stored public key over the
	// canonical content bytes. Reconstruct those bytes the way Run does.
	lockNoHash := res.Lock.withoutContentHash()
	mNoHash, err := injectLock(exampleSkillMD(t), lockNoHash)
	if err != nil {
		t.Fatal(err)
	}
	lfNoHash, err := MarshalLockfile(lockNoHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(canonicalContent(mNoHash, lfNoHash), res.Signature, res.PublicKey); err != nil {
		t.Errorf("signature must verify: %v", err)
	}

	// The frozen manifest re-parses with the lock present, and the
	// contentHash inside it matches the result.
	fm, err := manifest.Parse(res.FrozenManifest)
	if err != nil {
		t.Fatalf("frozen manifest must re-parse: %v", err)
	}
	if fm.Aileron.Lock["contentHash"] != res.ContentHash {
		t.Errorf("frozen manifest lock.contentHash = %v, want %v", fm.Aileron.Lock["contentHash"], res.ContentHash)
	}
}

// TestRun_EnvironmentImagePinsAndSigns is the end-to-end proof for the
// environment.image escape hatch: a full freeze over a manifest naming a
// custom base pins the resolved digest, and the produced signature verifies
// over the bytes that include that pin.
func TestRun_EnvironmentImagePinsAndSigns(t *testing.T) {
	_, keyPath := genSigningKey(t)
	var gotRef string
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		gotRef = ref
		return fakeDigest, nil
	})
	res, err := Run(context.Background(), []byte(envImageMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantRef := "registry.example.com/runner:1.4"
	if gotRef != wantRef {
		t.Errorf("resolver got ref %q, want the environment image %q", gotRef, wantRef)
	}
	if len(res.Lock.ResolvedImages) != 1 {
		t.Fatalf("an environment-image freeze must pin one image, got %+v", res.Lock.ResolvedImages)
	}
	if res.Lock.ResolvedImages[0].Ref != wantRef || res.Lock.ResolvedImages[0].Digest != fakeDigest {
		t.Errorf("lock must record the declared ref + digest, got %+v", res.Lock.ResolvedImages[0])
	}
	if len(res.Lock.ResolvedCapabilitySet) != 0 {
		t.Errorf("an image-only environment pins no capability set, got %v", res.Lock.ResolvedCapabilitySet)
	}

	// The signature verifies over the canonical content bytes, so the pin is
	// covered by the plan content hash and signature.
	lockNoHash := res.Lock.withoutContentHash()
	mNoHash, err := injectLock([]byte(envImageMD), lockNoHash)
	if err != nil {
		t.Fatal(err)
	}
	lfNoHash, err := MarshalLockfile(lockNoHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(canonicalContent(mNoHash, lfNoHash), res.Signature, res.PublicKey); err != nil {
		t.Errorf("signature must verify over the environment pin: %v", err)
	}

	// The frozen manifest re-parses with the lock present, carrying the pin.
	fm, err := manifest.Parse(res.FrozenManifest)
	if err != nil {
		t.Fatalf("frozen environment-image manifest must re-parse: %v", err)
	}
	if fm.Aileron.Lock["contentHash"] != res.ContentHash {
		t.Errorf("frozen manifest lock.contentHash = %v, want %v", fm.Aileron.Lock["contentHash"], res.ContentHash)
	}
}

func TestRun_Reproducible(t *testing.T) {
	_, keyPath := genSigningKey(t)
	opts := Options{Version: "1.0.0", SigningKeyPath: keyPath, Resolver: dummyResolver(), Composer: fakeComposer(fakeDigest)}
	// The tool-steps fixture carries a MULTI-ENTRY stepTrust section, so this
	// also proves the step-keyed map marshals byte-deterministically (two
	// freezes of the same input produce byte-identical artifacts).
	for name, fixture := range map[string][]byte{
		"environment tools": exampleSkillMD(t),
		"tool steps":        []byte(toolStepsMD),
	} {
		t.Run(name, func(t *testing.T) {
			a, err := Run(context.Background(), fixture, opts)
			if err != nil {
				t.Fatalf("Run a: %v", err)
			}
			b, err := Run(context.Background(), fixture, opts)
			if err != nil {
				t.Fatalf("Run b: %v", err)
			}
			if a.ContentHash != b.ContentHash {
				t.Errorf("contentHash not reproducible: %q != %q", a.ContentHash, b.ContentHash)
			}
			if string(a.FrozenManifest) != string(b.FrozenManifest) {
				t.Error("frozen manifest bytes not reproducible")
			}
			if string(a.Lockfile) != string(b.Lockfile) {
				t.Error("lockfile bytes not reproducible")
			}
		})
	}
}

func TestRun_ContentHashChangesWithVersion(t *testing.T) {
	_, keyPath := genSigningKey(t)
	a, err := Run(context.Background(), exampleSkillMD(t), Options{Version: "1.0.0", SigningKeyPath: keyPath, Resolver: dummyResolver(), Composer: fakeComposer(fakeDigest)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(context.Background(), exampleSkillMD(t), Options{Version: "2.0.0", SigningKeyPath: keyPath, Resolver: dummyResolver(), Composer: fakeComposer(fakeDigest)})
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentHash == b.ContentHash {
		t.Error("a different version label must change the content hash")
	}
}

// TestRun_ToolStepsSealStepTrust is the end-to-end proof of the step-keyed
// trust seal: a freeze over a manifest with tool steps produces a lockfile
// whose stepTrust carries exactly the contracted steps' declared hosts under
// the schema field names, and the emitted YAML round-trips.
func TestRun_ToolStepsSealStepTrust(t *testing.T) {
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(toolStepsMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dummyResolver(),
		Composer:       fakeComposer(fakeDigest2),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Lock.StepTrust) != 2 {
		t.Fatalf("StepTrust = %+v, want the two contracted steps", res.Lock.StepTrust)
	}
	if strings.Join(res.Lock.StepTrust["fetch"].Hosts, ",") != "s3.amazonaws.com,s3.amazonaws.com:443" {
		t.Errorf("fetch reach = %v", res.Lock.StepTrust["fetch"].Hosts)
	}
	if strings.Join(res.Lock.StepTrust["file"].Hosts, ",") != "api.github.com" {
		t.Errorf("file reach = %v", res.Lock.StepTrust["file"].Hosts)
	}
	// The emitted lockfile uses the schema field names.
	lf := string(res.Lockfile)
	for _, key := range []string{"stepTrust:", "fetch:", "file:", "hosts:"} {
		if !strings.Contains(lf, key) {
			t.Errorf("lockfile missing %q:\n%s", key, lf)
		}
	}
	if strings.Contains(lf, "version:\n") {
		t.Errorf("the uncontracted step must not appear in stepTrust:\n%s", lf)
	}
	// The frozen manifest re-parses and its lock block carries the section.
	fm, err := manifest.Parse(res.FrozenManifest)
	if err != nil {
		t.Fatalf("frozen manifest must re-parse: %v", err)
	}
	if _, ok := fm.Aileron.Lock["stepTrust"]; !ok {
		t.Error("frozen manifest lock block must carry stepTrust")
	}
}

// TestRun_MalformedToolStepTrustRefuses proves a tool step declaring an
// empty reach refuses to freeze. Run parses raw bytes, so the schema rejects
// the shape at the validation gate (sealStepTrust's own backstop for direct
// constructs is covered in steptrust_test.go); either way the refusal lands
// before signing, which is the fail-closed contract.
func TestRun_MalformedToolStepTrustRefuses(t *testing.T) {
	_, keyPath := genSigningKey(t)
	bad := strings.Replace(toolStepsMD, "        hosts:\n          - s3.amazonaws.com\n          - s3.amazonaws.com:443\n", "        hosts: []\n", 1)
	if bad == toolStepsMD {
		t.Fatal("test setup: fixture hosts not found to malform")
	}
	if _, err := Run(context.Background(), []byte(bad), Options{
		SigningKeyPath: keyPath,
		Resolver:       dummyResolver(),
		Composer:       fakeComposer(fakeDigest2),
	}); err == nil {
		t.Error("a tool step with an empty declared reach must refuse to freeze")
	}
}

func TestRun_NoEnvironmentStillSigns(t *testing.T) {
	_, keyPath := genSigningKey(t)
	// A skill with an aileron block but no environment block has no images
	// to resolve. Run must still produce a contentHash, inject an
	// empty-images lock, and sign the result.
	res, err := Run(context.Background(), []byte(noEnvironmentMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Lock.ResolvedImages) != 0 {
		t.Errorf("no-environment skill must have empty resolvedImages, got %v", res.Lock.ResolvedImages)
	}
	if !sha256Pattern.MatchString(res.ContentHash) {
		t.Errorf("contentHash = %q", res.ContentHash)
	}
	if len(res.Signature) == 0 || len(res.PublicKey) == 0 {
		t.Error("no-environment skill must still be signed")
	}
}

func TestRun_TrueInstructionOnlySigns(t *testing.T) {
	// A skill with NO aileron block still freezes: there is no lock to
	// inject into the manifest, but it gets a contentHash + signature over
	// the (empty) lockfile + the LF-canonical manifest.
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(instructionOnlyMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("Run instruction-only: %v", err)
	}
	if !sha256Pattern.MatchString(res.ContentHash) {
		t.Errorf("contentHash = %q", res.ContentHash)
	}
	if len(res.Signature) == 0 || len(res.PublicKey) == 0 {
		t.Error("instruction-only skill must still be signed")
	}
	if len(res.Lock.ResolvedImages) != 0 {
		t.Errorf("instruction-only resolvedImages = %v", res.Lock.ResolvedImages)
	}
	// The frozen manifest has no lock block (nowhere to put it).
	if strings.Contains(string(res.FrozenManifest), "lock:") {
		t.Error("instruction-only frozen manifest must carry no lock block")
	}
	// The signature verifies over the reconstructed canonical bytes.
	lfNoHash, err := MarshalLockfile(res.Lock.withoutContentHash())
	if err != nil {
		t.Fatal(err)
	}
	mNoHash := []byte(strings.ReplaceAll(instructionOnlyMD, "\r\n", "\n"))
	if err := Verify(canonicalContent(mNoHash, lfNoHash), res.Signature, res.PublicKey); err != nil {
		t.Errorf("instruction-only signature must verify: %v", err)
	}
}

func TestRun_ValidationFailsBeforeResolution(t *testing.T) {
	// A manifest that fails the validation gate (here an out-of-enum step
	// kind, which manifest.Parse rejects, and which Lint would also reject)
	// must fail before any resolver is touched. The lint direction itself is
	// covered directly in lint_test.go against a schema-bypassing manifest;
	// this asserts Run's ordering: no container work runs on a rejected
	// manifest. The composer flips a flag if reached.
	const badMD = `---
name: bad-skill
description: Smuggles an LLM call.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:x.y
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields:
              - result
  environment:
    tools:
      - gh@2
  inputs: []
  outputs: []
  steps:
    - id: sneaky
      kind: action-call
      actionRef: aileron:x.y
---

# Bad
`
	_, keyPath := genSigningKey(t)
	// Swap the valid kind for an out-of-enum one. Run parses raw bytes, so
	// the rejection happens in the validation phase (parse + lint) that
	// precedes resolution; the composer must not be reached.
	composerHit := false
	_, err := Run(context.Background(), []byte(strings.Replace(badMD, "kind: action-call", "kind: not-a-kind", 1)), Options{
		SigningKeyPath: keyPath,
		Resolver:       dummyResolver(),
		Composer: FeatureComposerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
			composerHit = true
			return fakeDigest, nil
		}),
	})
	if err == nil {
		t.Fatal("a manifest with a bad step kind must fail Run")
	}
	if composerHit {
		t.Error("resolution must not run when validation fails")
	}
}

func TestRun_MissingSigningKey(t *testing.T) {
	t.Setenv(SigningKeyEnv, "")
	_, err := Run(context.Background(), exampleSkillMD(t), Options{Composer: fakeComposer(fakeDigest)})
	if err == nil {
		t.Error("a missing signing key must fail Run")
	}
}

func TestRun_ResolutionErrorSurfaces(t *testing.T) {
	// An environment-image manifest whose resolver errors must fail Run after
	// lint and key-load, exercising the resolveImages error branch in Run.
	_, keyPath := genSigningKey(t)
	dr := DigestResolverFunc(func(context.Context, string) (string, error) {
		return "", context.Canceled
	})
	_, err := Run(context.Background(), []byte(envImageMD), Options{
		SigningKeyPath: keyPath,
		Resolver:       dr,
	})
	if err == nil {
		t.Error("a resolver error must fail Run")
	}
}

func TestRun_TagNotDigestFails(t *testing.T) {
	// A resolver returning a tag (not a digest) must fail Run via the
	// pin-by-digest guard.
	_, keyPath := genSigningKey(t)
	dr := DigestResolverFunc(func(context.Context, string) (string, error) {
		return "registry.example.com/runner:1.4", nil
	})
	_, err := Run(context.Background(), []byte(envImageMD), Options{
		SigningKeyPath: keyPath,
		Resolver:       dr,
	})
	if err == nil {
		t.Error("a tag (non-digest) resolution must fail Run")
	}
}

func TestRun_BadManifestErrors(t *testing.T) {
	_, keyPath := genSigningKey(t)
	if _, err := Run(context.Background(), []byte("not a manifest"), Options{SigningKeyPath: keyPath}); err == nil {
		t.Error("an unparseable manifest must error")
	}
}
