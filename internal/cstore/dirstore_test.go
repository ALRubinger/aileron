package cstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

const (
	testHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestCommitDir_HappyPathWritesTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node")

	entryDir, already, err := cstore.CommitDir(root, testHashA, func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "bin", "node"), []byte("#!node"), 0o755)
	})
	if err != nil {
		t.Fatalf("CommitDir: %v", err)
	}
	if already {
		t.Fatalf("first commit reported alreadyPresent=true")
	}

	want := filepath.Join(root, "sha256", testHashA)
	if entryDir != want {
		t.Fatalf("entryDir = %q, want %q", entryDir, want)
	}
	got, err := os.ReadFile(filepath.Join(entryDir, "bin", "node"))
	if err != nil {
		t.Fatalf("read committed file: %v", err)
	}
	if string(got) != "#!node" {
		t.Fatalf("committed content = %q, want %q", got, "#!node")
	}
}

func TestCommitDir_DuplicateIsBenignNoOp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node")

	if _, _, err := cstore.CommitDir(root, testHashA, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "marker"), []byte("first"), 0o644)
	}); err != nil {
		t.Fatalf("first CommitDir: %v", err)
	}

	// Second commit of the same hash: the write callback's output must be
	// discarded and the original entry left untouched.
	entryDir, already, err := cstore.CommitDir(root, testHashA, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "marker"), []byte("second"), 0o644)
	})
	if err != nil {
		t.Fatalf("second CommitDir: %v", err)
	}
	if !already {
		t.Fatalf("duplicate commit reported alreadyPresent=false")
	}
	got, err := os.ReadFile(filepath.Join(entryDir, "marker"))
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("entry content = %q, want the original %q", got, "first")
	}
}

func TestCommitDir_ConcurrentSameHashConverges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node")

	const n = 8
	var wg sync.WaitGroup
	results := make([]bool, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, already, err := cstore.CommitDir(root, testHashA, func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "data"), []byte("same"), 0o644)
			})
			results[i] = already
			errs[i] = err
		}(i)
	}
	wg.Wait()

	createdCount := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if !results[i] {
			createdCount++
		}
	}
	// Exactly one committer created the entry; the rest saw it already
	// present. Crucially none errored.
	if createdCount != 1 {
		t.Fatalf("createdCount = %d, want exactly 1 (CAS race did not converge)", createdCount)
	}
}

func TestCommitDir_DistinctHashesAreSeparateEntries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node")

	for _, h := range []string{testHashA, testHashB} {
		if _, _, err := cstore.CommitDir(root, h, func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "id"), []byte(h), 0o644)
		}); err != nil {
			t.Fatalf("CommitDir(%s): %v", h, err)
		}
	}
	for _, h := range []string{testHashA, testHashB} {
		got, err := os.ReadFile(filepath.Join(root, "sha256", h, "id"))
		if err != nil {
			t.Fatalf("read entry %s: %v", h, err)
		}
		if string(got) != h {
			t.Fatalf("entry %s content = %q", h, got)
		}
	}
}

func TestCommitDir_RejectsInvalidHash(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node")
	for _, bad := range []string{"", "../escape", "ABCDEF", "abc/def", "sha256:abc"} {
		called := false
		_, _, err := cstore.CommitDir(root, bad, func(dir string) error {
			called = true
			return nil
		})
		if err == nil {
			t.Fatalf("CommitDir(%q) = nil error, want rejection", bad)
		}
		if called {
			t.Fatalf("CommitDir(%q) invoked the write callback before validating the hash", bad)
		}
	}
}

func TestCommitDir_CallbackErrorCommitsNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node")
	wantErr := os.ErrPermission
	_, _, err := cstore.CommitDir(root, testHashA, func(dir string) error {
		return wantErr
	})
	if err == nil {
		t.Fatalf("CommitDir with failing callback returned nil error")
	}
	// The callback's error is surfaced verbatim so the caller can classify
	// it in its own vocabulary.
	if !errors.Is(err, wantErr) {
		t.Fatalf("CommitDir did not return the callback error verbatim: %v", err)
	}
	present, hErr := cstore.HasDirHash(root, testHashA)
	if hErr != nil {
		t.Fatalf("HasDirHash: %v", hErr)
	}
	if present {
		t.Fatalf("entry was committed despite callback failure")
	}
}

func TestHasDirHash(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node")

	present, err := cstore.HasDirHash(root, testHashA)
	if err != nil {
		t.Fatalf("HasDirHash before commit: %v", err)
	}
	if present {
		t.Fatalf("HasDirHash = true before any commit")
	}

	if _, _, err := cstore.CommitDir(root, testHashA, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "x"), []byte("y"), 0o644)
	}); err != nil {
		t.Fatalf("CommitDir: %v", err)
	}

	present, err = cstore.HasDirHash(root, testHashA)
	if err != nil {
		t.Fatalf("HasDirHash after commit: %v", err)
	}
	if !present {
		t.Fatalf("HasDirHash = false after commit")
	}
}

func TestDirEntryDir(t *testing.T) {
	root := "/tmp/store/node"
	got, err := cstore.DirEntryDir(root, testHashA)
	if err != nil {
		t.Fatalf("DirEntryDir: %v", err)
	}
	want := filepath.Join(root, "sha256", testHashA)
	if got != want {
		t.Fatalf("DirEntryDir = %q, want %q", got, want)
	}
	if _, err := cstore.DirEntryDir(root, "../bad"); err == nil {
		t.Fatalf("DirEntryDir accepted an invalid hash")
	}
}
