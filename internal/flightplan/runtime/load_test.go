package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// frozenExample freezes the committed worked example with a fresh signing key
// and returns the store.FrozenVersion the runtime loads. This is the shared
// fixture for load + run tests: the bytes are exactly what `skill freeze`
// would persist.
func frozenExample(t *testing.T) store.FrozenVersion {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "signing-key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	res, err := freeze.Run(context.Background(), raw, freeze.Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
		Resolver: freeze.DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
			return "sha256:" + strings.Repeat("b", 64), nil
		}),
		Composer: freeze.FeatureComposerFunc(func(_ context.Context, _ string, _ []string) ([]freeze.PlatformDigest, error) {
			return []freeze.PlatformDigest{
				{OS: "linux", Arch: "amd64", Digest: "sha256:" + strings.Repeat("a", 64)},
				{OS: "linux", Arch: "arm64", Digest: "sha256:" + strings.Repeat("a", 64)},
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("freeze.Run: %v", err)
	}
	return store.FrozenVersion{
		ID:        "test",
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}
}

// frozenNoImage freezes a no-environment variant of the worked example: the
// same fully-stepped plan with the `environment` block removed, so freeze
// pins no image. The runtime must stay on the in-process path for it.
// Deriving from the worked example keeps the plan a real, decodable step
// graph (the minimal no-step manifests Decode rejects) while isolating
// exactly the no-environment condition.
func frozenNoImage(t *testing.T) store.FrozenVersion {
	t.Helper()
	keyPath := writeSigningKey(t)

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}
	stripped := stripEnvironment(t, string(raw))

	res, err := freeze.Run(context.Background(), []byte(stripped), freeze.Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("freeze.Run no-environment variant: %v", err)
	}
	return store.FrozenVersion{
		ID:        "test",
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}
}

// stripEnvironment removes the `environment:` mapping (and its indented
// children) from the worked-example frontmatter, leaving a valid
// no-environment manifest. It drops the block by 2-space indent boundary:
// the block header sits at 2 spaces under the aileron block and its children
// are more deeply indented, so dropping from the header until the next line
// at 2-or-fewer spaces excises exactly the block.
func stripEnvironment(t *testing.T, md string) string {
	t.Helper()
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, ln := range lines {
		if !skipping {
			if strings.TrimSpace(ln) == "environment:" &&
				strings.HasPrefix(ln, "  environment:") {
				skipping = true
				continue
			}
			out = append(out, ln)
			continue
		}
		// While skipping: a line indented deeper than 2 spaces (or blank) is a
		// child of the block; a line at 2-or-fewer spaces ends it.
		trimmed := strings.TrimLeft(ln, " ")
		indent := len(ln) - len(trimmed)
		if trimmed == "" || indent > 2 {
			continue
		}
		skipping = false
		out = append(out, ln)
	}
	res := strings.Join(out, "\n")
	if strings.Contains(res, "environment:") {
		t.Fatalf("stripEnvironment left the block in place")
	}
	return res
}

// writeSigningKey generates a fresh ed25519 key, writes its PKCS#8 PEM to a
// temp file, and returns the path.
func writeSigningKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "signing-key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return keyPath
}

func TestLoadVerified_UntamperedLoads(t *testing.T) {
	fv := frozenExample(t)
	lp, err := verifyAndDecode(fv)
	if err != nil {
		t.Fatalf("verifyAndDecode rejected an untampered unit: %v", err)
	}
	if lp.Plan == nil || lp.Plan.Name != "weekly-metrics-digest" {
		t.Fatalf("decoded plan = %+v", lp.Plan)
	}
	if !strings.HasPrefix(lp.ContentHash, "sha256:") {
		t.Errorf("content hash = %q", lp.ContentHash)
	}
	// The verified signer fingerprint surfaces on the LoadedPlan so the launch
	// audit can record the plan's signer identity (#1752).
	if !strings.HasPrefix(lp.SignerFingerprint, "sha256:") {
		t.Errorf("SignerFingerprint = %q, want a sha256: prefix surfaced from verification", lp.SignerFingerprint)
	}
}

func TestLoadVerified_PropagatesResolvedImages(t *testing.T) {
	// The worked example declares an environment: its verified lock pins a composed image
	// digest. verifyAndDecode must carry that pin onto the LoadedPlan so Run can
	// boot the exact image the signature attested.
	fv := frozenExample(t)
	lp, err := verifyAndDecode(fv)
	if err != nil {
		t.Fatalf("verifyAndDecode: %v", err)
	}
	if len(lp.ResolvedImages) != 1 {
		t.Fatalf("ResolvedImages = %+v, want exactly one pin", lp.ResolvedImages)
	}
	// The worked example composes tools, so freeze pins a composed image by its
	// per-arch config-digest set; the host platform's entry must select to the
	// composer's digest.
	wantDigest := "sha256:" + strings.Repeat("a", 64)
	got, _, ok := lp.ResolvedImages[0].HostConfigDigest()
	if !ok {
		t.Fatalf("ResolvedImages[0] carries no config digest for the host platform: %+v", lp.ResolvedImages[0])
	}
	if got != wantDigest {
		t.Errorf("host config digest = %q, want %q", got, wantDigest)
	}
	if lp.ResolvedImages[0].Ref == "" {
		t.Error("ResolvedImages[0].Ref must carry the pre-freeze reference")
	}
}

