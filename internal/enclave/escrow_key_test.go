package enclave

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKey_GeneratesNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "escrow.key")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(key))
	}

	// File should exist with 0600 permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected 0600 permissions, got %o", perm)
	}
}

func TestLoadOrCreateKey_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "escrow.key")

	// Create key first time.
	key1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Load same key second time.
	key2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if string(key1) != string(key2) {
		t.Fatal("expected same key on second load")
	}
}

func TestLoadOrCreateKey_RejectsWrongSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "escrow.key")

	// Write a key with wrong size.
	if err := os.WriteFile(path, []byte("too-short"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadOrCreateKey(path)
	if err == nil {
		t.Fatal("expected error for wrong-size key")
	}
}

func TestLoadOrCreateKey_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "escrow.key")

	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(key))
	}
}

func TestLoadOrCreateKey_ReadError(t *testing.T) {
	// Path is a directory, not a file — os.ReadFile will return an error
	// that is not os.ErrNotExist.
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir")
	os.Mkdir(path, 0700)

	_, err := LoadOrCreateKey(path)
	if err == nil {
		t.Fatal("expected error when path is a directory")
	}
}

func TestLoadOrCreateKey_WriteError(t *testing.T) {
	// Parent directory is read-only — WriteFile will fail.
	dir := t.TempDir()
	readOnlyDir := filepath.Join(dir, "readonly")
	os.Mkdir(readOnlyDir, 0500)
	path := filepath.Join(readOnlyDir, "escrow.key")

	_, err := LoadOrCreateKey(path)
	if err == nil {
		t.Fatal("expected error when directory is read-only")
	}
}

func TestLoadOrCreateKey_MkdirError(t *testing.T) {
	// Parent path exists as a file, not a directory — MkdirAll will fail.
	dir := t.TempDir()
	blockingFile := filepath.Join(dir, "blocker")
	os.WriteFile(blockingFile, []byte("x"), 0600)
	path := filepath.Join(blockingFile, "nested", "escrow.key")

	_, err := LoadOrCreateKey(path)
	if err == nil {
		t.Fatal("expected error when parent is a file")
	}
}
