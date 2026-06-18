package proxybinding

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPaths(t *testing.T) {
	user := DefaultUserPath()
	if !strings.HasSuffix(user, filepath.Join(".aileron", "binding-descriptors.yaml")) {
		t.Errorf("DefaultUserPath = %q, want .../.aileron/binding-descriptors.yaml", user)
	}

	project := DefaultProjectPath("/work/proj")
	if project != filepath.Join("/work/proj", ".aileron", "binding-descriptors.yaml") {
		t.Errorf("DefaultProjectPath = %q", project)
	}

	opts := DefaultLoadOptions("/work/proj")
	if opts.ProjectPath != project {
		t.Errorf("DefaultLoadOptions.ProjectPath = %q, want %q", opts.ProjectPath, project)
	}
	if opts.UserPath == "" {
		t.Error("DefaultLoadOptions.UserPath is empty")
	}
}

// DefaultLoadOptions paths must load cleanly against the embedded defaults
// even when the override files do not exist (the common daemon case).
func TestDefaultLoadOptions_LoadsBuiltinWhenLayersAbsent(t *testing.T) {
	opts := DefaultLoadOptions(t.TempDir())
	entries, err := Load(opts)
	if err != nil {
		t.Fatalf("Load(DefaultLoadOptions): %v", err)
	}
	if _, ok := findEntry(entries, "api.linear.app"); !ok {
		t.Error("default load missing built-in Linear entry")
	}
}
