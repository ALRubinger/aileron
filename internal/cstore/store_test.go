package cstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStore_DefaultRoot_HasAileronStoreSuffix(t *testing.T) {
	got := DefaultRoot()
	if filepath.Base(got) != "store" {
		t.Errorf("DefaultRoot() = %q, want path ending in /store", got)
	}
	if filepath.Base(filepath.Dir(got)) != ".aileron" {
		t.Errorf("DefaultRoot() = %q, want parent .aileron", got)
	}
}

func TestStore_EntryDir_LayoutMatchesADR(t *testing.T) {
	// ADR-0004 §"Content-addressed store layout":
	//   <root>/connectors/sha256/<hash>/
	root := t.TempDir()
	s := NewStore(root)
	dir, err := s.EntryDir("sha256:abcdef")
	if err != nil {
		t.Fatalf("EntryDir: %v", err)
	}
	want := filepath.Join(root, "connectors", "sha256", "abcdef")
	if dir != want {
		t.Errorf("EntryDir = %q, want %q", dir, want)
	}
}

func TestStore_EntryDir_RejectsNonSha256(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.EntryDir("md5:abc"); err == nil {
		t.Fatal("EntryDir accepted non-sha256 algo; want error")
	}
	if _, err := s.EntryDir("not-a-hash"); err == nil {
		t.Fatal("EntryDir accepted malformed hash; want error")
	}
}

func TestStore_HasHash_ReportsExistence(t *testing.T) {
	s := NewStore(t.TempDir())
	exists, err := s.HasHash("sha256:deadbeef")
	if err != nil {
		t.Fatalf("HasHash: %v", err)
	}
	if exists {
		t.Errorf("HasHash on empty store = true")
	}
	dir, _ := s.EntryDir("sha256:deadbeef")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	exists, err = s.HasHash("sha256:deadbeef")
	if err != nil {
		t.Fatalf("HasHash: %v", err)
	}
	if !exists {
		t.Errorf("HasHash on populated entry = false")
	}
}

func TestStore_Commit_WritesAtomicallyAndUpdatesIndex(t *testing.T) {
	s := NewStore(t.TempDir())
	manifest := []byte(canonicalManifestFor("github://aileron/slack", "1.2.0"))
	tb := &Tarball{
		BinaryName: tarBinaryWASM,
		Binary:     []byte("BIN"),
		Manifest:   manifest,
		Signature:  []byte("SIG"),
	}
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	hashHex := tb.CanonicalHashHex()
	if err := s.commit(tb, ref, hashHex); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Files landed in the expected tree.
	dir := filepath.Join(s.sha256Root(), hashHex)
	for _, name := range []string{tarBinaryWASM, tarManifestFile, tarSignatureFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	// Index recorded the canonical compact form.
	got, ok := s.Lookup(ref)
	if !ok || got != "sha256:"+hashHex {
		t.Errorf("Lookup = (%q, %v), want sha256:%s", got, ok, hashHex)
	}

	// Index file was written and is well-formed JSON.
	data, err := os.ReadFile(s.indexFile())
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var on map[string]string
	if err := json.Unmarshal(data, &on); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	if on["github://aileron/slack@1.2.0"] != "sha256:"+hashHex {
		t.Errorf("index file = %v", on)
	}
}

func TestStore_Commit_ConcurrentInstallsConverge(t *testing.T) {
	// ADR-0004: "Concurrent installs of the same hash are safe." Two
	// commits with identical bytes (hence identical hash) must converge —
	// both succeed, the entry exists exactly once, the index records one
	// mapping.
	s := NewStore(t.TempDir())
	manifest := []byte(canonicalManifestFor("github://aileron/slack", "1.2.0"))
	tb := &Tarball{
		BinaryName: tarBinaryWASM,
		Binary:     []byte("BIN"),
		Manifest:   manifest,
		Signature:  []byte("SIG"),
	}
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	hashHex := tb.CanonicalHashHex()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.commit(tb, ref, hashHex)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent commit err = %v", err)
		}
	}

	// Exactly one entry exists.
	dirs, err := os.ReadDir(s.sha256Root())
	if err != nil {
		t.Fatalf("read sha256 root: %v", err)
	}
	if len(dirs) != 1 {
		t.Errorf("sha256/ has %d entries, want 1", len(dirs))
	}

	// No leftover temp directories from the racers (defer RemoveAll cleans).
	if entries, err := os.ReadDir(s.tmpRoot()); err == nil {
		if len(entries) != 0 {
			t.Errorf("tmp/ has %d leftover entries: %v", len(entries), entries)
		}
	}
}

func TestStore_RebuildIndex_FromOnDiskManifests(t *testing.T) {
	// ADR-0004: "The index files are caches, never sources of truth."
	// Wiping the index file and calling RebuildIndex must restore the
	// FQN+version → hash mapping by reading manifests on disk.
	s := NewStore(t.TempDir())
	manifest := []byte(canonicalManifestFor("github://aileron/slack", "1.2.0"))
	tb := &Tarball{
		BinaryName: tarBinaryWASM,
		Binary:     []byte("BIN"),
		Manifest:   manifest,
		Signature:  []byte("SIG"),
	}
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	hashHex := tb.CanonicalHashHex()
	if err := s.commit(tb, ref, hashHex); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Wipe the index entirely.
	if err := os.RemoveAll(s.indexRoot()); err != nil {
		t.Fatalf("wipe index: %v", err)
	}
	s.index = map[string]string{}

	if err := s.RebuildIndex(); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	got, ok := s.Lookup(ref)
	if !ok || got != "sha256:"+hashHex {
		t.Errorf("Lookup after rebuild = (%q, %v); want sha256:%s", got, ok, hashHex)
	}

	// Index file was re-persisted.
	if _, err := os.Stat(s.indexFile()); err != nil {
		t.Errorf("index file missing after rebuild: %v", err)
	}
}

func TestStore_LoadIndex_HandlesMissingFile(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.LoadIndex(); err != nil {
		t.Errorf("LoadIndex on empty store err = %v; want nil (cache miss is not an error)", err)
	}
}

func TestStore_LoadIndex_RoundtripsViaPersist(t *testing.T) {
	s := NewStore(t.TempDir())
	s.index["github://x/y@1.0.0"] = "sha256:abc"
	if err := s.persistIndex(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	s2 := NewStore(s.root)
	if err := s2.LoadIndex(); err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	got, ok := s2.Lookup(Ref{FQN: FQN{Raw: "github://x/y", Scheme: "github", Owner: "x", Repo: "y"}, Version: "1.0.0"})
	if !ok || got != "sha256:abc" {
		t.Errorf("Lookup after roundtrip = (%q, %v)", got, ok)
	}
}
