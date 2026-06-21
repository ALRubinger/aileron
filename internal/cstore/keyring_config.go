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
	"strings"
)

// keyringFile is the JSON shape persisted at ~/.aileron/keyring.json
// (and overridden via the path passed to LoadKeyring). It maps FQN
// authority strings to one or more base64-encoded ed25519 public keys.
//
// The v2 shape is hybrid: two sibling maps segregate the two grant kinds
// ADR-0013 introduces.
//
//		{
//		  "version": 2,
//		  "owners":     { "github://acme":           ["<b64>", ...] },
//		  "publishers": { "github://acme/connector":  ["<b64>", ...] }
//		}
//
//	  - Publishers is the unchanged v1 field; its keys are per-repo
//	    authorities (`<scheme>://<owner>/<repo>`).
//	  - Owners is new in v2; its keys are owner-level authorities
//	    (`<scheme>://<owner>`, no repo segment) granting trust across every
//	    repo a publisher owns.
//
// Both maps fold into the same flat in-memory key map, so Verify is
// unchanged and an owner-level grant coexists with a per-repo grant for
// the same publisher as two independent entries. A v1 file (only
// Publishers, "version": 1) loads losslessly under the v2 loader: its
// per-repo entries land untouched and Owners is simply empty.
//
// Multiple keys per authority support publisher key rotation: callers
// install a new key alongside the old one, switch signing to the new
// key, then drop the old. The verifier accepts any registered key.
type keyringFile struct {
	Version    int                 `json:"version"`
	Owners     map[string][]string `json:"owners,omitempty"`
	Publishers map[string][]string `json:"publishers"`
}

// isOwnerAuthority reports whether s is an owner-level authority
// (`<scheme>://<owner>`, no repo segment) rather than a per-repo
// authority (`<scheme>://<owner>/<repo>`). The single classifier drives
// both the load-time "v1 must not carry owners" guard and the save-time
// owners/publishers partition, so the rule lives in exactly one place.
//
// Classification is by path segments after the `://` separator: zero
// slashes after the host means owner-level; one or more means per-repo.
// Strings without `://` (which never occur for valid authorities) are
// treated as owner-level so they are not silently dropped from a save.
func isOwnerAuthority(s string) bool {
	const sep = "://"
	idx := strings.Index(s, sep)
	if idx < 0 {
		return true
	}
	return !strings.Contains(s[idx+len(sep):], "/")
}

// LoadKeyring loads an Ed25519Keyring from a JSON file at the given
// path. Per ADR-0002's "signing keys and rotation" deferral, the file
// is the source of trust — users edit it (or a future
// `aileron keyring add` command does) to authorize a publisher.
//
// Versions 1 and 2 are accepted. A v1 file carries only per-repo grants
// in "publishers"; the v2 loader reads it losslessly (its per-repo
// entries land untouched and the owner-level set is empty). A v2 file
// additionally carries owner-level grants in "owners". Both maps fold
// into the same flat in-memory key map. This is a hybrid forward-read,
// not a deprecation shim: v1 files are not rewritten until the next
// SaveKeyring, which emits v2.
//
// Behavior:
//
//   - Missing file → empty keyring, no error. Fail-closed semantics
//     are upheld at verify time: an empty keyring rejects every
//     install with ClassSignatureFailure. Aileron defaults to no
//     trusted publishers; users opt in.
//   - Malformed JSON → ClassValidationError.
//   - Unsupported version (anything other than 1 or 2) →
//     ClassValidationError.
//   - A "version": 1 document carrying an "owners" map →
//     ClassValidationError. Owner-level grants are a v2-only feature;
//     honoring them under v1 would let a downgrade hide trust.
//   - Empty authority key in either map → ClassValidationError.
//   - Bad base64 or invalid key length in either map →
//     ClassValidationError.
//
// Each authority maps to a list of base64 public keys (raw ed25519, 32
// bytes after decode). Both base64 standard encoding (with padding) and
// URL encoding (with or without padding) are accepted; standard is
// canonical.
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
	if doc.Version != 1 && doc.Version != 2 {
		return nil, newError(ClassValidationError, BoundaryRuntime, false,
			"keyring %q: unsupported version %d (expected 1 or 2)", path, doc.Version)
	}
	if doc.Version == 1 && len(doc.Owners) > 0 {
		return nil, newError(ClassValidationError, BoundaryRuntime, false,
			"keyring %q: owner-level grants require version 2 (found %d owners under version 1)",
			path, len(doc.Owners))
	}
	kr := NewEd25519Keyring()
	if err := loadAuthorities(path, doc.Publishers, kr); err != nil {
		return nil, err
	}
	if err := loadAuthorities(path, doc.Owners, kr); err != nil {
		return nil, err
	}
	return kr, nil
}

