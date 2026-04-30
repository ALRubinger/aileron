package vault

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ALRubinger/aileron/internal/crypto"
)

// State reports whether a vault file exists and is in a usable shape
// without committing to a passphrase.
type State int

const (
	// StateMissing — no file at the given path; the caller is on the
	// first-run path and must initialise the vault.
	StateMissing State = iota

	// StateLegacy — file exists but predates ADR-0011 (no
	// verification blob). The caller can still open it with the
	// legacy heuristic, but should encourage migration.
	StateLegacy

	// StateReady — file exists with a verification blob; ready for
	// passphrase-validated unlock.
	StateReady
)

// CheckState reports the [State] of a vault file at path without
// touching the file's contents beyond the bytes needed to discover
// the verification blob.
func CheckState(path string) (State, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return StateMissing, nil
		}
		return 0, err
	}
	fv, err := NewFileVault(path)
	if err != nil {
		return 0, err
	}
	if len(fv.Verification()) == 0 {
		return StateLegacy, nil
	}
	return StateReady, nil
}

// ErrPassphraseRejected is returned when a supplied passphrase does
// not derive the KEK that the verification blob was encrypted with.
// Callers prompt for a retry on this error.
var ErrPassphraseRejected = errors.New("vault: passphrase rejected")

// ErrVaultTampered is returned when the verification blob decrypts
// successfully but contains an unexpected plaintext, indicating the
// file was modified outside of Aileron. Callers refuse to start.
var ErrVaultTampered = errors.New("vault: tampered (verification blob mismatch)")

// ErrVaultExists is returned by [Init] when the target path already
// has a vault file. The caller should fall through to [Unlock].
var ErrVaultExists = errors.New("vault: already exists")

// ErrVaultMissing is returned by [Unlock] when the vault file
// doesn't exist yet. The caller should fall through to [Init].
var ErrVaultMissing = errors.New("vault: not initialised")

// Init creates a new vault at path, derives a KEK from the
// passphrase + a freshly-generated salt, encrypts the well-known
// [VerificationPlaintext] under that KEK, and writes the file. The
// returned [Vault] is the EncryptedVault wrapping the new FileVault
// — ready to accept Put/Get calls.
//
// Returns [ErrVaultExists] if a non-legacy file already exists at
// path.
func Init(path, passphrase string) (Vault, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("vault: passphrase cannot be empty")
	}
	state, err := CheckState(path)
	if err != nil {
		return nil, err
	}
	if state == StateReady {
		return nil, ErrVaultExists
	}

	fv, err := NewFileVault(path)
	if err != nil {
		return nil, err
	}
	salt, err := fv.Salt()
	if err != nil {
		return nil, err
	}
	kek, err := crypto.DeriveKEK([]byte(passphrase), salt)
	if err != nil {
		return nil, fmt.Errorf("derive KEK: %w", err)
	}
	verification, err := crypto.Encrypt([]byte(VerificationPlaintext), kek)
	if err != nil {
		return nil, fmt.Errorf("encrypt verification blob: %w", err)
	}
	if err := fv.SetVerification(verification); err != nil {
		return nil, err
	}
	ev, err := NewEncryptedVault(fv, kek)
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// Unlock opens an existing vault at path. The supplied passphrase is
// run through Argon2id with the file's salt; the resulting KEK
// decrypts the verification blob. On success, the returned [Vault]
// is the EncryptedVault wrapping the loaded FileVault.
//
//   - [ErrVaultMissing] if the file doesn't exist.
//   - [ErrPassphraseRejected] if the verification blob decryption
//     fails (wrong passphrase, or AEAD tag mismatch).
//   - [ErrVaultTampered] if decryption succeeds but the plaintext
//     is not the expected constant.
//
// Legacy vaults (created before ADR-0011 added the verification
// blob) are accepted with a fallback heuristic — the call succeeds
// if at least one stored secret decrypts cleanly under the derived
// KEK, OR if the vault is empty (in which case any passphrase is
// accepted; the next [Init]-equivalent write will populate the
// verification blob).
func Unlock(path, passphrase string) (Vault, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("vault: passphrase cannot be empty")
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrVaultMissing
		}
		return nil, err
	}
	fv, err := NewFileVault(path)
	if err != nil {
		return nil, err
	}
	salt, err := fv.Salt()
	if err != nil {
		return nil, err
	}
	kek, err := crypto.DeriveKEK([]byte(passphrase), salt)
	if err != nil {
		return nil, fmt.Errorf("derive KEK: %w", err)
	}

	if blob := fv.Verification(); len(blob) > 0 {
		plaintext, err := crypto.Decrypt(blob, kek)
		if err != nil {
			return nil, ErrPassphraseRejected
		}
		if string(plaintext) != VerificationPlaintext {
			return nil, ErrVaultTampered
		}
	} else {
		// Legacy fallback. If at least one secret exists, attempt to
		// decrypt it as a heuristic check; success means the
		// passphrase is correct. If no secrets exist, accept any
		// passphrase — the next write will install a verification
		// blob.
		ev, err := NewEncryptedVault(fv, kek)
		if err != nil {
			return nil, err
		}
		if names := fv.Names(); len(names) > 0 {
			if _, err := ev.Get(context.Background(), names[0]); err != nil {
				return nil, ErrPassphraseRejected
			}
		}
		return ev, nil
	}

	ev, err := NewEncryptedVault(fv, kek)
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// OpenOrCreate is a convenience that runs [Init] when the vault is
// missing and [Unlock] when it already exists. Returns the active
// vault and a boolean reporting whether this call performed the
// initial creation. Useful for first-launch flows where the same
// passphrase is offered for both paths.
func OpenOrCreate(path, passphrase string) (Vault, bool, error) {
	state, err := CheckState(path)
	if err != nil {
		return nil, false, err
	}
	switch state {
	case StateMissing:
		v, err := Init(path, passphrase)
		return v, err == nil, err
	default:
		v, err := Unlock(path, passphrase)
		return v, false, err
	}
}
