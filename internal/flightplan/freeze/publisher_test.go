package freeze

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// freezeWithPublisher freezes the no-environment fixture attributed to the
// given publisher with a fresh signing key. Used by the VerifyFrozen exposure
// tests.
func freezeWithPublisher(t *testing.T, publisher string) Result {
	t.Helper()
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(noEnvironmentMD), Options{
		Version:        "1.0.0",
		Publisher:      publisher,
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("freeze.Run with publisher %q: %v", publisher, err)
	}
	return res
}

// TestVerifyFrozen_ExposesPublisherAndSignerKey proves the verified path
// surfaces the declared publisher and the raw ed25519 signing key so the
// launch gate can check membership.
func TestVerifyFrozen_ExposesPublisherAndSignerKey(t *testing.T) {
	res := freezeWithPublisher(t, "github://acme/flightplans")
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen: %v", err)
	}
	if v.Publisher != "github://acme/flightplans" {
		t.Errorf("Publisher = %q, want github://acme/flightplans", v.Publisher)
	}
	if len(v.SignerKey) != ed25519.PublicKeySize {
		t.Fatalf("SignerKey length = %d, want %d", len(v.SignerKey), ed25519.PublicKeySize)
	}
	// The exposed signing key is the one the signature verified against.
	wantKey, err := parsePublicKeyPEM(res.PublicKey)
	if err != nil {
		t.Fatalf("parse res public key: %v", err)
	}
	if !v.SignerKey.Equal(wantKey) {
		t.Error("SignerKey must equal the verified author public key")
	}
}

// TestVerifyFrozen_NoPublisherYieldsEmpty proves a plan frozen without a
// publisher exposes an empty Publisher (so the launch gate is skipped) while
// still exposing a non-nil signer key.
func TestVerifyFrozen_NoPublisherYieldsEmpty(t *testing.T) {
	res := freezeWithPublisher(t, "")
	v, err := VerifyFrozen(res.FrozenManifest, res.Lockfile, res.Signature, res.PublicKey)
	if err != nil {
		t.Fatalf("VerifyFrozen: %v", err)
	}
	if v.Publisher != "" {
		t.Errorf("Publisher = %q, want empty for a publisher-less plan", v.Publisher)
	}
	if len(v.SignerKey) != ed25519.PublicKeySize {
		t.Errorf("SignerKey must be populated even without a publisher, got len %d", len(v.SignerKey))
	}
}

// TestVerifyFrozen_TamperedPublisherRefuses proves editing the declared
// publisher in the frozen manifest after signing changes the recomputed
// content hash and refuses before the publisher is read.
func TestVerifyFrozen_TamperedPublisherRefuses(t *testing.T) {
	res := freezeWithPublisher(t, "github://acme/flightplans")
	tampered := strings.Replace(string(res.FrozenManifest),
		"publisher: github://acme/flightplans",
		"publisher: github://attacker/evil", 1)
	if tampered == string(res.FrozenManifest) {
		t.Fatal("test setup: publisher line not found to tamper")
	}
	if _, err := VerifyFrozen([]byte(tampered), res.Lockfile, res.Signature, res.PublicKey); err == nil {
		t.Error("a tampered publisher must refuse verification")
	}
}

// TestLockfile_PublisherRoundTrips proves the publisher authority survives a
// Marshal/Unmarshal round-trip through the standalone lockfile and the spliced
// lock node, under the schema field name.
func TestLockfile_PublisherRoundTrips(t *testing.T) {
	l := sampleLock()
	l.Publisher = "github://acme/flightplans"

	b, err := MarshalLockfile(l)
	if err != nil {
		t.Fatalf("MarshalLockfile: %v", err)
	}
	if !strings.Contains(string(b), "publisher: github://acme/flightplans") {
		t.Errorf("lockfile missing publisher field:\n%s", b)
	}
	var got Lockfile
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Publisher != l.Publisher {
		t.Errorf("Publisher = %q, want %q", got.Publisher, l.Publisher)
	}

	// The spliced lock node carries it too.
	node, err := lockNode(l)
	if err != nil {
		t.Fatalf("lockNode: %v", err)
	}
	var back Lockfile
	if err := node.Decode(&back); err != nil {
		t.Fatalf("decode lock node: %v", err)
	}
	if back.Publisher != l.Publisher {
		t.Errorf("spliced Publisher = %q, want %q", back.Publisher, l.Publisher)
	}
}