// loadAuthorities decodes a map of authority → base64 keys into kr,
// applying the empty-authority and key-decode validation shared by the
// owners and publishers maps. Both grant kinds fold into the same flat
// key map.
func loadAuthorities(path string, authorities map[string][]string, kr *Ed25519Keyring) error {
	for authority, pubs := range authorities {
		if authority == "" {
			return newError(ClassValidationError, BoundaryRuntime, false,
				"keyring %q: empty authority key", path)
		}
		for i, pubB64 := range pubs {
			pub, err := decodeEd25519Pub(pubB64)
			if err != nil {
				return newError(ClassValidationError, BoundaryRuntime, false,
					"keyring %q: authority %q key[%d]: %s", path, authority, i, err.Error())
			}
			kr.Add(authority, pub)
		}
	}
	return nil
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

// SaveKeyring writes the keyring's current state to disk as the v2
// JSON shape. The flat in-memory key map is partitioned back into the
// "owners" and "publishers" maps by authority shape: owner-level
// authorities (`<scheme>://<owner>`) go to "owners"; per-repo
// authorities (`<scheme>://<owner>/<repo>`) go to "publishers". Both
// maps emit authorities in sorted order so successive saves produce
// stable diffs.
//
// "version" is always 2. When the keyring has no owner-level entries the
// "owners" map is empty and omitted from the JSON (omitempty), so a file
// whose content is v1-shaped stays clean while still declaring version
// 2. The file is written with mode 0600 to match the surrounding
// ~/.aileron/ files (vault, secrets); parent directories are created
// with 0700 if missing.
//
// Save is the inverse of LoadKeyring: a subsequent Load reads back an
// equivalent keyring (modulo key ordering within each authority,
// which the file format does not guarantee).
func (k *Ed25519Keyring) SaveKeyring(path string) error {
	k.mu.RLock()
	doc := keyringFile{
		Version:    2,
		Owners:     map[string][]string{},
		Publishers: map[string][]string{},
	}
	for authority, pubs := range k.keys {
		encoded := make([]string, 0, len(pubs))
		for _, pub := range pubs {
			encoded = append(encoded, base64.StdEncoding.EncodeToString(pub))
		}
		if isOwnerAuthority(authority) {
			doc.Owners[authority] = encoded
		} else {
			doc.Publishers[authority] = encoded
		}
	}
	k.mu.RUnlock()

	// omitempty fires on a nil map, not an empty non-nil one; drop the
	// owners map explicitly when there are no owner-level grants so the
	// common v1-shaped file omits the key entirely.
	if len(doc.Owners) == 0 {
		doc.Owners = nil
	}

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

// OwnerAuthorities returns the owner-level authorities
// (`<scheme>://<owner>`, no repo segment) the keyring has at least one
// key registered for, in sorted order. It filters the flat key map to
// owner-level entries, so it is the owner-grant counterpart to
// Authorities(), which returns the per-repo set. ADR-0013 install-time
// UX enumerates owner grants via this accessor without seeing per-repo
// noise.
func (k *Ed25519Keyring) OwnerAuthorities() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, 0, len(k.keys))
	for authority := range k.keys {
		if isOwnerAuthority(authority) {
			out = append(out, authority)
		}
	}
	sort.Strings(out)
	return out
}

// OwnerKeys returns a copy of the keys registered for an owner-level
// authority. Returns nil if the authority is not present. Mirrors
// Keys() for the owner-keyed call sites; derive the owner authority via
// FQN.OwnerAuthority().
func (k *Ed25519Keyring) OwnerKeys(ownerAuthority string) []ed25519.PublicKey {
	return k.Keys(ownerAuthority)
}

// RemoveOwner drops every key registered for an owner-level authority.
// Returns true when the authority was present (and its keys were
// removed), false when it was not registered. Mirrors Remove().
func (k *Ed25519Keyring) RemoveOwner(ownerAuthority string) bool {
	return k.Remove(ownerAuthority)
}

// HasOwnerKey reports whether the keyring already trusts the given
// public key for an owner-level authority. Mirrors HasKey() for the
// owner-keyed call sites; matches on exact key and exact authority.
func (k *Ed25519Keyring) HasOwnerKey(ownerAuthority string, pub ed25519.PublicKey) bool {
	return k.HasKey(ownerAuthority, pub)
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