// fakeImageRunner records the spec it was handed and returns a canned result,
// so the runtime boot-vs-in-process branch is testable with no live container.
type fakeImageRunner struct {
	called bool
	gotctx context.Context
	spec   ImageRunSpec
	result ImageRunResult
	err    error
}

func (f *fakeImageRunner) Run(ctx context.Context, spec ImageRunSpec) (ImageRunResult, error) {
	f.called = true
	f.gotctx = ctx
	f.spec = spec
	return f.result, f.err
}

func TestImageRunner_FakeSatisfiesInterface(t *testing.T) {
	// Compile-time and behavioral proof that the SPI is satisfiable by a fake:
	// the boot path never needs a live container to test.
	var r ImageRunner = &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:x"}}
	out, err := r.Run(context.Background(), ImageRunSpec{Image: "img@sha256:x"})
	if err != nil {
		t.Fatalf("fake ImageRunner: %v", err)
	}
	if out.ContentHash != "sha256:x" {
		t.Errorf("result ContentHash = %q, want the canned value", out.ContentHash)
	}
}

func TestLoadVerified_TamperedManifestRefuses(t *testing.T) {
	fv := frozenExample(t)
	fv.SkillMD = bytes.Replace(fv.SkillMD, []byte("Weekly Metrics Digest"), []byte("Weekly Metrics Digesz"), 1)
	if _, err := verifyAndDecode(fv); err == nil {
		t.Fatal("a tampered manifest must refuse to load")
	}
}

func TestLoadVerified_FlippedSignatureRefuses(t *testing.T) {
	fv := frozenExample(t)
	fv.Signature = append([]byte(nil), fv.Signature...)
	fv.Signature[0] ^= 0xFF
	if _, err := verifyAndDecode(fv); err == nil {
		t.Fatal("a flipped signature must refuse to load")
	}
}

func TestLoadVerified_ContentHashMismatchRefuses(t *testing.T) {
	fv := frozenExample(t)
	// Find and corrupt the recorded contentHash in the lockfile only, leaving
	// the manifest's recorded hash intact: the recomputed hash will diverge
	// because the lockfile bytes changed.
	fv.Lockfile = bytes.Replace(fv.Lockfile, []byte("resolvedCapabilitySet"), []byte("resolvedCapabilityseT"), 1)
	if _, err := verifyAndDecode(fv); err == nil {
		t.Fatal("a lockfile content change must refuse to load")
	}
}

func TestLoadVerified_MissingPublicKeyRefuses(t *testing.T) {
	fv := frozenExample(t)
	fv.PublicKey = []byte("not a pem key")
	if _, err := verifyAndDecode(fv); err == nil {
		t.Fatal("a missing/invalid public key must refuse to load")
	}
}

func TestLoadVerified_FromStore(t *testing.T) {
	fv := frozenExample(t)
	dir := t.TempDir()
	s := store.New(dir)
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	lp, err := LoadVerified(s, "weekly-metrics-digest", "test")
	if err != nil {
		t.Fatalf("LoadVerified: %v", err)
	}
	if lp.Plan.Name != "weekly-metrics-digest" {
		t.Errorf("name = %q", lp.Plan.Name)
	}
}

func TestLoadVerified_UnknownVersionRefuses(t *testing.T) {
	s := store.New(t.TempDir())
	if _, err := LoadVerified(s, "ghost", "nope"); err == nil {
		t.Fatal("an unknown version must refuse to load")
	}
}

func TestLoadVerified_LocallyFrozenHasNoImageOrigin(t *testing.T) {
	// A version written without an origin sidecar (a local freeze) loads with a
	// zero ImageOrigin, the signal runInImage uses to stay on the local-tag path.
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	lp, err := LoadVerified(s, "weekly-metrics-digest", "test")
	if err != nil {
		t.Fatalf("LoadVerified: %v", err)
	}
	if lp.ImageOrigin.Present {
		t.Errorf("locally-frozen plan has ImageOrigin.Present = true; want false")
	}
}

func TestLoadVerified_OCIInstalledCarriesImageOrigin(t *testing.T) {
	// A version with an origin sidecar (an OCI install) loads with the recorded
	// registry origin, so runInImage takes the registry-pull boot path.
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	if err := s.WriteOrigin("weekly-metrics-digest", "test", store.Origin{
		Registry:   "ghcr.io/acme/plan",
		VersionTag: "test",
	}); err != nil {
		t.Fatalf("WriteOrigin: %v", err)
	}
	lp, err := LoadVerified(s, "weekly-metrics-digest", "test")
	if err != nil {
		t.Fatalf("LoadVerified: %v", err)
	}
	if !lp.ImageOrigin.Present {
		t.Fatal("OCI-installed plan has ImageOrigin.Present = false; want true")
	}
	if lp.ImageOrigin.Registry != "ghcr.io/acme/plan" || lp.ImageOrigin.VersionTag != "test" {
		t.Errorf("ImageOrigin = %+v, want the recorded origin", lp.ImageOrigin)
	}
}
