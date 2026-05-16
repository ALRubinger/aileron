package cstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// LocalStore is the on-disk home for BYOCLI connectors authored
// at install time via `aileron cli add` (issue #749). Distinct
// from the content-addressed hub [Store]: local connectors are
// not signed by a publisher, are not shareable across machines,
// and their on-disk identity is the connector's leaf name rather
// than a content hash.
//
// Layout:
//
//	<root>/
//	  <name>/
//	    manifest.toml
//
// where `<name>` is the leaf segment of the connector FQN (e.g.
// `linear` for `local://linear`). One manifest per local
// connector; no version directories, since refresh rewrites the
// existing manifest in place.
//
// The daemon's startup connector load reads both LocalStore and
// the hub [Store]; the runtime treats their manifests identically
// for spawn/audit/policy purposes. The two stores are kept
// separate on disk so a `cli add` that picked the same FQN leaf
// as a hub-installed connector doesn't corrupt the content-
// addressed tree.
type LocalStore struct {
	root string
	mu   sync.Mutex
}

// NewLocalStore constructs a LocalStore rooted at root. The
// directory is not created until the first Save call.
func NewLocalStore(root string) *LocalStore {
	return &LocalStore{root: root}
}

// Root returns the on-disk root directory.
func (s *LocalStore) Root() string { return s.root }

// DefaultLocalRoot returns the user-level local connector root
// `~/.aileron/connectors/local`, mirroring [DefaultRoot]'s home-
// dir fallback so the store works in environments where
// `os.UserHomeDir` doesn't resolve.
func DefaultLocalRoot() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".aileron", "connectors", "local")
	}
	return filepath.Join(".aileron", "connectors", "local")
}

// LocalConnectorBinDir returns the sandboxed install directory for
// a local connector's wrapped binaries — `<root>/<name>/bin`. The
// `pp add` installer overrides `GOBIN` to this path so the wrapped
// CLI never lands on the user's `$PATH`, closing the bypass that
// let an agent's `Bash` tool reach the binary directly and skip
// vault-mediated credential injection (#780). Aileron's spawn
// primitive resolves the binary path from the manifest's
// `programs[].path` field, so the binary doesn't need `$PATH`
// reachability for the runtime itself.
func LocalConnectorBinDir(name string) string {
	return filepath.Join(DefaultLocalRoot(), name, "bin")
}

// localManifestFile is the single manifest filename a local
// connector directory holds. Matches the hub store's
// [tarManifestFile] so manifest parsers see the same byte layout
// in both stores.
const localManifestFile = "manifest.toml"

// Save writes `m` to <root>/<name>/manifest.toml after
// validation. The manifest's [ManifestConnector.Origin] must
// equal [OriginLocal] — the store refuses to mix hub-shaped
// manifests into the local tree. Returns the on-disk directory
// for the saved entry.
//
// Save is overwrite-safe: an existing manifest at the same name
// is replaced atomically (write to temp, rename), so a partial
// write never leaves a half-formed manifest behind for the
// daemon to load.
func (s *LocalStore) Save(name string, m *Manifest) (string, error) {
	if err := validateLocalName(name); err != nil {
		return "", err
	}
	if m == nil {
		return "", errors.New("cstore: local save: manifest is nil")
	}
	if m.Connector.Origin != OriginLocal {
		return "", fmt.Errorf("cstore: local save: manifest origin %q must be %q", m.Connector.Origin, OriginLocal)
	}
	if err := ValidateManifest(m, localManifestFile); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return "", fmt.Errorf("cstore: encode local manifest: %w", err)
	}

	target := filepath.Join(dir, localManifestFile)
	tmp, err := os.CreateTemp(dir, "manifest-*.toml.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(buf.String()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return dir, nil
}

