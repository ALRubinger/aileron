package cstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store is the on-disk content-addressed connector store rooted at a
// configurable directory (default: `~/.aileron/store/`).
//
// Layout (per ADR-0004):
//
//	<root>/
//	  connectors/
//	    sha256/
//	      <hash>/
//	        connector.wasm   (or connector.bin)
//	        manifest.toml
//	        signature.sig
//	    tmp/
//	      <random>/          (transient; renamed atomically into sha256/<hash>/)
//	  index/
//	    connectors.json      (FQN+version → hash; cache only)
//
// Trust property: the hash *is* the address. Lookup is by hash, not by
// name. Two installs that produce the same bytes share one entry; two
// installs that disagree (e.g. publisher re-tagged) produce two distinct
// entries and the runtime selects whichever the action file declared.
type Store struct {
	root string

	mu    sync.RWMutex
	index map[string]string // canonical "<FQN>@<version>" → "sha256:<hex>"
}

// NewStore constructs a Store rooted at root. The root is not created on
// disk until the first install or call to ensureLayout.
func NewStore(root string) *Store {
	return &Store{
		root:  root,
		index: map[string]string{},
	}
}

// Root returns the on-disk root directory.
func (s *Store) Root() string { return s.root }

// connectorsRoot returns the path that holds the sha256/ tree.
func (s *Store) connectorsRoot() string { return filepath.Join(s.root, "connectors") }
func (s *Store) sha256Root() string     { return filepath.Join(s.connectorsRoot(), "sha256") }
func (s *Store) tmpRoot() string        { return filepath.Join(s.connectorsRoot(), "tmp") }
func (s *Store) indexRoot() string      { return filepath.Join(s.root, "index") }
func (s *Store) indexFile() string      { return filepath.Join(s.indexRoot(), "connectors.json") }

// EntryDir returns the directory where the entry for hash should live (or
// does live, if it's installed). Hash must be of the form `sha256:<hex>`.
func (s *Store) EntryDir(hash string) (string, error) {
	algo, hex, ok := splitHash(hash)
	if !ok {
		return "", fmt.Errorf("invalid hash %q: expected sha256:<hex>", hash)
	}
	if algo != "sha256" {
		return "", fmt.Errorf("unsupported hash algorithm %q (only sha256 is supported)", algo)
	}
	return filepath.Join(s.connectorsRoot(), algo, hex), nil
}

// HasHash reports whether an entry for hash already exists in the store.
func (s *Store) HasHash(hash string) (bool, error) {
	dir, err := s.EntryDir(hash)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(dir)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Lookup returns the hash recorded in the index for the given ref, or the
// empty string when no install has happened yet. Reads from the in-memory
// index loaded by LoadIndex; concurrent installs update the in-memory and
// on-disk index together.
func (s *Store) Lookup(ref Ref) (string, bool) {
	key := canonicalRefKey(ref)
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.index[key]
	return h, ok
}

// Count returns the number of installed (FQN, version) entries in the
// index. Each entry corresponds to one connector tarball under
// `~/.aileron/store/connectors/sha256/`. Surfaced through `GET /v1/status`
// for the operator's "what's installed" snapshot per ADR-0004.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index)
}

// LookupAnyVersion returns one installed (Ref, hash) pair for the
// given FQN, or `false` when no version of the connector is installed.
// Multiple installed versions return an unspecified one — callers that
// need a specific version should use Lookup. Used by the binding setup
// flow which is FQN-scoped, not version-scoped.
func (s *Store) LookupAnyVersion(fqn FQN) (Ref, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := fqn.String() + "@"
	for k, h := range s.index {
		if strings.HasPrefix(k, prefix) {
			version := strings.TrimPrefix(k, prefix)
			return Ref{FQN: fqn, Version: version}, h, true
		}
	}
	return Ref{}, "", false
}

// LoadIndex reads the index file from disk into memory. A missing index
// file is not an error — the in-memory index simply stays empty.
func (s *Store) LoadIndex() error {
	data, err := os.ReadFile(s.indexFile())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var loaded map[string]string
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("decode index file: %w", err)
	}
	s.mu.Lock()
	s.index = loaded
	s.mu.Unlock()
	return nil
}

// RebuildIndex walks the store's sha256/ tree and rebuilds the FQN+version
// → hash index from on-disk manifests. Per ADR-0004 the index is a cache,
// never a source of truth — this method exists so a corrupted or missing
// index file can be regenerated from the trustworthy on-disk content.
//
// Each `manifest.toml` under sha256/<hash>/ contributes one index entry
// (its declared FQN and version → that hash). Manifests that fail to parse
// are skipped (their hash entries remain valid; only the index gap is
// papered over).
func (s *Store) RebuildIndex() error {
	rebuilt := map[string]string{}
	entries, err := os.ReadDir(s.sha256Root())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.mu.Lock()
			s.index = rebuilt
			s.mu.Unlock()
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		hex := e.Name()
		manifestPath := filepath.Join(s.sha256Root(), hex, tarManifestFile)
		data, rErr := os.ReadFile(manifestPath)
		if rErr != nil {
			continue
		}
		m, pErr := ParseManifest(manifestPath, data)
		if pErr != nil {
			continue
		}
		if m.Connector.Name == "" || m.Connector.Version == "" {
			continue
		}
		fqn, fErr := ParseFQN(m.Connector.Name)
		if fErr != nil {
			continue
		}
		key := canonicalRefKey(Ref{FQN: fqn, Version: m.Connector.Version})
		rebuilt[key] = "sha256:" + hex
	}
	s.mu.Lock()
	s.index = rebuilt
	s.mu.Unlock()
	return s.persistIndex()
}

