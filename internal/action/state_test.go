package action

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func ptrBool(b bool) *bool { return &b }

func TestState_IsEnabled_DefaultsTrueWhenUnset(t *testing.T) {
	if !(State{}).IsEnabled() {
		t.Errorf("zero State.IsEnabled() = false, want true (default)")
	}
}

func TestState_IsEnabled_RespectsExplicitValues(t *testing.T) {
	if !(State{Enabled: ptrBool(true)}).IsEnabled() {
		t.Errorf("State{Enabled: true}.IsEnabled() = false, want true")
	}
	if (State{Enabled: ptrBool(false)}).IsEnabled() {
		t.Errorf("State{Enabled: false}.IsEnabled() = true, want false")
	}
}

func TestFileStateStore_GetReturnsDefaultForUnknownAction(t *testing.T) {
	s, err := NewFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	got := s.Get("never-installed")
	if !got.IsEnabled() {
		t.Errorf("Get(unknown).IsEnabled() = false, want true")
	}
}

func TestFileStateStore_SetPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	if err := s.Set("ship-update", State{Enabled: ptrBool(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	reopened, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	got := reopened.Get("ship-update")
	if got.IsEnabled() {
		t.Errorf("after Set(enabled=false) and reopen, IsEnabled() = true, want false")
	}
}

func TestFileStateStore_AllReturnsSnapshotIndependentOfStore(t *testing.T) {
	s, err := NewFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	if err := s.Set("a", State{Enabled: ptrBool(true)}); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if err := s.Set("b", State{Enabled: ptrBool(false)}); err != nil {
		t.Fatalf("Set b: %v", err)
	}

	snap := s.All()
	if len(snap) != 2 {
		t.Fatalf("All() len = %d, want 2", len(snap))
	}
	// Mutating the snapshot must not bleed back into the store.
	delete(snap, "a")
	if _, ok := s.All()["a"]; !ok {
		t.Errorf("mutating All()'s map removed entry from underlying store")
	}
}

func TestFileStateStore_NonexistentFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "state.json")
	s, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	if got := s.All(); len(got) != 0 {
		t.Errorf("All() = %v, want empty", got)
	}
}

func TestFileStateStore_RejectsMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := NewFileStateStore(path); err == nil {
		t.Errorf("NewFileStateStore over malformed file: err = nil, want non-nil")
	}
}

func TestFileStateStore_WritesValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	if err := s.Set("ship-update", State{Enabled: ptrBool(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var parsed map[string]State
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal on-disk file: %v", err)
	}
	if parsed["ship-update"].IsEnabled() {
		t.Errorf("parsed Enabled = true, want false")
	}
}

func TestFileStateStore_ConcurrentSetsAllPersist(t *testing.T) {
	s, err := NewFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	var wg sync.WaitGroup
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if err := s.Set(name, State{Enabled: ptrBool(false)}); err != nil {
				t.Errorf("Set %q: %v", name, err)
			}
		}(n)
	}
	wg.Wait()
	for _, n := range names {
		if s.Get(n).IsEnabled() {
			t.Errorf("after concurrent Sets, Get(%q).IsEnabled() = true, want false", n)
		}
	}
}

func TestFileStateStore_SetReturnsErrorWhenDirCannotBeCreated(t *testing.T) {
	// Open the store at a path under an existing directory, then turn
	// that directory into a file before calling Set. flushLocked's
	// MkdirAll returns ENOTDIR because the path no longer references a
	// directory. Exercises the error-bubbling path that surfaces disk
	// failures rather than silently dropping the write.
	root := t.TempDir()
	subdir := filepath.Join(root, "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	path := filepath.Join(subdir, "state.json")
	s, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	// Replace subdir with a regular file so MkdirAll(subdir) fails.
	if err := os.RemoveAll(subdir); err != nil {
		t.Fatalf("rm subdir: %v", err)
	}
	if err := os.WriteFile(subdir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if err := s.Set("x", State{Enabled: ptrBool(false)}); err == nil {
		t.Errorf("Set: err = nil, want non-nil when parent is not a directory")
	}
}

func TestFileStateStore_EmptyFileTreatedAsEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	if got := s.All(); len(got) != 0 {
		t.Errorf("All() = %v, want empty for zero-byte file", got)
	}
}

func TestDefaultStatePath_UsesHomeAileronDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir on this host")
	}
	want := filepath.Join(home, ".aileron", "action-state.json")
	if got := DefaultStatePath(); got != want {
		t.Errorf("DefaultStatePath() = %q, want %q", got, want)
	}
}
