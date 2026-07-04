package cstore

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// mustGenKey returns a fresh ed25519 public key for the trust-resolution tests.
func mustGenKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub
}

// TestPublisherTrust_TrustedViaOwnerGrant proves an owner-level grant makes the
// signing key trusted for a per-repo authority under that owner.
func TestPublisherTrust_TrustedViaOwnerGrant(t *testing.T) {
	key := mustGenKey(t)
	ring := NewEd25519Keyring()
	ring.AddOwner("github://acme", key)

	res, err := ring.PublisherTrust("github://acme/plans", key)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if !res.Trusted {
		t.Error("an owner-level grant must trust the key for any repo under that owner")
	}
	if res.Conflict {
		t.Error("a single owner grant must not report a conflict")
	}
}

// TestPublisherTrust_TrustedViaPerRepoGrant proves a per-repo grant alone makes
// the signing key trusted for exactly that authority.
func TestPublisherTrust_TrustedViaPerRepoGrant(t *testing.T) {
	key := mustGenKey(t)
	ring := NewEd25519Keyring()
	ring.Add("github://acme/plans", key)

	res, err := ring.PublisherTrust("github://acme/plans", key)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if !res.Trusted {
		t.Error("a per-repo grant must trust the key for that authority")
	}
	if res.Conflict {
		t.Error("a per-repo-only grant must not report a conflict")
	}
}

// TestPublisherTrust_UntrustedEmptyUnion proves fail-closed: an empty keyring
// trusts no key.
func TestPublisherTrust_UntrustedEmptyUnion(t *testing.T) {
	key := mustGenKey(t)
	ring := NewEd25519Keyring()

	res, err := ring.PublisherTrust("github://acme/plans", key)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if res.Trusted {
		t.Error("an empty union must fail closed (Trusted=false)")
	}
}

// TestPublisherTrust_UntrustedKeyDivergence proves a non-empty union that does
// not contain the signing key fails closed: the publisher is trusted for OTHER
// keys, but this specific signing key is not a member.
func TestPublisherTrust_UntrustedKeyDivergence(t *testing.T) {
	trustedKey := mustGenKey(t)
	planKey := mustGenKey(t)
	ring := NewEd25519Keyring()
	ring.AddOwner("github://acme", trustedKey)

	res, err := ring.PublisherTrust("github://acme/plans", planKey)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if res.Trusted {
		t.Error("a signing key not in the non-empty union must fail closed")
	}
}

// TestPublisherTrust_ConflictWhenScopesDiffer proves the P2 conflict signal:
// the signing key is a member of both the owner and per-repo scopes but the two
// key sets differ. Membership passes (Trusted=true) and Conflict=true.
func TestPublisherTrust_ConflictWhenScopesDiffer(t *testing.T) {
	shared := mustGenKey(t)
	ownerExtra := mustGenKey(t)
	ring := NewEd25519Keyring()
	// Owner scope has {shared, ownerExtra}; per-repo scope has {shared}.
	ring.AddOwner("github://acme", shared)
	ring.AddOwner("github://acme", ownerExtra)
	ring.Add("github://acme/plans", shared)

	res, err := ring.PublisherTrust("github://acme/plans", shared)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if !res.Trusted {
		t.Fatal("the shared key is a member of both scopes; must be trusted")
	}
	if !res.Conflict {
		t.Error("both scopes non-empty and divergent must report a conflict")
	}
}

// TestPublisherTrust_NoConflictWhenScopesAgree proves identical key sets in
// both scopes trust the key with no conflict.
func TestPublisherTrust_NoConflictWhenScopesAgree(t *testing.T) {
	key := mustGenKey(t)
	ring := NewEd25519Keyring()
	ring.AddOwner("github://acme", key)
	ring.Add("github://acme/plans", key)

	res, err := ring.PublisherTrust("github://acme/plans", key)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if !res.Trusted {
		t.Fatal("agreeing scopes must trust the key")
	}
	if res.Conflict {
		t.Error("scopes that agree on key material must not report a conflict")
	}
}

// TestPublisherTrust_BareOwnerAuthorityResolves proves a bare-owner publisher
// authority (`github://owner`, which ParseFQN rejects) resolves against the
// owner grant the keyring stores under that exact string. This is the shape a
// Flight Plan frozen with `--publisher github://owner` declares.
func TestPublisherTrust_BareOwnerAuthorityResolves(t *testing.T) {
	key := mustGenKey(t)
	ring := NewEd25519Keyring()
	ring.AddOwner("github://acme", key)

	res, err := ring.PublisherTrust("github://acme", key)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if !res.Trusted {
		t.Error("a bare-owner publisher must resolve against the owner grant")
	}
	// A different key under the same bare owner is not trusted.
	res, err = ring.PublisherTrust("github://acme", mustGenKey(t))
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if res.Trusted {
		t.Error("a bare-owner publisher must fail closed for an unregistered key")
	}
}

// TestPublisherTrust_MalformedAuthorityStaysFailClosed proves a malformed
// authority degrades to per-repo-only resolution (no owner widening) and stays
// fail-closed, mirroring Verify's documented degrade path.
func TestPublisherTrust_MalformedAuthorityStaysFailClosed(t *testing.T) {
	key := mustGenKey(t)
	ring := NewEd25519Keyring()
	// A key registered under the raw (unparseable) authority resolves; nothing
	// else does.
	ring.Add("not-an-authority", key)

	// The exact raw string resolves via the per-repo scope.
	res, err := ring.PublisherTrust("not-an-authority", key)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if !res.Trusted {
		t.Error("a per-repo grant under the raw authority must still resolve")
	}
	// A different key under the same malformed authority is not trusted (no
	// owner widening could rescue it).
	other := mustGenKey(t)
	res, err = ring.PublisherTrust("not-an-authority", other)
	if err != nil {
		t.Fatalf("PublisherTrust: %v", err)
	}
	if res.Trusted {
		t.Error("a malformed authority must not widen trust to an unregistered key")
	}
}
