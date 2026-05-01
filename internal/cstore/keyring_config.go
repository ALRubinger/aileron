package cstore

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
