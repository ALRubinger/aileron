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
		Composer:       fakeComposer(fakeDigest),
	})
	if err != nil {
		t.Fatalf("freeze.Run: %v", err)
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

func TestVerifyFrozen_ExposesResolvedImagesForRung2(t *testing.T) {
	// The worked example is rung-2: freeze composes the capability units to a
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

func TestVerifyFrozen_ExposesResolvedImagesForRung1(t *testing.T) {
	// A rung-1 unit names a whole prebuilt image; freeze resolves it to a
	// digest pin. The verified pin must carry both the ref and the digest.
	_, keyPath := genSigningKey(t)
	dr := DigestResolverFunc(func(_ context.Context, ref string) (string, error) {
		return fakeDigest, nil
	})
	res, err := Run(context.Background(), []byte(rung1MD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver:       dr,
	})
	if err != nil {
		t.Fatalf("freeze.Run rung-1: %v", err)
	}
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen rung-1: %v", err)
	}
	if len(v.ResolvedImages) != 1 {
		t.Fatalf("ResolvedImages = %+v, want exactly one pin", v.ResolvedImages)
	}
	if v.ResolvedImages[0].Digest != fakeDigest {
		t.Errorf("ResolvedImages[0].Digest = %q, want %q", v.ResolvedImages[0].Digest, fakeDigest)
	}
	if v.ResolvedImages[0].Ref != "registry.example.com/runner:1.4" {
		t.Errorf("ResolvedImages[0].Ref = %q, want the rung1Image.ref", v.ResolvedImages[0].Ref)
	}
}

func TestVerifyFrozen_EmptyResolvedImagesForNoExecEnv(t *testing.T) {
	// A no-execution-environment skill has an aileron block but declares no
	// image to resolve, so freeze pins none and VerifyFrozen exposes an empty
	// pin set (the runtime stays on the in-process parity path).
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(noExecEnvMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("freeze.Run no-exec-env: %v", err)
	}
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen no-exec-env: %v", err)
	}
	if len(v.ResolvedImages) != 0 {
		t.Errorf("no-exec-env ResolvedImages = %+v, want empty", v.ResolvedImages)
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
	// The worked example is rung-2: its lock pins a resolved image digest.
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
