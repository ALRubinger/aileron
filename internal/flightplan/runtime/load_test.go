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
		Composer: freeze.FeatureComposerFunc(func(_ context.Context, _ []string) (string, error) {
			return "sha256:" + strings.Repeat("a", 64), nil
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
