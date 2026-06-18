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

	opts := DefaultLoadOptions()
	if opts.UserPath != user {
		t.Errorf("DefaultLoadOptions.UserPath = %q, want %q", opts.UserPath, user)
	}
}

// DefaultLoadOptions must load cleanly against the embedded defaults even
// when the user override file does not exist (the common daemon case).
func TestDefaultLoadOptions_LoadsBuiltinWhenLayersAbsent(t *testing.T) {
	opts := DefaultLoadOptions()
	entries, err := Load(opts)
	if err != nil {
		t.Fatalf("Load(DefaultLoadOptions): %v", err)
	}
	if _, ok := findEntry(entries, "api.linear.app"); !ok {
		t.Error("default load missing built-in Linear entry")
	}
}
