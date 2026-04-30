package action

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestStore_Load_EmptyDirReturnsNoActions(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if s.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", s.Dir(), dir)
	}
	res, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.HasErrors() {
		t.Errorf("expected no load errors, got %v", res.Errors)
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("List() = %v, want empty", got)
	}
}

func TestStore_Load_NonexistentDirIsNotAnError(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "does-not-exist"))
	res, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for missing dir", err)
	}
	if res.HasErrors() {
		t.Errorf("expected no load errors, got %v", res.Errors)
	}
}

func TestStore_Load_ParsesValidActionAndExposesIt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ship-update.md", validActionFile)

	s := NewStore(dir)
	res, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.HasErrors() {
		t.Fatalf("unexpected load errors: %v", res.Errors)
	}
	all := s.List()
	if len(all) != 1 {
		t.Fatalf("got %d actions, want 1", len(all))
	}
	if all[0].Manifest.Name != "ship-update" {
		t.Errorf("Name = %q", all[0].Manifest.Name)
	}
	got, err := s.Get("ship-update")
	if err != nil {
		t.Fatalf("Get(ship-update) error = %v", err)
	}
	if got.Path == "" {
		t.Errorf("Path is empty")
	}
}

func TestStore_Get_UnknownReturnsErrNotFound(t *testing.T) {
	s := NewStore(t.TempDir())
	_, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_, err = s.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) err = %v, want ErrNotFound", err)
	}
}

func TestStore_Load_CollectsPerFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "good.md", validActionFile)
	writeFile(t, dir, "bad-toml.md", `+++
this is = = bad toml
+++
`)
	writeFile(t, dir, "missing-required.md", `+++
name = "x"
+++
`)
	// Non-md files are ignored.
	writeFile(t, dir, "README.txt", "ignored")

	s := NewStore(dir)
	res, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !res.HasErrors() {
		t.Fatal("expected load errors")
	}
	if len(res.Errors) != 2 {
		t.Errorf("got %d errors, want 2: %v", len(res.Errors), res.Errors)
	}
	classes := map[FailureClass]int{}
	for _, e := range res.Errors {
		classes[e.Class]++
	}
	if classes[ClassParseError] != 1 || classes[ClassValidationError] != 1 {
		t.Errorf("class counts = %v, want 1 parse_error and 1 validation_error", classes)
	}

	// The good action still loads despite the others' errors.
	if _, err := s.Get("ship-update"); err != nil {
		t.Errorf("Get(ship-update) after partial load err = %v", err)
	}
}

func TestStore_Load_DuplicateNameSurfacesError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", validActionFile)
	writeFile(t, dir, "b.md", validActionFile) // same name inside

	s := NewStore(dir)
	res, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !res.HasErrors() {
		t.Fatal("expected duplicate-name error")
	}
	found := false
	for _, e := range res.Errors {
		if e.Class == ClassValidationError && strings.Contains(e.Message, "duplicate action name") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected duplicate-name error, got %v", res.Errors)
	}
	// Only the first parses into the store; the duplicate is rejected.
	if got := s.List(); len(got) != 1 {
		t.Errorf("List() = %d items, want 1", len(got))
	}
}

func TestStore_Load_ReplacesPreviousIndex(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "ship-update.md", validActionFile)

	s := NewStore(dir)
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := s.Get("ship-update"); err != nil {
		t.Fatalf("first Get err = %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := s.Load(); err != nil {
		t.Fatalf("second Load() err = %v", err)
	}
	if _, err := s.Get("ship-update"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after reload Get err = %v, want ErrNotFound", err)
	}
}

func TestDefaultDir_HasActionsSuffix(t *testing.T) {
	got := DefaultDir()
	if filepath.Base(got) != "actions" {
		t.Errorf("DefaultDir() = %q, want path ending in /actions", got)
	}
	if filepath.Base(filepath.Dir(got)) != ".aileron" {
		t.Errorf("DefaultDir() = %q, want parent .aileron", got)
	}
}

