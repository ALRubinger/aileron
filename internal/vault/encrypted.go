package vault

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/internal/crypto"
)

// EncryptedVault is a decorator that encrypts secret values before storing
// them in the inner vault and decrypts them on retrieval. Metadata is stored
// unencrypted (it contains no sensitive data).
//
// This enables zero-knowledge storage: the inner vault (database, file system,
// cloud service) holds only ciphertext. Without the KEK, the stored values
// are meaningless.
type EncryptedVault struct {
	inner Vault
	kek   []byte
}

// NewEncryptedVault wraps an existing vault with AES-256-GCM envelope encryption.
// The kek (Key Encryption Key) must be exactly 32 bytes (256 bits).
func NewEncryptedVault(inner Vault, kek []byte) (*EncryptedVault, error) {
	if inner == nil {
		return nil, fmt.Errorf("vault: inner vault must not be nil")
	}
	if len(kek) != crypto.KEKLen {
		return nil, fmt.Errorf("vault: KEK must be %d bytes, got %d", crypto.KEKLen, len(kek))
	}
	return &EncryptedVault{inner: inner, kek: kek}, nil
}

// Get retrieves a secret from the inner vault and decrypts its value.
func (v *EncryptedVault) Get(ctx context.Context, path string) (Secret, error) {
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

// Put encrypts the value and stores it in the inner vault.
func (v *EncryptedVault) Put(ctx context.Context, path string, value []byte, meta Metadata) error {
	ciphertext, err := crypto.Encrypt(value, v.kek)
	if err != nil {
		return fmt.Errorf("vault: encrypting secret for %q: %w", path, err)
	}
	return v.inner.Put(ctx, path, ciphertext, meta)
}

// Delete removes a secret from the inner vault.
func (v *EncryptedVault) Delete(ctx context.Context, path string) error {
	return v.inner.Delete(ctx, path)
}

// List delegates to the inner vault. Metadata is stored unencrypted, so
// the encryption decorator passes the entries through unchanged. The
// caller never receives ciphertext from this path because Entry omits
// the value bytes by construction.
func (v *EncryptedVault) List(ctx context.Context) ([]Entry, error) {
	return v.inner.List(ctx)
}
