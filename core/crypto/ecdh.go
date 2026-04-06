package crypto

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

// GenerateKeyPair generates an ephemeral ECDH key pair on the P-256 curve.
func GenerateKeyPair() (*ecdh.PrivateKey, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("crypto: generating ECDH key pair: %w", err)
	}
	return priv, nil
}

// DeriveSharedSecret performs an ECDH key exchange and returns a 256-bit
// shared secret derived by hashing the raw ECDH output with SHA-256.
// The hash step ensures uniform key material suitable for use as an
// encryption key.
func DeriveSharedSecret(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	raw, err := priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("crypto: ECDH exchange: %w", err)
	}
	h := sha256.Sum256(raw)
	return h[:], nil
}

// MarshalPublicKey serializes an ECDH public key to its uncompressed byte
// representation for transmission over the wire.
func MarshalPublicKey(pub *ecdh.PublicKey) []byte {
	return pub.Bytes()
}

// UnmarshalPublicKey deserializes a P-256 ECDH public key from bytes.
func UnmarshalPublicKey(data []byte) (*ecdh.PublicKey, error) {
	pub, err := ecdh.P256().NewPublicKey(data)
	if err != nil {
		return nil, fmt.Errorf("crypto: parsing ECDH public key: %w", err)
	}
	return pub, nil
}
