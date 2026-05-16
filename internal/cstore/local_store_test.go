package cstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLocalManifest(name string) *Manifest {
	fqn, _ := LocalFQN(name)
	return &Manifest{
		Connector: ManifestConnector{
			Name:      fqn,
			Version:   "0.0.1",
			Origin:    OriginLocal,
			Forwarder: BuiltinForwarderSpawn,
		},
		Capabilities: ManifestCapabilities{
			Spawn: &ManifestSpawn{
				Programs: []ManifestSpawnProgram{
					{Path: "/usr/local/bin/" + name},
				},
				Operations: map[string]ManifestSpawnOperation{
					"help": {Argv: "--help"},
				},
			},
		},
	}
}

func TestLocalStore_SaveLoadRoundtrip(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	m := newTestLocalManifest("linear")
	dir, err := s.Save("linear", m)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if dir == "" {
		t.Fatal("Save returned empty dir")
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.toml")); err != nil {
		t.Fatalf("manifest.toml not written: %v", err)
	}

	loaded, err := s.Load("linear")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Connector.Origin != OriginLocal {
		t.Errorf("origin not preserved: got %q", loaded.Connector.Origin)
	}
	if loaded.Connector.Name != m.Connector.Name {
		t.Errorf("name not preserved: got %q want %q", loaded.Connector.Name, m.Connector.Name)
	}
}

func TestLocalStore_SaveRejectsHubManifest(t *testing.T) {
	// A manifest with Origin == "" (hub-published) must not be
	// accepted into the local store; mixing the two shapes is
	// exactly the bug we want this fence to prevent.
	s := NewLocalStore(t.TempDir())
	m := newTestLocalManifest("linear")
	m.Connector.Origin = ""
	if _, err := s.Save("linear", m); err == nil {
		t.Fatal("expected error for non-local origin, got nil")
	}
}

func TestLocalStore_SaveRejectsInvalidName(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	m := newTestLocalManifest("linear")
	for _, bad := range []string{"", "Linear", "../escape", ".hidden", "name with spaces", strings.Repeat("a", 64)} {
		if _, err := s.Save(bad, m); err == nil {
			t.Errorf("Save accepted invalid name %q", bad)
		}
	}
}

func TestLocalStore_LoadMissingReturnsErrNotExist(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	_, err := s.Load("absent")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want wrapped fs.ErrNotExist", err)
	}
}

func TestLocalStore_List_EmptyRootReturnsNil(t *testing.T) {
	s := NewLocalStore(filepath.Join(t.TempDir(), "does-not-exist"))
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List on missing root should not error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected zero entries, got %d", len(entries))
	}
}

