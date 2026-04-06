package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const (
	nonceLen = 12 // 96-bit nonce for AES-GCM
	minKeyLen = 16 // AES-128 minimum; we use 32 (AES-256)
)

// Encrypt encrypts plaintext using AES-256-GCM with the provided key.
// A random 96-bit nonce is generated and prepended to the ciphertext.
// The returned byte slice is: nonce (12 bytes) || ciphertext || tag (16 bytes).
func Encrypt(plaintext, key []byte) ([]byte, error) {
	if len(key) < minKeyLen {
		return nil, fmt.Errorf("crypto: key must be at least %d bytes", minKeyLen)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: creating GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: generating nonce: %w", err)
	}

	// Seal appends the ciphertext to nonce, so the result is nonce || ciphertext || tag.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt decrypts ciphertext produced by Encrypt using AES-256-GCM.
// It expects the input format: nonce (12 bytes) || ciphertext || tag (16 bytes).
func Decrypt(ciphertext, key []byte) ([]byte, error) {
	if len(key) < minKeyLen {
		return nil, fmt.Errorf("crypto: key must be at least %d bytes", minKeyLen)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: creating GCM: %w", err)
	}

	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("crypto: ciphertext too short")
	}

	nonce := ciphertext[:gcm.NonceSize()]
	ct := ciphertext[gcm.NonceSize():]

	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decryption failed (wrong key or tampered data): %w", err)
	}
	return plaintext, nil
}

// WrapKey wraps a Data Encryption Key with a Key Encryption Key.
// Semantically identical to Encrypt but named for clarity in key-wrapping contexts.
func WrapKey(dek, kek []byte) ([]byte, error) {
	return Encrypt(dek, kek)
}

// UnwrapKey unwraps a Data Encryption Key using a Key Encryption Key.
// Semantically identical to Decrypt but named for clarity in key-wrapping contexts.
func UnwrapKey(wrappedDEK, kek []byte) ([]byte, error) {
	return Decrypt(wrappedDEK, kek)
}
