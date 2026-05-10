package cstore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forwarderManifestFor returns a minimal-valid forwarder-shaped
// manifest for the given FQN/version. Used across the install tests.
func forwarderManifestFor(fqn, version string) []byte {
	return []byte(`[connector]
name      = "` + fqn + `"
version   = "` + version + `"
publisher = "test"
forwarder = "builtin://spawn-forwarder"

[capabilities.spawn]

[[capabilities.spawn.programs]]
path = "/usr/bin/git"

[capabilities.spawn.operations.status]
argv = "git status"
`)
}

func TestForwarderConnectorHash_IsCanonical(t *testing.T) {
	// The hash is sha256(forwarder || manifest). Two independent
	// invocations on the same inputs produce identical output; two
	// invocations on different manifests do not.
	fw := []byte("FORWARDER")
	m1 := []byte("MANIFEST-1")
	m2 := []byte("MANIFEST-2")
	if ForwarderConnectorHash(fw, m1) != ForwarderConnectorHash(fw, m1) {
		t.Error("hash is not deterministic")
	}
	if ForwarderConnectorHash(fw, m1) == ForwarderConnectorHash(fw, m2) {
		t.Error("different manifests must produce different hashes")
	}
	sum := sha256.Sum256(append(fw, m1...))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := ForwarderConnectorHash(fw, m1); got != want {
		t.Errorf("hash = %q, want %q", got, want)
	}
}

func TestInstallForwarderManifest_WritesManifestOnlyAtComputedHash(t *testing.T) {
	store := NewStore(t.TempDir())
	ref := mustRef(t, "github://acme/gitcrawl@0.0.1")
	manifest := forwarderManifestFor(ref.FQN.String(), ref.Version)
	forwarderBytes := []byte("FORWARDER-WASM")

	res, err := store.InstallForwarderManifest(manifest, forwarderBytes, ref)
	if err != nil {
		t.Fatalf("InstallForwarderManifest: %v", err)
	}
	if res.AlreadyInstalled {
		t.Error("first install reported AlreadyInstalled")
	}
	if want := ForwarderConnectorHash(forwarderBytes, manifest); res.Hash != want {
		t.Errorf("Hash = %q, want %q", res.Hash, want)
	}
	if !filepath.IsAbs(res.EntryDir) {
		t.Errorf("EntryDir not absolute: %q", res.EntryDir)
	}

	// The manifest is on disk; no connector.wasm or signature.sig was
	// written. The store entry's identity is the manifest alone.
	if _, err := os.Stat(filepath.Join(res.EntryDir, "manifest.toml")); err != nil {
		t.Errorf("manifest.toml missing: %v", err)
	}
	for _, leftover := range []string{"connector.wasm", "connector.bin", "signature.sig"} {
		if _, err := os.Stat(filepath.Join(res.EntryDir, leftover)); err == nil {
			t.Errorf("forwarder install must not write %q", leftover)
		}
	}

	if got, ok := store.Lookup(ref); !ok || got != res.Hash {
		t.Errorf("Lookup after install = (%q, %v)", got, ok)
	}
}

func TestInstallForwarderManifest_ReinstallSameBytesIsIdempotent(t *testing.T) {
	store := NewStore(t.TempDir())
	ref := mustRef(t, "github://acme/gitcrawl@0.0.1")
	manifest := forwarderManifestFor(ref.FQN.String(), ref.Version)
	fw := []byte("FORWARDER")

	first, err := store.InstallForwarderManifest(manifest, fw, ref)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	second, err := store.InstallForwarderManifest(manifest, fw, ref)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !second.AlreadyInstalled {
		t.Error("second install should report AlreadyInstalled")
	}
	if first.Hash != second.Hash {
		t.Errorf("hashes diverged: %q vs %q", first.Hash, second.Hash)
	}
	if first.EntryDir != second.EntryDir {
		t.Errorf("entry dirs diverged: %q vs %q", first.EntryDir, second.EntryDir)
	}
}

func TestInstallForwarderManifest_DifferentForwarderProducesDifferentHash(t *testing.T) {
	// This is the practically-important invariant for daemon upgrades:
	// if the daemon ships new forwarder bytes, every wrap's hash
	// changes — manifests get re-stored under the new hash, not
	// silently confused with the old one.
	store := NewStore(t.TempDir())
	ref := mustRef(t, "github://acme/gitcrawl@0.0.1")
	manifest := forwarderManifestFor(ref.FQN.String(), ref.Version)

	v1, err := store.InstallForwarderManifest(manifest, []byte("FORWARDER-V1"), ref)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := store.InstallForwarderManifest(manifest, []byte("FORWARDER-V2"), ref)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Hash == v2.Hash {
		t.Error("different forwarder bytes must produce different store hashes")
	}
	if v1.EntryDir == v2.EntryDir {
		t.Error("different hashes must land in different entry dirs")
	}
}

