package action

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox/forwarder"
)

// loadConnectorBytes is the single switch point that decides whether a
// connector's bytes come from the embedded forwarder or from disk.
// Regressions here would silently break every shared-forwarder wrap,
// so the two paths are exercised directly here.

func TestLoadConnectorBytes_ForwarderUsesEmbeddedWASM(t *testing.T) {
	manifest := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:      "github://acme/gitcrawl",
			Version:   "0.0.1",
			Forwarder: cstore.BuiltinForwarderSpawn,
		},
	}
	// entryDir is intentionally non-existent: the load path must not
	// consult the filesystem for forwarder connectors.
	got, err := loadConnectorBytes(manifest, "/does/not/exist")
	if err != nil {
		t.Fatalf("loadConnectorBytes: %v", err)
	}
	if !bytes.Equal(got, forwarder.WASM) {
		t.Errorf("forwarder path returned %d bytes; want forwarder.WASM (%d bytes)",
			len(got), len(forwarder.WASM))
	}
}

func TestLoadConnectorBytes_PerBinaryReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	want := []byte("PER-BINARY-WASM")
	if err := os.WriteFile(filepath.Join(dir, "connector.wasm"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:    "github://acme/slack-http",
			Version: "1.0.0",
			// No Forwarder — per-binary connector.
		},
	}
	got, err := loadConnectorBytes(manifest, dir)
	if err != nil {
		t.Fatalf("loadConnectorBytes: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("per-binary read = %q, want %q", got, want)
	}
}

func TestLoadConnectorBytes_PerBinaryFallsBackToConnectorWAT(t *testing.T) {
	// The existing per-binary path tries connector.wasm first, then
	// falls back to connector.wat (text format used by older test
	// fixtures). Confirm the fallback path is intact for non-forwarder
	// manifests so this PR doesn't accidentally regress it.
	dir := t.TempDir()
	want := []byte("(module)")
	if err := os.WriteFile(filepath.Join(dir, "connector.wat"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:    "github://acme/x",
			Version: "0.1.0",
		},
	}
	got, err := loadConnectorBytes(manifest, dir)
	if err != nil {
		t.Fatalf("loadConnectorBytes: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("wat fallback = %q, want %q", got, want)
	}
}

func TestLoadConnectorBytes_ForwarderEmptyWASMReturnsClearError(t *testing.T) {
	// Empty forwarder.WASM is what a daemon built without
	// `task build:forwarder` ships. The load path should surface a
	// targeted error rather than handing a zero-length slice to
	// Wazero, which would produce an opaque "invalid magic number"
	// failure from somewhere deep in the compiler.
	orig := forwarder.WASM
	forwarder.WASM = nil
	t.Cleanup(func() { forwarder.WASM = orig })

	manifest := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:      "github://acme/gitcrawl",
			Version:   "0.0.1",
			Forwarder: cstore.BuiltinForwarderSpawn,
		},
	}
	_, err := loadConnectorBytes(manifest, "/does/not/exist")
	if err == nil {
		t.Fatal("expected error when forwarder WASM is empty")
	}
	if !strings.Contains(err.Error(), "task build:") {
		t.Errorf("error should mention the remediation; got %v", err)
	}
}

func TestLoadConnectorBytes_PerBinaryMissingFileSurfacesError(t *testing.T) {
	manifest := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:    "github://acme/x",
			Version: "0.1.0",
		},
	}
	_, err := loadConnectorBytes(manifest, t.TempDir())
	if err == nil {
		t.Fatal("expected error when neither connector.wasm nor connector.wat exists")
	}
}
