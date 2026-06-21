package cstore

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
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

// TestEd25519Keyring_OwnerLevelGrantAuthorizesEveryRepo pins the core
// ADR-0013 feature: an owner-level grant (`<scheme>://<owner>`, no repo
// segment) authorizes a signature for *every* repo under that owner with
// no per-repo entry. Trust once by identity; all repos are covered.
func TestEd25519Keyring_OwnerLevelGrantAuthorizesEveryRepo(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)

	ring := NewEd25519Keyring()
	ring.AddOwner("github://aileron", pub) // owner-level only, no per-repo

	for _, authority := range []string{"github://aileron/repoA", "github://aileron/repoB"} {
		if err := ring.Verify(authority, binary, manifest, sig); err != nil {
			t.Errorf("Verify(%q) under owner-level grant err = %v", authority, err)
		}
	}
}

// TestEd25519Keyring_UntrustedOwnerFailsClosed pins fail-closed for the
// owner dimension: with neither an owner-level grant nor a per-repo
// grant, Verify must reject with ClassSignatureFailure and the
// "no signing key" diagnosis.
func TestEd25519Keyring_UntrustedOwnerFailsClosed(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, _ := signedManifest(t, binary, manifest)

	ring := NewEd25519Keyring() // empty: no owner-level, no per-repo
	err := ring.Verify("github://aileron/slack", binary, manifest, sig)
	if err == nil {
		t.Fatal("Verify with no grant of any scope succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassSignatureFailure {
		t.Fatalf("err class = %v, want ClassSignatureFailure", aerr)
	}
	if !strings.Contains(err.Error(), "no signing key") {
		t.Errorf("err = %q; want it to mention missing key", err.Error())
	}
}

// TestEd25519Keyring_PerRepoGrantStillAuthorizes guards the v1 path:
// a per-repo grant with no owner-level grant present still authorizes
// exactly its FQN. The owner-level union must not regress per-repo-only
// behavior.
func TestEd25519Keyring_PerRepoGrantStillAuthorizes(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)

	ring := NewEd25519Keyring()
	ring.Add("github://aileron/slack", pub) // per-repo only, no owner-level

	if err := ring.Verify("github://aileron/slack", binary, manifest, sig); err != nil {
		t.Errorf("Verify under per-repo-only grant err = %v", err)
	}
}

// TestEd25519Keyring_UnionAcceptsEitherScope pins the no-deny-list union
// semantics: when an owner-level grant (key A) and a per-repo grant
// (key B) both exist for the same authority, a signature under A AND a
// signature under B each verify. Neither scope shadows the other.
func TestEd25519Keyring_UnionAcceptsEitherScope(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sigA, pubA := signedManifest(t, binary, manifest) // owner-level key
	sigB, pubB := signedManifest(t, binary, manifest) // per-repo key

	ring := NewEd25519Keyring()
	ring.AddOwner("github://aileron", pubA)
	ring.Add("github://aileron/slack", pubB)

	if err := ring.Verify("github://aileron/slack", binary, manifest, sigA); err != nil {
		t.Errorf("Verify under owner-level key (A) err = %v", err)
	}
	if err := ring.Verify("github://aileron/slack", binary, manifest, sigB); err != nil {
		t.Errorf("Verify under per-repo key (B) err = %v", err)
	}
}

// TestEd25519Keyring_OwnerDerivationIsSchemeAgnostic confirms owner
// derivation uses ParseFQN (not a hardcoded `github`): an owner-level
// grant under gitlab:// authorizes a gitlab:// repo.
func TestEd25519Keyring_OwnerDerivationIsSchemeAgnostic(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)

	ring := NewEd25519Keyring()
	ring.AddOwner("gitlab://acme", pub)

	if err := ring.Verify("gitlab://acme/widgets", binary, manifest, sig); err != nil {
		t.Errorf("Verify(gitlab://acme/widgets) under gitlab owner grant err = %v", err)
	}
}

// TestEd25519Keyring_OwnerGrantDoesNotLeakAcrossOwners pins that the
// derived owner-level key is owner-scoped: an owner-level grant for
// github://aileron must NOT authorize github://acme/slack (different
// owner). The "different authority is a different trust domain"
// invariant holds for the owner dimension too.
func TestEd25519Keyring_OwnerGrantDoesNotLeakAcrossOwners(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)

	ring := NewEd25519Keyring()
	ring.AddOwner("github://aileron", pub)

	err := ring.Verify("github://acme/slack", binary, manifest, sig)
	if err == nil {
		t.Fatal("Verify across owners under owner-level grant succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassSignatureFailure {
		t.Fatalf("err class = %v, want ClassSignatureFailure", aerr)
	}
}

