package cstore

import (
	"crypto/ed25519"
	"sync"
)

// Verifier checks the detached signature in a connector tarball against
// keys associated with the FQN's authority per ADR-0002.
//
// The Verifier interface is the seam between the install pipeline (which
// orchestrates fetch → verify → hash → store) and the signing-keys-and-
// rotation policy that ADR-0002 defers to a separate implementation note.
// The pipeline always calls Verify; what counts as a valid key is a
// Verifier-implementation concern.
//
// Verify is given the bytes that were signed (`binary || manifest`, the
// same canonical-hash input from ADR-0004) and the detached signature.
// `authority` is the FQN's `<scheme>://<owner>/<repo>` triple — Verifier
// implementations look up authorized keys keyed by this string.
type Verifier interface {
	Verify(authority string, binary, manifest, signature []byte) error
}

// Ed25519Keyring is the v1 Verifier — a thread-safe map of FQN authority
// (`<scheme>://<owner>/<repo>`) to one or more authorized ed25519 public
// keys.
//
// Verification semantics:
//
//   - The signed payload is `binary || manifest` (the canonical-hash input
//     from ADR-0004).
//   - At least one registered key for the authority must verify the
//     signature for the call to succeed.
//   - When the authority has zero registered keys, Verify fails with
//     ClassSignatureFailure ("no signing key registered"). This is the
//     "fail closed" behavior demanded by ADR-0004's failure-modes table —
//     unsigned binaries are not accepted.
//
// Key distribution and rotation are out of scope per ADR-0002 ("signing-
// keys-and-rotation will be its own implementation note rather than a
// fresh ADR"). Callers populate the keyring from whatever source they
// trust (config, hub://, sigstore — all post-MVP).
type Ed25519Keyring struct {
	mu   sync.RWMutex
	keys map[string][]ed25519.PublicKey
}

// NewEd25519Keyring returns an empty Ed25519Keyring. Use Add to register
// public keys for an FQN authority before passing the keyring into the
// install pipeline.
func NewEd25519Keyring() *Ed25519Keyring {
	return &Ed25519Keyring{keys: map[string][]ed25519.PublicKey{}}
}

// Add registers a public key as authorized for the given FQN authority.
// Multiple keys may be registered for the same authority to support key
// rotation.
func (k *Ed25519Keyring) Add(authority string, pub ed25519.PublicKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[authority] = append(k.keys[authority], pub)
}

// AddOwner registers a public key as authorized for an owner-level
// authority (`<scheme>://<owner>`, no repo segment) per ADR-0013
// per-publisher trust. An owner-level grant and a per-repo grant for the
// same publisher coexist as independent entries in the same flat key
// map; this is a thin, discoverability wrapper over Add that documents
// the owner-level contract for the owner-keyed call sites. Derive the
// owner authority via FQN.OwnerAuthority(). Multiple keys may be
// registered for the same owner to support key rotation.
func (k *Ed25519Keyring) AddOwner(ownerAuthority string, pub ed25519.PublicKey) {
	k.Add(ownerAuthority, pub)
}

// Verify implements Verifier.
func (k *Ed25519Keyring) Verify(authority string, binary, manifest, signature []byte) error {
	k.mu.RLock()
	keys := append([]ed25519.PublicKey(nil), k.keys[authority]...)
	k.mu.RUnlock()

	if len(keys) == 0 {
		return newError(ClassSignatureFailure, BoundaryRuntime, false,
			"no signing key registered for authority %q", authority)
	}

	payload := make([]byte, 0, len(binary)+len(manifest))
	payload = append(payload, binary...)
	payload = append(payload, manifest...)

	for _, key := range keys {
		if ed25519.Verify(key, payload, signature) {
			return nil
		}
	}
	return newError(ClassSignatureFailure, BoundaryRuntime, false,
		"signature does not verify against any key registered for authority %q", authority)
}

// PermissiveVerifier accepts every signature unconditionally. Intended
// **only** for tests and local development bootstrapping; never wire this
// into a production install pipeline.
type PermissiveVerifier struct{}

// Verify implements Verifier — always returns nil.
func (PermissiveVerifier) Verify(_ string, _, _, _ []byte) error { return nil }

// ReloadingKeyring is a Verifier that re-reads its keyring from Path
// on every Verify call. Use this when the daemon must observe
// out-of-band keyring writes (e.g. `aileron keyring trust ...` writes
// directly to ~/.aileron/keyring.json without notifying the daemon)
// without needing a restart.
//
// Failure modes match LoadKeyring + Ed25519Keyring.Verify:
//
//   - Path missing → empty keyring → ClassSignatureFailure ("no
//     signing key registered for authority …"). Same fail-closed
//     behavior as a freshly started daemon with no keyring file.
//   - Path malformed → ClassValidationError surfaced to the caller.
//     The install pipeline turns this into a 422 with the parse
//     error embedded, which is the right place for the user to see
//     it (vs. silently falling back to empty and reporting "no
//     signing key" for an authority they thought they had trusted).
//
// The file is small (a handful of KB even with many publishers) and
// installs are infrequent, so re-parsing on every call is negligible.
type ReloadingKeyring struct {
	Path string
}

// Verify implements Verifier by loading the keyring at Path and
// delegating to its Verify. See ReloadingKeyring docs for failure
// modes.
func (r *ReloadingKeyring) Verify(authority string, binary, manifest, signature []byte) error {
	keyring, err := LoadKeyring(r.Path)
	if err != nil {
		return err
	}
	return keyring.Verify(authority, binary, manifest, signature)
}
