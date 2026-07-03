package freeze

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// freezeExample freezes the committed worked example with a fresh signing key
// and returns the freeze Result. It is the shared fixture for the
// Launch-side verification tests: the bytes it produces are exactly what the
// store persists and what VerifyFrozen must accept.
func freezeExample(t *testing.T) Result {
	t.Helper()
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), exampleSkillMD(t), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dummyResolver(),
		Composer:       fakeComposer(fakeDigest),
	})
	if err != nil {
		t.Fatalf("freeze.Run: %v", err)
	}
	return res
}

// freezeToolSteps freezes the tool-steps fixture (two contracted tool steps)
// with a fresh signing key, the shared fixture for the verified stepTrust
// exposure tests.
func freezeToolSteps(t *testing.T) Result {
	t.Helper()
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(toolStepsMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dummyResolver(),
		Composer:       fakeComposer(fakeDigest2),
	})
	if err != nil {
		t.Fatalf("freeze.Run tool steps: %v", err)
	}
	return res
}

func TestVerifyFrozen_AcceptsUntamperedUnit(t *testing.T) {
	res := freezeExample(t)
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen rejected an untampered unit: %v", err)
	}
	if v.ContentHash != res.ContentHash {
		t.Errorf("verified contentHash = %q, want %q", v.ContentHash, res.ContentHash)
	}
	if !bytes.Equal(v.SkillMD, res.FrozenManifest) {
		t.Error("verified SkillMD must equal the stored frozen manifest bytes")
	}
}

func TestVerifyFrozen_ExposesResolvedImagesForEnvironmentTools(t *testing.T) {
	// The worked example declares environment tools: freeze composes them to a
	// single pinned image digest. VerifyFrozen must surface that verified pin so
	// the runtime can boot the exact image the signature attested.
	res := freezeExample(t)
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen: %v", err)
	}
	if len(v.ResolvedImages) != 1 {
		t.Fatalf("ResolvedImages = %+v, want exactly one pin", v.ResolvedImages)
	}
	if v.ResolvedImages[0].Digest != fakeDigest {
		t.Errorf("ResolvedImages[0].Digest = %q, want %q", v.ResolvedImages[0].Digest, fakeDigest)
	}
	if v.ResolvedImages[0].Ref == "" {
		t.Error("ResolvedImages[0].Ref must carry the pre-freeze reference")
	}
}

func TestVerifyFrozen_ExposesSignerFingerprint(t *testing.T) {
	// A successful verification surfaces a `sha256:`-prefixed fingerprint of the
	// verified author public key. It is the honest signer identity (there is no
	// human signer name), computed over the same pubPEM the signature verified.
	res := freezeExample(t)
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen: %v", err)
	}
	if !strings.HasPrefix(v.SignerFingerprint, "sha256:") {
		t.Errorf("SignerFingerprint = %q, want a sha256: prefix", v.SignerFingerprint)
	}
	if len(v.SignerFingerprint) != len("sha256:")+64 {
		t.Errorf("SignerFingerprint = %q, want sha256: + 64 hex chars", v.SignerFingerprint)
	}
	// The fingerprint is stable: verifying the same artifacts yields the same value.
	v2, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen (second): %v", err)
	}
	if v2.SignerFingerprint != v.SignerFingerprint {
		t.Errorf("fingerprint not stable: %q vs %q", v.SignerFingerprint, v2.SignerFingerprint)
	}
}

func TestVerifyFrozen_SignerFingerprintDiffersPerKey(t *testing.T) {
	// A different signing key yields a different fingerprint, so the value names
	// the specific key that attested the unit.
	a := freezeExample(t)
	b := freezeExample(t)
	va, err := VerifyFrozen(a.FrozenManifest, a.Lockfile, a.Signature, a.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen a: %v", err)
	}
	vb, err := VerifyFrozen(b.FrozenManifest, b.Lockfile, b.Signature, b.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen b: %v", err)
	}
	if bytes.Equal(a.PublicKey, b.PublicKey) {
		t.Skip("freeze reused a key; cannot assert per-key difference")
	}
	if va.SignerFingerprint == vb.SignerFingerprint {
		t.Errorf("different keys produced the same fingerprint %q", va.SignerFingerprint)
	}
}

func TestVerifyFrozen_ExposesResolvedImagesForEnvironmentImage(t *testing.T) {
	// An environment-image unit names a custom base image; freeze resolves it
	// to a digest pin. The verified pin must carry both the ref and the digest.
	_, keyPath := genSigningKey(t)
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		return fakeDigest, nil
	})
	res, err := Run(context.Background(), []byte(envImageMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dr,
	})
	if err != nil {
		t.Fatalf("freeze.Run environment-image: %v", err)
	}
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen environment-image: %v", err)
	}
	if len(v.ResolvedImages) != 1 {
		t.Fatalf("ResolvedImages = %+v, want exactly one pin", v.ResolvedImages)
	}
	if v.ResolvedImages[0].Digest != fakeDigest {
		t.Errorf("ResolvedImages[0].Digest = %q, want %q", v.ResolvedImages[0].Digest, fakeDigest)
	}
	if v.ResolvedImages[0].Ref != "registry.example.com/runner:1.4" {
		t.Errorf("ResolvedImages[0].Ref = %q, want the environment.image ref", v.ResolvedImages[0].Ref)
	}
}

