package action

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// State holds per-action user preferences that live outside the manifest
// because they are user preferences rather than properties of the action
// itself. The overlay survives re-install (which may overwrite the
// manifest) and hand-edits to the manifest.
//
// New fields added here should default to the value a freshly-installed
// action would receive — readers must never need to mutate the overlay
// just to get sensible behavior for an action with no overlay entry.
type State struct {
	// Enabled controls whether the action is exposed to the LLM as a
	// callable tool. False hides the action from MCP's tools/list and
	// causes the daemon to refuse direct calls to RunAction.
	//
	// Stored via a pointer so the on-disk overlay can omit the field
	// entirely and let the default (`Enabled() == true`) apply. Callers
	// should use [State.IsEnabled] rather than reading the pointer
	// directly.
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled reports whether the action is enabled, applying the
// default-true rule when the overlay omitted the field.
func (s State) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// StateStore reads and writes per-action user preferences. Implementations
// must be safe for concurrent use by daemon goroutines (HTTP handlers,
// reloads, background workers).
type StateStore interface {
	// Get returns the overlay entry for the action. A miss returns the
	// zero value (which IsEnabled-reports true), so callers may read
	// unconditionally without a separate "exists" check.
	Get(name string) State

	// Set persists the overlay entry for the action, replacing any
	// prior value. Concurrent Sets are serialized; the final on-disk
	// view reflects the last write to land.
	Set(name string, s State) error

	// All returns a snapshot of every recorded overlay entry, keyed by
	// action name. Actions with no overlay entry are absent — callers
	// must still treat absence as "default state".
	All() map[string]State
}

// FileStateStore persists per-action preferences as a single JSON file.
// Format:
//
//	{
//	  "<action-name>": { "enabled": false },
//	  ...
//	}
//
// A missing or empty file is treated as "no overrides" — every action
// returns the default state. Writes go via a temp-file + rename so a
// crashed write can never leave a partial file on disk.
type FileStateStore struct {
	path string

	mu    sync.RWMutex
	state map[string]State
}

// NewFileStateStore opens or initializes the overlay file at path. A
// non-existent file is not an error — the store starts empty and the
// file is created on the first Set. Returns an error only when the
// file exists but cannot be read or parsed.
func NewFileStateStore(path string) (*FileStateStore, error) {
	s := &FileStateStore{path: path, state: map[string]State{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, err
	}
	return s, nil
}

// Get implements [StateStore.Get].
func (s *FileStateStore) Get(name string) State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state[name]
}

// Set implements [StateStore.Set].
func (s *FileStateStore) Set(name string, st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[name] = st
	return s.flushLocked()
}

// All implements [StateStore.All].
func (s *FileStateStore) All() map[string]State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]State, len(s.state))
	for k, v := range s.state {
		out[k] = v
	}
	return out
}

// flushLocked writes the current in-memory map to disk. Caller must hold
// the write lock. A temp file is rendered alongside the destination and
// atomically renamed so a partial write can never appear on disk.
func (s *FileStateStore) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path)
}

// DefaultStatePath returns the canonical overlay-file path
// `~/.aileron/action-state.json`. Falls back to `./.aileron/action-state.json`
// when the user's home directory is not resolvable — matching the
// fallback rule of [DefaultDir] for the actions directory itself.
func DefaultStatePath() string {
	const name = "action-state.json"
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".aileron", name)
	}
	return filepath.Join(".aileron", name)
}
