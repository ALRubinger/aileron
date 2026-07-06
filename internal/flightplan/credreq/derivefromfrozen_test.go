package credreq

import (
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

// TestDeriveFromFrozen_NilStore proves the wrapper surfaces the load error
// rather than panicking when handed a nil store (delegating to
// runtime.LoadVerified's nil-store guard).
func TestDeriveFromFrozen_NilStore(t *testing.T) {
	if _, err := DeriveFromFrozen(nil, "any", "test"); err == nil {
		t.Fatal("DeriveFromFrozen(nil, ...) must return an error")
	}
}

// TestDeriveFromFrozen_MissingVersion proves the wrapper surfaces the store
// read error for a version that was never frozen.
func TestDeriveFromFrozen_MissingVersion(t *testing.T) {
	s := store.New(t.TempDir())
	if _, err := DeriveFromFrozen(s, "absent", "test"); err == nil {
		t.Fatal("DeriveFromFrozen for an absent version must return an error")
	}
}

// writeSigningKey writes a fresh ed25519 PKCS#8 signing key to a temp file and
// returns its path, mirroring the freeze/runtime test key convention.
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

// frozenSigv4MD is a schema-valid, environment-pinned SKILL.md with one
// aws-sigv4 tool step, the fixture the happy-path wrapper test freezes.
const frozenSigv4MD = `---
name: credreq-frozen-fixture
description: credreq frozen wrapper fixture.
aileron:
  schemaVersion: aileron.flightplan.v1
  environment:
    tools: [aws-cli@2.x]
  inputs: []
  outputs: []
  steps:
    - id: q1
      kind: tool
      command: [athena-query]
      outputs: [rows]
      trustContract:
        credential: { kind: aws-sigv4, placement: signing, identityLabel: prod-reader }
        hosts: ["athena.us-east-1.amazonaws.com"]
        effect: read
        idempotency: { safeToRetry: true }
        audit: { fields: [result] }
---
# frozen fixture
`

// TestDeriveFromFrozen_HappyPath proves the wrapper freezes-then-derives end to
// end: it reads and signature-verifies a frozen unit and returns the same set
// the pure Derive would produce from the decoded plan. This is the one wrapper
// integration test the plan asks for when a freeze helper is cheaply reachable.
func TestDeriveFromFrozen_HappyPath(t *testing.T) {
	res, err := freeze.Run(context.Background(), []byte(frozenSigv4MD), freeze.Options{
		Version:        "1.0.0",
		SigningKeyPath: writeSigningKey(t),
		Resolver: freeze.DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
			return "sha256:" + strings.Repeat("b", 64), nil
		}),
		Composer: freeze.FeatureComposerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
			return "sha256:" + strings.Repeat("a", 64), nil
		}),
	})
	if err != nil {
		t.Fatalf("freeze.Run: %v", err)
	}

	s := store.New(t.TempDir())
	const name = "credreq-frozen-fixture"
	if err := s.WriteFrozen(name, store.FrozenVersion{
		ID:        "test",
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}

	got, err := DeriveFromFrozen(s, name, "test")
	if err != nil {
		t.Fatalf("DeriveFromFrozen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bindings, want 1: %+v", len(got), got)
	}
	rb := got[0]
	if rb.CredentialKind != "aws-sigv4" || rb.Scheme != "sigv4-resign" || !rb.HostLessIdentity() {
		t.Errorf("binding = %+v, want aws-sigv4/sigv4-resign/host-less-identity", rb)
	}
	if rb.CredentialRef != "user/prod-reader" {
		t.Errorf("ref = %q, want user/prod-reader", rb.CredentialRef)
	}
}