// TestVerifyFrozen_ExposesStepTrust proves launch/runtime read the sealed
// reach off the verified path: a successful verification surfaces the
// step-keyed trust section exactly as the manifest's verified lock block
// recorded it, defensively copied.
func TestVerifyFrozen_ExposesStepTrust(t *testing.T) {
	res := freezeToolSteps(t)
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen: %v", err)
	}
	if len(v.StepTrust) != 2 {
		t.Fatalf("StepTrust = %+v, want the two contracted steps", v.StepTrust)
	}
	if strings.Join(v.StepTrust["fetch"].Hosts, ",") != "s3.amazonaws.com,s3.amazonaws.com:443" {
		t.Errorf("fetch reach = %v", v.StepTrust["fetch"].Hosts)
	}
	if strings.Join(v.StepTrust["file"].Hosts, ",") != "api.github.com" {
		t.Errorf("file reach = %v", v.StepTrust["file"].Hosts)
	}
	// A skill sealing no reach exposes a nil section.
	plain := freezeExample(t)
	vp, err := VerifyFrozen(plain.FrozenManifest, plain.Lockfile, plain.Signature, plain.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen (no tool steps): %v", err)
	}
	if vp.StepTrust != nil {
		t.Errorf("a unit sealing no reach must expose nil StepTrust, got %+v", vp.StepTrust)
	}
}

// TestVerifyFrozen_TamperedStepTrustRefuses is the sign-coverage regression
// for the step-keyed section: widening a sealed host inside the frozen
// manifest's lock block after signing changes the recomputed content hash,
// so verification refuses before any reach is exposed.
func TestVerifyFrozen_TamperedStepTrustRefuses(t *testing.T) {
	res := freezeToolSteps(t)
	// The lock block is injected at the end of the aileron mapping, so the
	// LAST occurrence of the sealed host is the lock's stepTrust entry (the
	// earlier occurrences are the step's declared contract in the signed
	// frontmatter). Tamper only that sealed entry.
	target := []byte("api.github.com")
	idx := bytes.LastIndex(res.FrozenManifest, target)
	if idx < 0 {
		t.Fatal("test setup: sealed host not found in the frozen manifest")
	}
	tampered := append([]byte(nil), res.FrozenManifest[:idx]...)
	tampered = append(tampered, []byte("api.evil-example.com")...)
	tampered = append(tampered, res.FrozenManifest[idx+len(target):]...)
	v, err := VerifyFrozen(tampered, res.Lockfile, res.Signature, res.PublicKey)
	if err == nil {
		t.Fatal("a tampered stepTrust section must refuse to verify")
	}
	if len(v.StepTrust) != 0 {
		t.Errorf("a refused verification must expose no sealed reach, got %+v", v.StepTrust)
	}
}

func TestVerifyFrozen_EmptyResolvedImagesForNoEnvironment(t *testing.T) {
	// A no-environment skill has an aileron block but declares no image to
	// resolve, so freeze pins none and VerifyFrozen exposes an empty pin set
	// (the runtime stays on the in-process parity path).
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(noEnvironmentMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("freeze.Run no-environment: %v", err)
	}
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen no-environment: %v", err)
	}
	if len(v.ResolvedImages) != 0 {
		t.Errorf("no-environment ResolvedImages = %+v, want empty", v.ResolvedImages)
	}
}

func TestVerifyFrozen_TamperedDigestRefusesBeforeExposure(t *testing.T) {
	// Swapping the resolved digest inside the frozen manifest's lock block
	// changes the recomputed content hash. Verification must refuse before any
	// pin is exposed, so a tampered image can never reach the runtime.
	res := freezeExample(t)
	swapped := "sha256:" + strings.Repeat("b", 64)
	tampered := bytes.Replace(res.FrozenManifest, []byte(fakeDigest), []byte(swapped), 1)
	if bytes.Equal(tampered, res.FrozenManifest) {
		t.Fatal("test setup: worked example lock digest not found to tamper")
	}
	v, err := VerifyFrozen(tampered, res.Lockfile, res.Signature, res.PublicKey)
	if err == nil {
		t.Fatal("a tampered resolved digest must refuse to verify")
	}
	if len(v.ResolvedImages) != 0 {
		t.Errorf("a refused verification must expose no pins, got %+v", v.ResolvedImages)
	}
}

