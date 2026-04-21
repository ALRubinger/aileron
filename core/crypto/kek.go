// Package crypto provides cryptographic primitives for the zero-knowledge vault.
//
// This package implements key derivation (Argon2id), envelope encryption
// (AES-256-GCM), and key exchange (ECDH) used to protect user secrets so that
// neither Aileron operators nor hosting providers can access them.
package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// Key derivation defaults. These can be overridden via DeriveKEKWithParams for
// testing (lower cost) or future tuning.
const (
	DefaultArgon2Time    = 3
	DefaultArgon2Memory  = 64 * 1024 // 64 MB
	DefaultArgon2Threads = 4
	KEKLen               = 32 // 256-bit KEK
	SaltLen              = 16
)

// DeriveKEK derives a 256-bit Key Encryption Key from a passphrase and salt
// using Argon2id with default parameters.
func DeriveKEK(passphrase, salt []byte) ([]byte, error) {
	return DeriveKEKWithParams(passphrase, salt, DefaultArgon2Time, DefaultArgon2Memory, DefaultArgon2Threads)
}

// DeriveKEKWithParams derives a KEK with explicit Argon2id parameters.
// Use lower values in tests to avoid slow key derivation.
func DeriveKEKWithParams(passphrase, salt []byte, time, memory uint32, threads uint8) ([]byte, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("crypto: passphrase must not be empty")
	}
	if len(salt) < SaltLen {
		return nil, fmt.Errorf("crypto: salt must be at least %d bytes", SaltLen)
	}
	kek := argon2.IDKey(passphrase, salt, time, memory, threads, KEKLen)
	return kek, nil
}

// GenerateRandomKEK returns a cryptographically random 256-bit key suitable
// for use as a Key Encryption Key. Used by local/dev mode where there is no
// user passphrase — the KEK lives only in process memory.
func GenerateRandomKEK() ([]byte, error) {
	kek := make([]byte, KEKLen)
	if _, err := rand.Read(kek); err != nil {
		return nil, fmt.Errorf("crypto: generating random KEK: %w", err)
	}
	return kek, nil
}

// GenerateSalt returns a cryptographically random salt for Argon2id key derivation.
func GenerateSalt() ([]byte, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("crypto: generating salt: %w", err)
	}
	return salt, nil
}
