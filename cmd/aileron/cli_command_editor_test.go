package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

func TestEditManifestInEditor_RejectsMissingEditor(t *testing.T) {
	t.Setenv("EDITOR", "")
	m := newEditorTestManifest()
	_, err := editManifestInEditor(m)
	if err == nil {
		t.Fatal("expected error when EDITOR unset")
	}
}

func TestEditManifestInEditor_RoundtripsThroughEditorStub(t *testing.T) {
	// The stub editor doesn't modify the file — just exits 0.
	// The function still parses the (unchanged) file back, so
	// a no-op edit returns an equivalent manifest. Verifies
	// the encode → invoke-editor → re-parse pipeline works
	// without depending on a real interactive editor.
	if runtime.GOOS == "windows" {
		t.Skip("editor-stub uses /bin/sh; Windows port pending")
	}
	stub := writeEditorStub(t, "")
	t.Setenv("EDITOR", stub)

	m := newEditorTestManifest()
	out, err := editManifestInEditor(m)
	if err != nil {
		t.Fatalf("editManifestInEditor: %v", err)
	}
	if out.Connector.Name != m.Connector.Name {
		t.Errorf("connector.name not preserved: got %q want %q", out.Connector.Name, m.Connector.Name)
	}
	if out.Connector.Origin != cstore.OriginLocal {
		t.Errorf("origin not preserved: got %q", out.Connector.Origin)
	}
}

func TestEditManifestInEditor_SurfacesEditorFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("editor-stub uses /bin/sh; Windows port pending")
	}
	stub := writeEditorStub(t, "exit 7")
	t.Setenv("EDITOR", stub)

	_, err := editManifestInEditor(newEditorTestManifest())
	if err == nil {
		t.Fatal("expected error when EDITOR exits non-zero")
	}
}

func TestEditManifestInEditor_RejectsCorruptedFile(t *testing.T) {
	// Stub editor that rewrites the temp file with invalid
	// TOML; the parse-back step must surface the error so
	// users see exactly what went wrong with their edits.
	if runtime.GOOS == "windows" {
		t.Skip("editor-stub uses /bin/sh; Windows port pending")
	}
	stub := writeEditorStub(t, "printf 'not valid toml ===' > \"$1\"")
	t.Setenv("EDITOR", stub)

	_, err := editManifestInEditor(newEditorTestManifest())
	if err == nil {
		t.Fatal("expected parse error after corrupting edit")
	}
}

// writeEditorStub creates a POSIX shell script the test sets as
// $EDITOR. The script accepts the file path as $1; the body
// parameter is shell that runs in addition to (or instead of)
// the default no-op.
func writeEditorStub(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-editor")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func newEditorTestManifest() *cstore.Manifest {
	fqn, _ := cstore.LocalFQN("editme")
	return &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:      fqn,
			Version:   "0.0.1",
			Origin:    cstore.OriginLocal,
			Forwarder: cstore.BuiltinForwarderSpawn,
		},
		Capabilities: cstore.ManifestCapabilities{
			Spawn: &cstore.ManifestSpawn{
				Programs: []cstore.ManifestSpawnProgram{
					{Path: "/usr/local/bin/editme"},
				},
				Operations: map[string]cstore.ManifestSpawnOperation{
					"help": {Argv: "--help"},
				},
			},
		},
	}
}

func TestLocalStoreDefaultLocalRoot_ReturnsAbsolutePath(t *testing.T) {
	// Smoke-test the helper used by every cli command path so
	// the trivial 0%-coverage symbol becomes 100%.
	got := cstore.DefaultLocalRoot()
	if !filepath.IsAbs(got) && !filepath.HasPrefix(got, ".aileron") {
		t.Errorf("DefaultLocalRoot returned %q which is neither absolute nor a .aileron-prefixed fallback", got)
	}
}

func TestLocalStoreRoot_RoundtripsConstructorArgument(t *testing.T) {
	root := t.TempDir()
	s := cstore.NewLocalStore(root)
	if s.Root() != root {
		t.Errorf("Root() = %q, want %q", s.Root(), root)
	}
}
