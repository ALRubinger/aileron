package freeze

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
)

// SigningKeyEnv is the environment variable freeze reads the signing-key
// path from when no explicit path is supplied. The value is a path to a
// PEM-encoded ed25519 private key; the key bytes themselves are never
// carried in the environment.
const SigningKeyEnv = "AILERON_SIGNING_KEY"

// LoadSigningKey loads a PEM-encoded ed25519 private key from path, or —
// when path is empty — from the file named by $AILERON_SIGNING_KEY. The
// private key is read into memory for signing only; it is never written
// into the frozen artifact or the store (only the public key and the
// detached signature are persisted).
//
// The PEM form is the standard PKCS#8 ed25519 private key
// (`openssl genpkey -algorithm ed25519`). Mirrors the PEM/ed25519
// conventions in cmd/aileron/keyring.go.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		path = os.Getenv(SigningKeyEnv)
	}
	if path == "" {
		return nil, fmt.Errorf("freeze: no signing key: pass --signing-key or set %s", SigningKeyEnv)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("freeze: read signing key %q: %w", path, err)
	}
	return ParsePEMPrivateKey(data)
}

// ParsePEMPrivateKey decodes a PEM-encoded PKCS#8 ed25519 private key.
func ParsePEMPrivateKey(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("freeze: signing key is not PEM-encoded")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("freeze: parse PKCS#8 private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("freeze: signing key is not ed25519 (got %T)", key)
	}
	return priv, nil
}

// MarshalPublicKeyPEM renders an ed25519 public key as PKIX PEM, the form
// cstore.ParsePEMPublicKey and the keyring read (so Launch verification and
// `keyring trust` consume the stored signing-key.pub directly).
func MarshalPublicKeyPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("freeze: marshal public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// Sign produces a detached ed25519 signature over the content bytes and
// returns the signature plus the PEM-encoded public key. The content bytes
// are the canonical content-hash input (the same bytes contentHash is taken
// over), so a verifier confirms the signature covers the exact frozen unit.
//
// ed25519 is deterministic for a fixed key and message, so re-signing
// byte-identical content yields a byte-identical signature.
func Sign(priv ed25519.PrivateKey, content []byte) (sig, pubPEM []byte, err error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("freeze: signing key has wrong length %d", len(priv))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("freeze: signing key has no ed25519 public half")
	}
	pubPEM, err = MarshalPublicKeyPEM(pub)
	if err != nil {
		return nil, nil, err
	}
	return ed25519.Sign(priv, content), pubPEM, nil
}

// Verify checks a detached ed25519 signature over content against a
// PEM-encoded public key. It is self-contained (signature + public key
// only) so Launch (#1511) can call it without the private key or any store
// access. Returns nil when the signature is valid, an error otherwise.
func Verify(content, sig, pubPEM []byte) error {
	pub, err := parsePublicKeyPEM(pubPEM)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, content, sig) {
		return fmt.Errorf("freeze: signature does not verify against the public key")
	}
	return nil
}

// parsePublicKeyPEM decodes a PKIX PEM ed25519 public key. Kept local so
// the freeze package does not depend on cmd/aileron; it accepts the same
// PEM form MarshalPublicKeyPEM emits.
func parsePublicKeyPEM(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(string(pemBytes))))
	if block == nil {
		return nil, fmt.Errorf("freeze: public key is not PEM-encoded")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("freeze: parse PKIX public key: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("freeze: public key is not ed25519 (got %T)", key)
	}
	return pub, nil
}
