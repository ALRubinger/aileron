package cstore

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func TestEd25519Keyring_VerifiesValidSignature(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)

	ring := NewEd25519Keyring()
	ring.Add("github://aileron/slack", pub)

	if err := ring.Verify("github://aileron/slack", binary, manifest, sig); err != nil {
		t.Errorf("Verify on valid signature err = %v", err)
	}
}

func TestEd25519Keyring_FailsWhenAuthorityHasNoKey(t *testing.T) {
	// ADR-0004's failure-modes table: signature-verification failure is a
	// hard fail with no "warning, you are about to install an unsigned
	// binary" downgrade. An authority with no registered key is the
	// no-trust-anchor case: must fail closed.
	ring := NewEd25519Keyring()
	err := ring.Verify("github://aileron/slack", []byte("x"), []byte("y"), []byte("z"))
	if err == nil {
		t.Fatal("Verify with no key registered succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassSignatureFailure {
		t.Fatalf("err class = %v, want ClassSignatureFailure", aerr)
	}
	if !strings.Contains(err.Error(), "no signing key") {
		t.Errorf("err = %q; want it to mention missing key", err.Error())
	}
}

func TestEd25519Keyring_FailsOnWrongKey(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, _ := signedManifest(t, binary, manifest)

	// Register an unrelated key only.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	ring := NewEd25519Keyring()
	ring.Add("github://aileron/slack", otherPub)

	verifyErr := ring.Verify("github://aileron/slack", binary, manifest, sig)
	if verifyErr == nil {
		t.Fatal("Verify with wrong key succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(verifyErr, &aerr) || aerr.Class != ClassSignatureFailure {
		t.Errorf("err class = %v, want ClassSignatureFailure", aerr)
	}
}

func TestEd25519Keyring_AcceptsAnyRegisteredKeyForRotation(t *testing.T) {
	// ADR-0002 §"signing-keys-and-rotation will be its own implementation
	// note": multiple keys per authority is the rotation primitive. A
	// signature valid under any one of the registered keys must succeed.
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)

	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	ring := NewEd25519Keyring()
	ring.Add("github://aileron/slack", otherPub) // expired/revoked
	ring.Add("github://aileron/slack", pub)      // current

	if err := ring.Verify("github://aileron/slack", binary, manifest, sig); err != nil {
		t.Errorf("Verify with rotation set err = %v", err)
	}
}

func TestEd25519Keyring_DifferentAuthorityIsDifferentTrustDomain(t *testing.T) {
	// ADR-0002: forks are distinct connectors. A key registered for
	// github://aileron/slack must NOT verify a binary fetched as
	// github://acme/slack — the authorities are different.
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)

	ring := NewEd25519Keyring()
	ring.Add("github://aileron/slack", pub)

	err := ring.Verify("github://acme/slack", binary, manifest, sig)
	if err == nil {
		t.Fatal("Verify across authorities succeeded; want failure")
	}
}

func TestPermissiveVerifier_AlwaysAccepts(t *testing.T) {
	// PermissiveVerifier is documented as test-only; this test holds it to
	// that contract so a future change that adds checks gets caught.
	v := PermissiveVerifier{}
	if err := v.Verify("anything", nil, nil, nil); err != nil {
		t.Errorf("PermissiveVerifier rejected nil input: %v", err)
	}
	if err := v.Verify("github://x/y", []byte("a"), []byte("b"), []byte("c")); err != nil {
		t.Errorf("PermissiveVerifier rejected normal input: %v", err)
	}
}
