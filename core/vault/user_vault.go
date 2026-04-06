package vault

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/crypto"
)

// EncryptedLabel is the metadata label key that indicates a secret's value
// is encrypted with a user's KEK. Consumers must check this label before
// attempting to use the raw value.
const EncryptedLabel = "encrypted"

// UserScopedVault is a decorator that applies per-user encryption to vault
// operations. When a KEK is provided, Put encrypts values before storage and
// Get decrypts them on retrieval. When KEK is nil, operations pass through
// unchanged (for users who haven't set a passphrase yet).
//
// Secrets encrypted by this vault are tagged with the metadata label
// "encrypted": "true" so that downstream consumers (e.g. RunExecution)
// can detect whether decryption is needed.
type UserScopedVault struct {
	inner Vault
	kek   []byte // nil means passthrough (no encryption)
}

// NewUserScopedVault wraps a vault with optional per-user encryption.
// If kek is nil, all operations pass through unchanged.
func NewUserScopedVault(inner Vault, kek []byte) *UserScopedVault {
	return &UserScopedVault{inner: inner, kek: kek}
}

// Get retrieves a secret. If the secret is marked as encrypted and a KEK is
// available, the value is decrypted before returning.
func (v *UserScopedVault) Get(ctx context.Context, path string) (Secret, error) {
	secret, err := v.inner.Get(ctx, path)
	if err != nil {
		return Secret{}, err
	}

	if !isEncrypted(secret.Metadata) {
		return secret, nil
	}

	if v.kek == nil {
		return Secret{}, fmt.Errorf("vault: secret at %q is encrypted but no KEK available", path)
	}

	plaintext, err := crypto.Decrypt(secret.Value, v.kek)
	if err != nil {
		return Secret{}, fmt.Errorf("vault: decrypting secret at %q: %w", path, err)
	}

	secret.Value = plaintext
	return secret, nil
}

// Put stores a secret. If a KEK is available, the value is encrypted before
// storage and the metadata is tagged with "encrypted": "true".
func (v *UserScopedVault) Put(ctx context.Context, path string, value []byte, meta Metadata) error {
	if v.kek == nil {
		return v.inner.Put(ctx, path, value, meta)
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

// IsEncrypted reports whether a secret's metadata indicates it was encrypted
// with a user KEK.
func IsEncrypted(meta Metadata) bool {
	return isEncrypted(meta)
}

func isEncrypted(meta Metadata) bool {
	return meta.Labels != nil && meta.Labels[EncryptedLabel] == "true"
}