// TestWithoutContentHash_KeepsPublisher proves the publisher participates in
// the content hash: withoutContentHash clears only the hash, so a declared
// publisher is retained in the hashed bytes and is therefore covered by the
// signature (it cannot be re-supplied at launch).
func TestWithoutContentHash_KeepsPublisher(t *testing.T) {
	l := sampleLock()
	l.Publisher = "github://acme"
	got := l.withoutContentHash()
	if got.Publisher != "github://acme" {
		t.Errorf("withoutContentHash dropped Publisher: %q", got.Publisher)
	}
}

// TestRun_PublisherRecordedInLockAndArtifacts proves freeze --publisher lands
// in Result.Lock and both emitted artifacts (the standalone lockfile and the
// manifest's embedded lock block).
func TestRun_PublisherRecordedInLockAndArtifacts(t *testing.T) {
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(noEnvironmentMD), Options{
		Version:        "1.0.0",
		Publisher:      "github://acme/flightplans",
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Lock.Publisher != "github://acme/flightplans" {
		t.Errorf("Result.Lock.Publisher = %q", res.Lock.Publisher)
	}
	if !strings.Contains(string(res.Lockfile), "publisher: github://acme/flightplans") {
		t.Errorf("standalone lockfile missing publisher:\n%s", res.Lockfile)
	}
	if !strings.Contains(string(res.FrozenManifest), "publisher: github://acme/flightplans") {
		t.Errorf("frozen manifest lock block missing publisher:\n%s", res.FrozenManifest)
	}
}

// TestRun_DifferentPublisherChangesContentHash proves two freezes that differ
// only in the declared publisher produce different content hashes (the
// publisher is inside the hashed bytes).
func TestRun_DifferentPublisherChangesContentHash(t *testing.T) {
	_, keyPath := genSigningKey(t)
	base := Options{Version: "1.0.0", SigningKeyPath: keyPath}
	optsA := base
	optsA.Publisher = "github://acme"
	optsB := base
	optsB.Publisher = "github://other"

	a, err := Run(context.Background(), []byte(noEnvironmentMD), optsA)
	if err != nil {
		t.Fatalf("Run a: %v", err)
	}
	b, err := Run(context.Background(), []byte(noEnvironmentMD), optsB)
	if err != nil {
		t.Fatalf("Run b: %v", err)
	}
	if a.ContentHash == b.ContentHash {
		t.Error("a different publisher must change the content hash")
	}
}

// TestRun_OmittedPublisherByteIdenticalToPrePublisher proves omitting
// --publisher yields a lock with no publisher field (omitempty), so a
// publisher-less freeze is byte-identical to the pre-publisher format: no
// `publisher:` key appears anywhere in the artifacts.
func TestRun_OmittedPublisherByteIdenticalToPrePublisher(t *testing.T) {
	_, keyPath := genSigningKey(t)
	res, err := Run(context.Background(), []byte(noEnvironmentMD), Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Lock.Publisher != "" {
		t.Errorf("omitted publisher must leave Lock.Publisher empty, got %q", res.Lock.Publisher)
	}
	if strings.Contains(string(res.Lockfile), "publisher:") {
		t.Errorf("omitted publisher must not emit a publisher key:\n%s", res.Lockfile)
	}
	if strings.Contains(string(res.FrozenManifest), "publisher:") {
		t.Errorf("omitted publisher must not emit a publisher key in the manifest:\n%s", res.FrozenManifest)
	}
}