// Load reads and parses the manifest stored under `name`. Returns
// [fs.ErrNotExist] (wrapped) when no entry exists; surfaces
// validation errors verbatim so callers can show them to the
// user (the daemon's startup loader logs and skips an invalid
// manifest rather than aborting startup).
func (s *LocalStore) Load(name string) (*Manifest, error) {
	if err := validateLocalName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(s.root, name, localManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(path, data)
	if err != nil {
		return nil, err
	}
	if err := ValidateManifest(m, path); err != nil {
		return nil, err
	}
	if m.Connector.Origin != OriginLocal {
		return nil, fmt.Errorf("cstore: local load %s: manifest origin %q must be %q", path, m.Connector.Origin, OriginLocal)
	}
	return m, nil
}

// LocalEntry pairs a local connector's on-disk name with its
// parsed manifest. Returned by [LocalStore.List] so the daemon's
// startup loader and `aileron cli list` can iterate without
// re-reading every file from disk.
type LocalEntry struct {
	// Name is the on-disk directory name under the local root.
	// Stable across refresh; safe to use as a key.
	Name string

	// Manifest is the parsed manifest for the entry. Nil when
	// the manifest failed to parse — the caller decides whether
	// to surface the LoadErr.
	Manifest *Manifest

	// LoadErr carries the parse/validation error when Manifest
	// is nil. Allows the daemon to log a skipped entry without
	// aborting startup on one bad local connector.
	LoadErr error
}

// List enumerates every entry under the local root. Returns an
// empty slice when the root doesn't exist yet. Errors loading
// individual manifests land in [LocalEntry.LoadErr] rather than
// failing the whole call — the daemon needs a manifest-per-error
// view so one corrupted local connector doesn't take down the
// rest at startup.
//
// Output is sorted by Name for deterministic rendering in
// `aileron cli list` and stable daemon load order.
func (s *LocalStore) List() ([]LocalEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	out := make([]LocalEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if err := validateLocalName(name); err != nil {
			// Skip directories that aren't shaped like local
			// connector entries. The validation rule is
			// intentionally conservative; anything that doesn't
			// match is foreign to this store.
			continue
		}
		path := filepath.Join(s.root, name, localManifestFile)
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			out = append(out, LocalEntry{Name: name, LoadErr: rErr})
			continue
		}
		m, pErr := ParseManifest(path, data)
		if pErr != nil {
			out = append(out, LocalEntry{Name: name, LoadErr: pErr})
			continue
		}
		if vErr := ValidateManifest(m, path); vErr != nil {
			out = append(out, LocalEntry{Name: name, LoadErr: vErr})
			continue
		}
		if m.Connector.Origin != OriginLocal {
			out = append(out, LocalEntry{
				Name:    name,
				LoadErr: fmt.Errorf("manifest origin %q must be %q", m.Connector.Origin, OriginLocal),
			})
			continue
		}
		out = append(out, LocalEntry{Name: name, Manifest: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remove deletes the local connector entry under `name`. Idempotent:
// a missing entry returns nil. Removes the on-disk directory and
// its manifest.toml together so the daemon's next List doesn't see
// a stale name with no manifest.
func (s *LocalStore) Remove(name string) error {
	if err := validateLocalName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.root, name)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return os.RemoveAll(dir)
}

// Has reports whether an entry exists for `name`. Cheap probe
// for `aileron cli refresh`'s pre-flight check before
// re-introspecting.
func (s *LocalStore) Has(name string) (bool, error) {
	if err := validateLocalName(name); err != nil {
		return false, err
	}
	path := filepath.Join(s.root, name, localManifestFile)
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// localNameRe matches the on-disk name shape: lower-case ASCII,
// digits, dash, underscore, dot. Conservative on purpose —
// rejects shell-special characters and Unicode quirks that would
// confuse argv quoting or directory traversal. Names beginning
// with `.` are also rejected to avoid colliding with hidden
// files and `.DS_Store`-style noise the daemon shouldn't try to
// load as a connector.
var localNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// validateLocalName enforces [localNameRe]. Centralized so
// every caller (Save/Load/List/Remove/Has) checks the same rule.
func validateLocalName(name string) error {
	if name == "" {
		return errors.New("cstore: local name is required")
	}
	if !localNameRe.MatchString(name) {
		return fmt.Errorf("cstore: local name %q must match [a-z0-9][a-z0-9._-]{0,62}", name)
	}
	return nil
}

// LocalFQNOwner is the synthesized owner segment every local
// connector FQN carries. The hub-store FQN shape is
// `<scheme>://<owner>/<repo>` (per ADR-0004); local connectors
// adopt the same shape with `user` as the owner so [ParseFQN]
// accepts them without special-casing, while reading clearly as
// "this user's locally-installed CLI."
const LocalFQNOwner = "user"

// LocalFQN renders the canonical local FQN for an on-disk
// connector name. Inverse of [LocalNameForFQN]. Used by
// `aileron cli add` when seeding the manifest's
// [ManifestConnector.Name].
func LocalFQN(name string) (string, error) {
	if err := validateLocalName(name); err != nil {
		return "", err
	}
	return "local://" + LocalFQNOwner + "/" + name, nil
}

// LocalNameForFQN extracts the on-disk local name from a
// connector FQN. For `local://user/linear` returns `linear`.
// Rejects FQNs whose scheme is not `local` or whose owner is
// not [LocalFQNOwner] so callers surface a misshapen FQN at
// install time rather than silently writing to the wrong slot.
func LocalNameForFQN(fqn string) (string, error) {
	parsed, err := ParseFQN(fqn)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "local" {
		return "", fmt.Errorf("cstore: FQN %q is not a local connector (scheme %q, expected local)", fqn, parsed.Scheme)
	}
	if parsed.Owner != LocalFQNOwner {
		return "", fmt.Errorf("cstore: local FQN %q must use owner %q (got %q)", fqn, LocalFQNOwner, parsed.Owner)
	}
	if parsed.Subpath != "" {
		return "", fmt.Errorf("cstore: local FQN %q must have no subpath segment", fqn)
	}
	if err := validateLocalName(parsed.Repo); err != nil {
		return "", err
	}
	return parsed.Repo, nil
}

