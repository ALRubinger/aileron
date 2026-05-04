package cstore

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// keyringFile is the JSON shape persisted at ~/.aileron/keyring.json
// (and overridden via the path passed to LoadKeyring). It maps FQN
// authority strings to one or more base64-encoded ed25519 public keys.
//
// Multiple keys per authority support publisher key rotation: callers
// install a new key alongside the old one, switch signing to the new
// key, then drop the old. The verifier accepts any registered key.
type keyringFile struct {
	Version    int                 `json:"version"`
	Publishers map[string][]string `json:"publishers"`
}

// LoadKeyring loads an Ed25519Keyring from a JSON file at the given
// path. Per ADR-0002's "signing keys and rotation" deferral, the file
// is the v1 source of trust — users edit it (or a future
// `aileron keyring add` command does) to authorize a publisher.
//
// Behavior:
//
//   - Missing file → empty keyring, no error. Fail-closed semantics
//     are upheld at verify time: an empty keyring rejects every
//     install with ClassSignatureFailure. Aileron defaults to no
//     trusted publishers; users opt in.
//   - Malformed JSON → ClassValidationError.
//   - Unsupported version → ClassValidationError.
//   - Bad base64 or invalid key length → ClassValidationError.
//
// The single "Publishers" key per authority accepts a list of base64
// public keys (raw ed25519, 32 bytes after decode). Both base64
// standard encoding (with padding) and URL encoding (with or without
// padding) are accepted; standard is canonical.
func LoadKeyring(path string) (*Ed25519Keyring, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return NewEd25519Keyring(), nil
		}
		return nil, fmt.Errorf("read keyring %q: %w", path, err)
	}
	var doc keyringFile
	if err := json.Unmarshal(bytes, &doc); err != nil {
		return nil, newError(ClassValidationError, BoundaryRuntime, false,
			"keyring %q: invalid JSON: %s", path, err.Error())
	}
	if doc.Version != 1 {
		return nil, newError(ClassValidationError, BoundaryRuntime, false,
			"keyring %q: unsupported version %d (expected 1)", path, doc.Version)
	}
	kr := NewEd25519Keyring()
	for authority, pubs := range doc.Publishers {
		if authority == "" {
			return nil, newError(ClassValidationError, BoundaryRuntime, false,
				"keyring %q: empty authority key", path)
		}
		for i, pubB64 := range pubs {
			pub, err := decodeEd25519Pub(pubB64)
			if err != nil {
				return nil, newError(ClassValidationError, BoundaryRuntime, false,
					"keyring %q: authority %q key[%d]: %s", path, authority, i, err.Error())
			}
			kr.Add(authority, pub)
		}
	}
	return kr, nil
}

// decodeEd25519Pub decodes a base64-encoded ed25519 public key,
// trying standard and URL encodings (with and without padding) in
// that order. Returns an error if no decoding produces a valid 32-byte
// ed25519 public key.
func decodeEd25519Pub(s string) (ed25519.PublicKey, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		bytes, err := enc.DecodeString(s)
		if err != nil {
			continue
		}
		if len(bytes) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("decoded length %d, want %d", len(bytes), ed25519.PublicKeySize)
		}
		return ed25519.PublicKey(bytes), nil
	}
	return nil, fmt.Errorf("not valid base64")
}

// SaveKeyring writes the keyring's current state to disk as the v1
// JSON shape. Authorities are emitted in sorted order so successive
// saves produce stable diffs. The file is written with mode 0600 to
// match the surrounding ~/.aileron/ files (vault, secrets); parent
// directories are created with 0700 if missing.
//
// Save is the inverse of LoadKeyring: a subsequent Load reads back an
// equivalent keyring (modulo key ordering within each authority,
// which the file format does not guarantee).
func (k *Ed25519Keyring) SaveKeyring(path string) error {
	k.mu.RLock()
	doc := keyringFile{
		Version:    1,
		Publishers: make(map[string][]string, len(k.keys)),
	}
	for authority, pubs := range k.keys {
		encoded := make([]string, 0, len(pubs))
		for _, pub := range pubs {
			encoded = append(encoded, base64.StdEncoding.EncodeToString(pub))
		}
		doc.Publishers[authority] = encoded
	}
	k.mu.RUnlock()

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keyring: %w", err)
	}
	body = append(body, '\n')

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create keyring directory %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write keyring %q: %w", path, err)
	}
	return nil
}

// Authorities returns the authority strings the keyring has at least
// one key registered for, in sorted order.
func (k *Ed25519Keyring) Authorities() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, 0, len(k.keys))
	for authority := range k.keys {
		out = append(out, authority)
	}
	sort.Strings(out)
	return out
}

// Keys returns a copy of the keys registered for an authority. Returns
// nil if the authority is not present.
func (k *Ed25519Keyring) Keys(authority string) []ed25519.PublicKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	src := k.keys[authority]
	if src == nil {
		return nil
	}
	out := make([]ed25519.PublicKey, len(src))
	copy(out, src)
	return out
}

// Remove drops every key registered for the authority. Returns true
// when the authority was present (and its keys were removed), false
// when the authority was not registered.
func (k *Ed25519Keyring) Remove(authority string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.keys[authority]; !ok {
		return false
	}
	delete(k.keys, authority)
	return true
}

// HasKey reports whether the keyring already trusts the given public
// key for the authority. Used by callers (e.g. `aileron keyring trust`)
// to detect duplicate adds and avoid bloating the file with copies of
// the same key on repeated invocation.
func (k *Ed25519Keyring) HasKey(authority string, pub ed25519.PublicKey) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	for _, existing := range k.keys[authority] {
		if ed25519.PublicKey(existing).Equal(pub) {
			return true
		}
	}
	return false
}

// ParsePEMPublicKey decodes a PEM-encoded ed25519 public key
// (SubjectPublicKeyInfo as produced by `openssl pkey -pubout`) into
// the raw ed25519.PublicKey form Aileron's keyring stores. Returns
// an error when the PEM block is missing, the algorithm is not
// ed25519, or the encoded length is wrong.
func ParsePEMPublicKey(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	edPub, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is not ed25519 (got %T)", pub)
	}
	if len(edPub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key has wrong length %d (want %d)",
			len(edPub), ed25519.PublicKeySize)
	}
	return edPub, nil
}

// DefaultKeyringPath returns the conventional path
// `$HOME/.aileron/keyring.json`. Returns "" when the user's home
// directory cannot be determined; LoadKeyring treats that as a
// missing file (empty keyring, fail-closed at install).
func DefaultKeyringPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".aileron", "keyring.json")
}