func TestLocalStore_List_SortsByName(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	for _, name := range []string{"zulu", "alpha", "mike"} {
		if _, err := s.Save(name, newTestLocalManifest(name)); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	gotOrder := make([]string, 0, len(entries))
	for _, e := range entries {
		gotOrder = append(gotOrder, e.Name)
	}
	wantOrder := []string{"alpha", "mike", "zulu"}
	for i, w := range wantOrder {
		if i >= len(gotOrder) || gotOrder[i] != w {
			t.Errorf("order mismatch: got %v, want %v", gotOrder, wantOrder)
			break
		}
	}
}

func TestLocalStore_List_SurfacesPerEntryLoadError(t *testing.T) {
	// One corrupt manifest must not prevent the others from
	// listing — the daemon's startup loader needs the
	// per-entry view to skip-and-log rather than abort.
	root := t.TempDir()
	s := NewLocalStore(root)
	if _, err := s.Save("good", newTestLocalManifest("good")); err != nil {
		t.Fatalf("save good: %v", err)
	}
	badDir := filepath.Join(root, "broken")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatalf("mkdir broken: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "manifest.toml"), []byte("not valid toml ===="), 0o644); err != nil {
		t.Fatalf("write broken manifest: %v", err)
	}

	entries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sawGood, sawBroken bool
	for _, e := range entries {
		switch e.Name {
		case "good":
			sawGood = true
			if e.LoadErr != nil {
				t.Errorf("good entry should load: %v", e.LoadErr)
			}
			if e.Manifest == nil {
				t.Errorf("good entry should have manifest")
			}
		case "broken":
			sawBroken = true
			if e.LoadErr == nil {
				t.Errorf("broken entry should have LoadErr")
			}
			if e.Manifest != nil {
				t.Errorf("broken entry should have nil manifest")
			}
		}
	}
	if !sawGood || !sawBroken {
		t.Errorf("missing entries: good=%v broken=%v", sawGood, sawBroken)
	}
}

func TestLocalStore_Remove_IdempotentOnMissing(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	if err := s.Remove("never-existed"); err != nil {
		t.Errorf("Remove of missing entry should be nil, got %v", err)
	}
}

func TestLocalStore_Remove_DeletesEntry(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	if _, err := s.Save("gone", newTestLocalManifest("gone")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.Remove("gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	has, err := s.Has("gone")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if has {
		t.Error("Has returned true after Remove")
	}
}

func TestLocalStore_Save_AtomicOverwrite(t *testing.T) {
	// Save twice with different manifests; the second must
	// replace the first cleanly with no stale temp files left
	// in the entry dir.
	root := t.TempDir()
	s := NewLocalStore(root)
	m1 := newTestLocalManifest("dual")
	m1.Connector.Version = "0.0.1"
	if _, err := s.Save("dual", m1); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	m2 := newTestLocalManifest("dual")
	m2.Connector.Version = "0.0.2"
	if _, err := s.Save("dual", m2); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	loaded, err := s.Load("dual")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Connector.Version != "0.0.2" {
		t.Errorf("overwrite did not take: got version %q", loaded.Connector.Version)
	}

	entries, _ := os.ReadDir(filepath.Join(root, "dual"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("stale temp file left behind: %s", e.Name())
		}
	}
}

func TestLocalFQN_Roundtrip(t *testing.T) {
	for _, name := range []string{"linear", "sentry", "coingecko"} {
		fqn, err := LocalFQN(name)
		if err != nil {
			t.Errorf("LocalFQN(%q): %v", name, err)
			continue
		}
		got, err := LocalNameForFQN(fqn)
		if err != nil {
			t.Errorf("LocalNameForFQN(%q): %v", fqn, err)
			continue
		}
		if got != name {
			t.Errorf("roundtrip mismatch: %q → %q → %q", name, fqn, got)
		}
	}
}

func TestNewLocalStore_RoundtripsRootArg(t *testing.T) {
	// Trivial smoke-test so the constructor's Root() accessor
	// is exercised at the cstore package level (the cmd/aileron
	// tests can call it too, but go test attributes coverage to
	// the package the test lives in).
	root := t.TempDir()
	s := NewLocalStore(root)
	if got := s.Root(); got != root {
		t.Errorf("Root()=%q want %q", got, root)
	}
}

func TestDefaultLocalRoot_IncludesAileronSubdir(t *testing.T) {
	got := DefaultLocalRoot()
	if !strings.Contains(got, ".aileron") {
		t.Errorf("DefaultLocalRoot()=%q should contain .aileron", got)
	}
	if !strings.Contains(got, "connectors") {
		t.Errorf("DefaultLocalRoot()=%q should contain connectors", got)
	}
	if !strings.Contains(got, "local") {
		t.Errorf("DefaultLocalRoot()=%q should contain local", got)
	}
}

func TestLocalConnectorBinDir_IsUnderLocalRoot(t *testing.T) {
	// #780 contract: the sandbox-bin path is per-connector, lives
	// under DefaultLocalRoot, and is *not* on $PATH. The agent's
	// shell tool can't find binaries here, which closes the BYOCLI
	// credential-sealing bypass.
	got := LocalConnectorBinDir("linear")
	if !strings.HasPrefix(got, DefaultLocalRoot()) {
		t.Errorf("LocalConnectorBinDir(%q)=%q should be under DefaultLocalRoot %q", "linear", got, DefaultLocalRoot())
	}
	if !strings.HasSuffix(got, "linear/bin") && !strings.HasSuffix(got, "linear\\bin") {
		t.Errorf("LocalConnectorBinDir(%q)=%q should end with linear/bin", "linear", got)
	}
}

func TestLocalFQN_RejectsInvalidName(t *testing.T) {
	for _, bad := range []string{"", "Bad-Caps", "with space", "../escape", ".hidden"} {
		if _, err := LocalFQN(bad); err == nil {
			t.Errorf("LocalFQN(%q) accepted invalid name", bad)
		}
	}
}

func TestLocalNameForFQN_RejectsWrongScheme(t *testing.T) {
	for _, fqn := range []string{
		"github://acme/linear",
		"hub://aileron/linear",
		"local://other/linear",     // wrong owner
		"local://user/Linear",      // invalid leaf shape
		"local://user/linear/extra", // subpath not allowed
	} {
		if _, err := LocalNameForFQN(fqn); err == nil {
			t.Errorf("expected rejection for %q", fqn)
		}
	}
}
