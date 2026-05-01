package vault

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/internal/crypto"
)

// EncryptedLabel is the metadata label key that indicates a secret's value
// is encrypted with a user's KEK. Consumers must check this label before
// attempting to use the raw value.
const EncryptedLabel = "encrypted"

// UserScopedVault is a decorator that applies per-user encryption to vault
// operations. Both Put and Get require a KEK — plaintext storage and
// retrieval are not supported. All vault secrets are encrypted.
//
// Secrets are tagged with the metadata label "encrypted": "true".
type UserScopedVault struct {
	inner Vault
	kek   []byte // required — nil causes both Get and Put to fail
}

// NewUserScopedVault wraps a vault with optional per-user encryption.
// If kek is nil, all operations pass through unchanged.
func NewUserScopedVault(inner Vault, kek []byte) *UserScopedVault {
	return &UserScopedVault{inner: inner, kek: kek}
}

// Get retrieves and decrypts a secret. Requires a KEK — all vault secrets
// are encrypted and plaintext storage is not supported.
func (v *UserScopedVault) Get(ctx context.Context, path string) (Secret, error) {
	if v.kek == nil {
		return Secret{}, fmt.Errorf("vault: cannot read %q — no KEK available", path)
	}

	secret, err := v.inner.Get(ctx, path)
	if err != nil {
		return Secret{}, err
	}

	plaintext, err := crypto.Decrypt(secret.Value, v.kek)
	if err != nil {
		return Secret{}, fmt.Errorf("vault: decrypting secret at %q: %w", path, err)
	}

	secret.Value = plaintext
	return secret, nil
}

// Put stores a secret encrypted with the user's KEK. Refuses to store
// plaintext — returns an error if no KEK is available.
func (v *UserScopedVault) Put(ctx context.Context, path string, value []byte, meta Metadata) error {
	if v.kek == nil {
		return fmt.Errorf("vault: refusing to store plaintext at %q — no KEK provided", path)
	}

	ciphertext, err := crypto.Encrypt(value, v.kek)
	if err != nil {
		return fmt.Errorf("vault: encrypting secret for %q: %w", path, err)
	}

	if meta.Labels == nil {
		meta.Labels = make(map[string]string)
	}
	meta.Labels[EncryptedLabel] = "true"

	return v.inner.Put(ctx, path, ciphertext, meta)
}

// Delete removes a secret from the inner vault.
func (v *UserScopedVault) Delete(ctx context.Context, path string) error {
	return v.inner.Delete(ctx, path)
}

// List delegates to the inner vault. No KEK is required because
// metadata is plaintext.
func (v *UserScopedVault) List(ctx context.Context) ([]Entry, error) {
	return v.inner.List(ctx)
}

// IsEncrypted reports whether a secret's metadata indicates it was encrypted
// with a user KEK.
func IsEncrypted(meta Metadata) bool {
	return isEncrypted(meta)
}

func isEncrypted(meta Metadata) bool {
	return meta.Labels != nil && meta.Labels[EncryptedLabel] == "true"
}