// recordIndex updates the in-memory index and rewrites the index file
// atomically (write to temp, fsync, rename).
func (s *Store) recordIndex(ref Ref, hash string) error {
	key := canonicalRefKey(ref)
	s.mu.Lock()
	s.index[key] = hash
	s.mu.Unlock()
	return s.persistIndex()
}

// persistIndex writes the current in-memory index to the index file
// atomically. The keys are sorted so output is reproducible across runs.
func (s *Store) persistIndex() error {
	if err := os.MkdirAll(s.indexRoot(), 0o755); err != nil {
		return wrapStoreErr(err)
	}

	s.mu.RLock()
	keys := make([]string, 0, len(s.index))
	for k := range s.index {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sorted := make(map[string]string, len(s.index))
	ordered := make([][2]string, 0, len(s.index))
	for _, k := range keys {
		sorted[k] = s.index[k]
		ordered = append(ordered, [2]string{k, s.index[k]})
	}
	s.mu.RUnlock()

	// JSON object key order is undefined; emit a sorted-key encoding by
	// streaming a hand-built object so external tools see deterministic
	// output.
	buf, err := encodeOrderedJSON(ordered)
	if err != nil {
		return wrapStoreErr(err)
	}

	tmp, err := os.CreateTemp(s.indexRoot(), "connectors-*.json.tmp")
	if err != nil {
		return wrapStoreErr(err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return wrapStoreErr(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return wrapStoreErr(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return wrapStoreErr(err)
	}
	if err := os.Rename(tmpName, s.indexFile()); err != nil {
		os.Remove(tmpName)
		return wrapStoreErr(err)
	}
	return nil
}

// commit writes a freshly-extracted tarball into the store at sha256/<hex>/
// using atomic-rename semantics:
//
//   1. Make a unique temp directory under tmp/.
//   2. Write the three files into it.
//   3. Rename the temp directory to sha256/<hex>/.
//
// Concurrent installs of the same hash converge naturally: rename of a
// directory onto a non-empty existing directory fails on every Unix, so
// the loser observes the entry in place and proceeds. The temp directory
// is cleaned up afterward whether or not the rename succeeded.
func (s *Store) commit(t *Tarball, ref Ref, hashHex string) error {
	if err := os.MkdirAll(s.tmpRoot(), 0o755); err != nil {
		return wrapStoreErr(err)
	}
	if err := os.MkdirAll(s.sha256Root(), 0o755); err != nil {
		return wrapStoreErr(err)
	}

	tmp, err := uniqueTempDir(s.tmpRoot())
	if err != nil {
		return wrapStoreErr(err)
	}
	defer os.RemoveAll(tmp)

	if err := t.writeTo(tmp); err != nil {
		return wrapStoreErr(err)
	}

	dst := filepath.Join(s.sha256Root(), hashHex)
	// If a parallel installer already won the race, dst exists. That's
	// fine — both produced identical bytes (same hash). We can drop our
	// temp directory and update the index.
	if _, err := os.Stat(dst); err == nil {
		return s.recordIndex(ref, "sha256:"+hashHex)
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Concurrent install may have raced and won between the Stat and
		// the Rename. Re-stat: if dst now exists, treat that as a benign
		// race; otherwise surface the rename error.
		if _, sErr := os.Stat(dst); sErr == nil {
			return s.recordIndex(ref, "sha256:"+hashHex)
		}
		return wrapStoreErr(err)
	}
	return s.recordIndex(ref, "sha256:"+hashHex)
}

// uniqueTempDir creates and returns a fresh subdirectory of parent.
func uniqueTempDir(parent string) (string, error) {
	const tries = 16
	for i := 0; i < tries; i++ {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		name := "install-" + hex.EncodeToString(b[:])
		path := filepath.Join(parent, name)
		err := os.Mkdir(path, 0o755)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate unique temp directory under %s after %d tries", parent, tries)
}

// wrapStoreErr converts a generic file/IO error into a structured store-
// unwritable error per ADR-0004's failure-modes table.
func wrapStoreErr(err error) error {
	if err == nil {
		return nil
	}
	return newError(ClassStoreUnwritable, BoundaryRuntime, false, "%s", err.Error())
}

// splitHash decomposes "sha256:<hex>" into its two halves. Returns
// (algo, hex, true) on a well-formed input; (_, _, false) otherwise.
func splitHash(h string) (algo, hexDigest string, ok bool) {
	for i := 0; i < len(h); i++ {
		if h[i] == ':' {
			return h[:i], h[i+1:], i > 0 && i < len(h)-1
		}
	}
	return "", "", false
}

// encodeOrderedJSON emits a flat string→string JSON object with keys in
// the order supplied. Avoids the map iteration nondeterminism in
// encoding/json's default behavior.
func encodeOrderedJSON(pairs [][2]string) ([]byte, error) {
	buf := []byte{'{', '\n'}
	for i, kv := range pairs {
		k, err := json.Marshal(kv[0])
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(kv[1])
		if err != nil {
			return nil, err
		}
		buf = append(buf, ' ', ' ')
		buf = append(buf, k...)
		buf = append(buf, ':', ' ')
		buf = append(buf, v...)
		if i != len(pairs)-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, '\n')
	}
	buf = append(buf, '}', '\n')
	return buf, nil
}

// DefaultRoot returns the user-level connector store root
// `~/.aileron/store`, or `./.aileron/store` when the user's home directory
// is not resolvable.
func DefaultRoot() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".aileron", "store")
	}
	return filepath.Join(".aileron", "store")
}