// TestEd25519Keyring_MalformedAuthorityStaysFailClosed pins the single
// behavioral risk: a malformed authority must not widen trust. The
// ParseFQN error path degrades to per-repo-only resolution; with no
// matching per-repo entry it fails closed (and never panics), even when
// the keyring is non-empty.
func TestEd25519Keyring_MalformedAuthorityStaysFailClosed(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)

	// Non-empty keyring: an owner-level grant and a per-repo grant under
	// a well-formed owner exist, but the queried authority is malformed.
	ring := NewEd25519Keyring()
	ring.AddOwner("github://aileron", pub)
	ring.Add("github://aileron/slack", pub)

	for _, bad := range []string{"not-a-valid-fqn", "ftp://aileron/slack", "github://other-owner"} {
		err := ring.Verify(bad, binary, manifest, sig)
		if err == nil {
			t.Fatalf("Verify(%q) on malformed/owner-only authority succeeded; want failure", bad)
		}
		var aerr *Error
		if !errors.As(err, &aerr) || aerr.Class != ClassSignatureFailure {
			t.Fatalf("Verify(%q) err class = %v, want ClassSignatureFailure", bad, aerr)
		}
	}
}

// TestReloadingKeyring_PicksUpFileChangesWithoutDaemonRestart pins the
// fix for finding #4 of #532: when the user runs `aileron keyring trust
// <authority> <pubkey>`, the CLI writes to ~/.aileron/keyring.json
// directly and does not notify the daemon. With ReloadingKeyring as
// the install pipeline's verifier, a subsequent `aileron action add`
// must observe the new key on the next install — no `aileron stop`
// required.
//
// Repro: write keyring with key A, Verify against a binary signed by
// key B → ClassSignatureFailure ("no signing key registered" or
// "signature does not verify"). Add key B to the file. Verify again
// → must succeed without any explicit reload from the test.
func TestReloadingKeyring_PicksUpFileChangesWithoutDaemonRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")

	binary := []byte("BIN")
	manifest := []byte("MAN")
	sig, pub := signedManifest(t, binary, manifest)
	const authority = "github://acme/connector"

	// Step 1: keyring file exists but does NOT trust the signing key.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	writeKeyringFile(t, path, authority, []ed25519.PublicKey{otherPub})

	v := &ReloadingKeyring{Path: path}
	if err := v.Verify(authority, binary, manifest, sig); err == nil {
		t.Fatal("Verify before adding pub should fail; got nil")
	}

	// Step 2: user runs `aileron keyring trust` — writes the pub to
	// the file. The verifier must observe it on the next call without
	// any explicit reload.
	writeKeyringFile(t, path, authority, []ed25519.PublicKey{otherPub, pub})

	if err := v.Verify(authority, binary, manifest, sig); err != nil {
		t.Errorf("Verify after adding pub should succeed; got %v", err)
	}
}

// TestReloadingKeyring_MissingFileFailsClosed pins the
// no-trust-anchor case: when ~/.aileron/keyring.json does not exist,
// Verify must reject every install with ClassSignatureFailure. This
// matches the daemon-startup behavior LoadKeyring already implements
// for missing files.
func TestReloadingKeyring_MissingFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	v := &ReloadingKeyring{Path: path}
	err := v.Verify("github://acme/connector", []byte("x"), []byte("y"), []byte("z"))
	if err == nil {
		t.Fatal("Verify with missing keyring file succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassSignatureFailure {
		t.Fatalf("err class = %v, want ClassSignatureFailure", aerr)
	}
}

// TestReloadingKeyring_MalformedFileSurfacesError pins the
// fail-closed-with-cause case: when the keyring file is unparseable,
// Verify must surface the parse error rather than silently falling
// back to an empty keyring (which would say "no signing key" — wrong
// diagnosis for a syntax error in the file the user just edited).
func TestReloadingKeyring_MalformedFileSurfacesError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	v := &ReloadingKeyring{Path: path}
	err := v.Verify("github://acme/connector", []byte("x"), []byte("y"), []byte("z"))
	if err == nil {
		t.Fatal("Verify with malformed keyring succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassValidationError {
		t.Fatalf("err class = %v, want ClassValidationError (malformed JSON should surface, not fall back to empty)", aerr)
	}
}

// writeKeyringFile is a test helper that writes a v1 keyring file
// trusting `pubs` for `authority`. Distinct from Ed25519Keyring's
// SaveKeyring so the test exercises the file format directly without
// depending on Save's semantics.
func writeKeyringFile(t *testing.T, path, authority string, pubs []ed25519.PublicKey) {
	t.Helper()
	encoded := make([]string, 0, len(pubs))
	for _, p := range pubs {
		encoded = append(encoded, base64.StdEncoding.EncodeToString(p))
	}
	body := `{"version":1,"publishers":{` +
		`"` + authority + `":[`
	for i, e := range encoded {
		if i > 0 {
			body += ","
		}
		body += `"` + e + `"`
	}
	body += `]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write keyring: %v", err)
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
