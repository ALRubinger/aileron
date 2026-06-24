package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultDir(t *testing.T) {
	d := DefaultDir()
	if filepath.Base(d) != "skills" {
		t.Errorf("DefaultDir = %q, want a path ending in skills", d)
	}
}

func TestNewDefaultsRootAndExposesIt(t *testing.T) {
	// An empty dir falls back to DefaultDir.
	if got := New("").Root(); got != DefaultDir() {
		t.Errorf("New(\"\").Root() = %q, want %q", got, DefaultDir())
	}
	// An explicit dir is exposed verbatim.
	if got := New("/tmp/x").Root(); got != "/tmp/x" {
		t.Errorf("Root() = %q, want /tmp/x", got)
	}
}

func TestListAndRead(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// Empty / missing store lists as empty.
	names, err := s.List()
	if err != nil {
		t.Fatalf("List on empty store: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("empty store List = %v, want empty", names)
	}

	// A directory without SKILL.md is not a skill.
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory with SKILL.md is a skill.
	skillDir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const content = "---\nname: alpha\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err = s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "alpha" {
		t.Errorf("List = %v, want [alpha]", names)
	}

	got, err := s.Read("alpha")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != content {
		t.Errorf("Read = %q", string(got))
	}
}