func TestVerifyFrozen_RejectsTamperedManifest(t *testing.T) {
	res := freezeExample(t)
	// Flip a byte in the Markdown body (after the frontmatter) so the
	// manifest no longer matches the signed canonical bytes.
	tampered := bytes.Replace(res.FrozenManifest,
		[]byte("Weekly Metrics Digest"), []byte("Weekly Metrics Digesz"), 1)
	if bytes.Equal(tampered, res.FrozenManifest) {
		t.Fatal("test setup: expected to alter the manifest body")
	}
	if _, err := VerifyFrozen(tampered, res.Lockfile, res.Signature, res.PublicKey); err == nil {
		t.Fatal("VerifyFrozen accepted a tampered manifest")
	}
}

func TestVerifyFrozen_RejectsTamperedSignature(t *testing.T) {
	res := freezeExample(t)
	sig := append([]byte(nil), res.Signature...)
	sig[0] ^= 0xFF
	if _, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, sig, res.PublicKey); err == nil {
		t.Fatal("VerifyFrozen accepted a tampered signature")
	}
}

func TestVerifyFrozen_RejectsContentHashMismatch(t *testing.T) {
	res := freezeExample(t)
	// Rewrite the recorded contentHash in the frozen manifest to a different
	// (but well-formed) digest. The recomputed hash will no longer match.
	bad := "sha256:" + strings.Repeat("0", 64)
	tampered := bytes.Replace(res.FrozenManifest, []byte(res.ContentHash), []byte(bad), 1)
	if bytes.Equal(tampered, res.FrozenManifest) {
		t.Fatal("test setup: contentHash not found in frozen manifest")
	}
	if _, err := VerifyFrozen(tampered, res.Lockfile, res.Signature, res.PublicKey); err == nil {
		t.Fatal("VerifyFrozen accepted a manifest whose recorded hash was altered")
	}
}

func TestVerifyFrozen_RejectsMissingLockBlock(t *testing.T) {
	// An unfrozen manifest (no lock block) is not a frozen unit.
	if _, err := VerifyFrozen(exampleSkillMD(t), []byte("{}\n"), []byte("sig"), []byte("pub")); err == nil {
		t.Fatal("VerifyFrozen accepted an unfrozen manifest")
	}
}

func TestVerifyFrozen_RejectsTamperedManifestLockBlock(t *testing.T) {
	res := freezeExample(t)
	// The worked example declares environment tools: its lock pins a resolved
	// composed-image digest.
	// Swap that digest inside the frozen manifest's lock block while leaving
	// the standalone lockfile and the recorded contentHash untouched. The
	// reconstruction rebuilds the manifest region from the manifest's OWN lock,
	// so the recomputed hash diverges and verification must refuse. A rebuild
	// that healed the manifest from the lockfile would wrongly accept this.
	orig := fakeDigest
	swapped := "sha256:" + strings.Repeat("b", 64)
	tampered := bytes.Replace(res.FrozenManifest, []byte(orig), []byte(swapped), 1)
	if bytes.Equal(tampered, res.FrozenManifest) {
		t.Fatal("test setup: worked example lock digest not found in the frozen manifest to tamper")
	}
	if _, err := VerifyFrozen(tampered, res.Lockfile, res.Signature, res.PublicKey); err == nil {
		t.Fatal("a tampered manifest lock block must refuse to verify")
	}
}

func TestVerifyFrozen_DefensiveCopyOfSkillMD(t *testing.T) {
	res := freezeExample(t)
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen: %v", err)
	}
	// Mutating the caller's slice must not change the verified bytes.
	if len(res.FrozenManifest) > 0 {
		res.FrozenManifest[0] ^= 0xFF
	}
	if bytes.Equal(v.SkillMD, res.FrozenManifest) {
		t.Error("VerifiedFrozen.SkillMD must be a defensive copy, not alias the caller's slice")
	}
}

func TestVerifyFrozen_RejectsInconsistentLockfileHash(t *testing.T) {
	res := freezeExample(t)
	// Alter the standalone lockfile's recorded contentHash so it disagrees with
	// the manifest's recorded hash. The two artifacts are inconsistent and
	// verification must refuse.
	bad := "sha256:" + strings.Repeat("c", 64)
	tampered := bytes.Replace(res.Lockfile, []byte(res.ContentHash), []byte(bad), 1)
	if bytes.Equal(tampered, res.Lockfile) {
		t.Fatal("test setup: contentHash not found in the lockfile")
	}
	if _, err := VerifyFrozen(res.FrozenManifest, tampered, res.Signature, res.PublicKey); err == nil {
		t.Fatal("a lockfile whose recorded hash disagrees with the manifest must refuse")
	}
}

func TestVerifyFrozen_RejectsNoFrontmatter(t *testing.T) {
	if _, err := VerifyFrozen([]byte("no frontmatter here\n"), []byte("{}\n"), []byte("s"), []byte("p")); err == nil {
		t.Fatal("a manifest with no frontmatter must refuse")
	}
}