func TestInstallForwarderManifest_RejectsNonForwarderManifest(t *testing.T) {
	// A manifest that does not declare the builtin forwarder should
	// not pass through this install path; the regular Installer.Install
	// pipeline (with binary fetch + signature) is the right path for
	// per-binary connectors.
	store := NewStore(t.TempDir())
	ref := mustRef(t, "github://acme/gitcrawl@0.0.1")
	manifest := []byte(canonicalManifestFor(ref.FQN.String(), ref.Version))

	_, err := store.InstallForwarderManifest(manifest, []byte("FW"), ref)
	if err == nil {
		t.Fatal("expected refusal of non-forwarder manifest")
	}
	if !strings.Contains(err.Error(), "shared forwarder") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallForwarderManifest_RejectsManifestFQNMismatch(t *testing.T) {
	store := NewStore(t.TempDir())
	ref := mustRef(t, "github://acme/gitcrawl@0.0.1")
	// Manifest claims a different FQN than the install ref.
	manifest := forwarderManifestFor("github://impostor/gitcrawl", "0.0.1")

	_, err := store.InstallForwarderManifest(manifest, []byte("FW"), ref)
	if err == nil {
		t.Fatal("expected FQN-mismatch refusal")
	}
	var sErr *Error
	if !errors_As(err, &sErr) || sErr.Class != ClassFQNMismatch {
		t.Errorf("err = %v, want ClassFQNMismatch", err)
	}
}

func TestInstallForwarderManifest_RejectsManifestVersionMismatch(t *testing.T) {
	store := NewStore(t.TempDir())
	ref := mustRef(t, "github://acme/gitcrawl@0.0.1")
	// Manifest version differs from the install ref's version.
	manifest := forwarderManifestFor(ref.FQN.String(), "9.9.9")

	_, err := store.InstallForwarderManifest(manifest, []byte("FW"), ref)
	if err == nil {
		t.Fatal("expected version-mismatch refusal")
	}
	var sErr *Error
	if !errors_As(err, &sErr) || sErr.Class != ClassFQNMismatch {
		t.Errorf("err = %v", err)
	}
}

func TestInstallForwarderManifest_RejectsMalformedTOML(t *testing.T) {
	// The wrap tool emits TOML, but users hand-edit manifests too.
	// Malformed input must surface as a parse error before any file
	// is written.
	store := NewStore(t.TempDir())
	ref := mustRef(t, "github://acme/gitcrawl@0.0.1")
	_, err := store.InstallForwarderManifest([]byte("name = ="), []byte("FW"), ref)
	if err == nil {
		t.Fatal("expected parse error for malformed TOML")
	}
	var sErr *Error
	if !errors_As(err, &sErr) || sErr.Class != ClassParseError {
		t.Errorf("err = %v, want ClassParseError", err)
	}
	// Confirm nothing was written.
	entries, _ := os.ReadDir(filepath.Join(store.root, "connectors", "sha256"))
	if len(entries) != 0 {
		t.Errorf("store has %d entries after failed install; want 0", len(entries))
	}
}

func TestInstallForwarderManifest_PropagatesValidationErrors(t *testing.T) {
	// A manifest that fails ValidateManifest (e.g. missing required
	// fields) should be rejected before any file is written.
	store := NewStore(t.TempDir())
	ref := mustRef(t, "github://acme/gitcrawl@0.0.1")
	// Missing [capabilities.spawn.operations] is a validation failure.
	bad := []byte(`[connector]
name      = "github://acme/gitcrawl"
version   = "0.0.1"
forwarder = "builtin://spawn-forwarder"

[capabilities.spawn]

[[capabilities.spawn.programs]]
path = "/usr/bin/git"
`)
	_, err := store.InstallForwarderManifest(bad, []byte("FW"), ref)
	if err == nil {
		t.Fatal("expected validation failure")
	}
}

// errors_As wraps errors.As so the file does not need its own errors
// import; tests already import errors transitively, but the helper
// keeps the call site readable.
func errors_As(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
