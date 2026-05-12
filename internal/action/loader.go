package action

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store is a thread-safe in-memory index of installed actions, keyed by
// `Manifest.Name`.
//
// The store is the runtime's view of `~/.aileron/actions/` after a successful
// `Load`. Subsequent reloads replace the index atomically.
type Store struct {
	mu      sync.RWMutex
	dir     string
	actions map[string]LoadedAction
}

// LoadedAction pairs a parsed manifest with the absolute path of its source
// file on disk.
type LoadedAction struct {
	Manifest *Manifest
	Path     string
}

// NewStore constructs an empty Store rooted at dir. Use Load to populate it.
func NewStore(dir string) *Store {
	return &Store{
		dir:     dir,
		actions: map[string]LoadedAction{},
	}
}

// Dir returns the directory the store is rooted at.
func (s *Store) Dir() string {
	return s.dir
}

// Load walks the store's directory and parses every `*.md` file it finds as
// an action manifest. Per-file errors are aggregated into a LoadResult so
// callers can surface every failure in a single pass rather than only the
// first.
//
// A non-existent directory is not an error — it simply yields an empty
// store. The action surface is optional; users with no installed actions
// should not see startup failures.
func (s *Store) Load() (*LoadResult, error) {
	res := &LoadResult{}
	loaded := map[string]LoadedAction{}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.replace(loaded)
			return res, nil
		}
		return nil, err
	}

	// Stable iteration order so error reports and listings are deterministic.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	nameToPath := map[string]string{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			res.Errors = append(res.Errors, &Error{
				Class:    ClassParseError,
				Boundary: BoundaryAction,
				File:     path,
				Message:  "read failed: " + readErr.Error(),
			})
			continue
		}
		manifest, parseErr := Parse(path, data)
		if parseErr != nil {
			var aerr *Error
			if errors.As(parseErr, &aerr) {
				res.Errors = append(res.Errors, aerr)
			} else {
				res.Errors = append(res.Errors, newParseErr(path, 0, "%s", parseErr.Error()))
			}
			continue
		}
		if vErr := Validate(manifest, path); vErr != nil {
			var aerr *Error
			if errors.As(vErr, &aerr) {
				res.Errors = append(res.Errors, aerr)
			} else {
				res.Errors = append(res.Errors, newValidationErr(path, "%s", vErr.Error()))
			}
			continue
		}
		if existing, dup := nameToPath[manifest.Name]; dup {
			res.Errors = append(res.Errors, newValidationErr(path,
				"duplicate action name %q also declared in %s", manifest.Name, existing))
			continue
		}
		nameToPath[manifest.Name] = path
		loaded[manifest.Name] = LoadedAction{Manifest: manifest, Path: path}
	}

	// Cross-manifest validation pass for [approval.preview] (ADR-0016).
	// Manifests whose preview directive references an op that is not
	// declared idempotent in the bundle, or that is itself approval-
	// gated, are reported as errors but the offending action is left
	// loaded — the runtime resolves the preview at call time and falls
	// back to "Preview unavailable" rather than gating the entire
	// action behind a bundle-load failure. Callers (the daemon's
	// startup) surface the errors via the existing LoadResult.Errors
	// surface.
	for _, e := range validatePreviewBundle(loaded) {
		res.Errors = append(res.Errors, e)
	}

	s.replace(loaded)
	return res, nil
}

// Get returns the loaded action for the given name. A miss returns
// ErrNotFound.
func (s *Store) Get(name string) (LoadedAction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.actions[name]
	if !ok {
		return LoadedAction{}, ErrNotFound
	}
	return a, nil
}

// List returns all loaded actions, sorted by name.
func (s *Store) List() []LoadedAction {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LoadedAction, 0, len(s.actions))
	for _, a := range s.actions {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out
}

func (s *Store) replace(loaded map[string]LoadedAction) {
	s.mu.Lock()
	s.actions = loaded
	s.mu.Unlock()
}

// LoadResult aggregates per-file failures from a Load call. A LoadResult
// with a non-empty Errors slice still represents a successful Load — the
// store contains the actions that did parse and validate. Callers decide
// whether to log, surface, or fail closed on the errors.
type LoadResult struct {
	// Errors holds one *Error per failed action file. Files that loaded
	// successfully produce no entry.
	Errors []*Error
}

// HasErrors reports whether any per-file failures were recorded.
func (r *LoadResult) HasErrors() bool {
	return r != nil && len(r.Errors) > 0
}

// DefaultDir returns the user-level actions directory `~/.aileron/actions`.
// Falls back to `./.aileron/actions` if the user's home directory is not
// resolvable, matching the existing `internal/launch` behavior.
func DefaultDir() string {
	const subdir = "actions"
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".aileron", subdir)
	}
	return filepath.Join(".aileron", subdir)
}
