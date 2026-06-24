// Package store is the host-side canonical store for installed Flight Plan
// skills. It is the single writer of `~/.aileron/skills/<name>/`, mirroring
// the actions store convention (internal/action/loader.go DefaultDir).
//
// The store is designed as the shared canonical location the freeze step
// (#1509) writes frozen skill versions into. It is not a parser-local
// artifact: an installed skill is read-only after install (editing is an
// explicit future op), and `aileron launch` bind-mounts this directory
// read-only at the agent's skills path (#1508 launch-anywhere projection).
//
// The store never reads or carries credential values (ADR-0005, ADR-0019).
// It writes skill content (SKILL.md plus any accompanying files) only.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// DefaultDir returns the user-level skills directory `~/.aileron/skills`.
// Falls back to `./.aileron/skills` if the user's home directory is not
// resolvable, matching internal/action/loader.go DefaultDir.
func DefaultDir() string {
	const subdir = "skills"
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".aileron", subdir)
	}
	return filepath.Join(".aileron", subdir)
}

// Store is a canonical skill store rooted at a directory.
type Store struct {
	root string
}

// New returns a Store rooted at dir. An empty dir uses DefaultDir.
func New(dir string) *Store {
	if dir == "" {
		dir = DefaultDir()
	}
	return &Store{root: dir}
}

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

// Dir returns the on-disk directory for a named skill,
// `<root>/<name>`. It does not create the directory.
func (s *Store) Dir(name string) string {
	return filepath.Join(s.root, name)
}

// List returns the names of installed skills in sorted order. A skill is
// any subdirectory of the store root that contains a SKILL.md file. A
// missing store root is not an error: it lists as empty.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read skills dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.root, e.Name(), "SKILL.md")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Read returns the raw SKILL.md bytes for an installed skill.
func (s *Store) Read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Dir(name), "SKILL.md"))
}
