package nodedist_test

import (
	"bytes"
	"testing"

	"golang.org/x/crypto/openpgp" //nolint:staticcheck // test signing primitive mirrors the production verify path.
)

// keyring aliases the openpgp.EntityList used as the trusted Node release key
// set in tests.
type keyring = openpgp.EntityList

// signer wraps a PGP entity used to sign test fixtures.
type signer struct {
	entity *openpgp.Entity
}

// armoredDetachedSign returns an armored detached signature over msg, the
// same shape as SHASUMS256.txt.asc.
func (s *signer) armoredDetachedSign(t *testing.T, msg []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, s.entity, bytes.NewReader(msg), nil); err != nil {
		t.Fatalf("ArmoredDetachSign: %v", err)
	}
	return buf.Bytes()
}

// newEntity creates a fresh in-memory PGP entity for test signing.
func newEntity(t *testing.T) *openpgp.Entity {
	t.Helper()
	ent, err := openpgp.NewEntity("nodedist test", "", "test@example.com", nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return ent
}

// matchedSigner returns a signer whose public key IS in the returned
// keyring, modelling "Node signs a release with a key we trust".
func matchedSigner(t *testing.T) (*signer, keyring) {
	t.Helper()
	ent := newEntity(t)
	return &signer{entity: ent}, keyring{ent}
}

// mismatchedSigner returns a signer whose public key is NOT in the returned
// keyring (the keyring holds a different, unrelated key), modelling a
// checksums file signed by an untrusted key.
func mismatchedSigner(t *testing.T) (*signer, keyring) {
	t.Helper()
	signerEnt := newEntity(t)
	trustedEnt := newEntity(t)
	return &signer{entity: signerEnt}, keyring{trustedEnt}
}

// newTestSigner returns a fresh entity plus a keyring containing only its
// public half, for the verify-package unit tests.
func newTestSigner(t *testing.T) (*openpgp.Entity, openpgp.EntityList) {
	t.Helper()
	ent := newEntity(t)
	return ent, openpgp.EntityList{ent}
}

// armoredDetachedSign signs msg with ent (free-function form used by the
// verify unit tests).
func armoredDetachedSign(t *testing.T, ent *openpgp.Entity, msg []byte) []byte {
	t.Helper()
	return (&signer{entity: ent}).armoredDetachedSign(t, msg)
}
