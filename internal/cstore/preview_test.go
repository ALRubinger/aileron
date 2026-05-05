package cstore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

// TestInstaller_Preview_ReturnsManifestWithoutCommitting is the core
// contract test: Preview runs the pipeline up through verify + parse
// but leaves the cstore untouched. After Preview, no entry exists in
// the store; calling Install separately is what commits.
func TestInstaller_Preview_ReturnsManifestWithoutCommitting(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, _, _ := happyPathInstaller(t, ref)

	preview, err := inst.Preview(context.Background(), InstallRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if preview.Manifest == nil {
		t.Fatal("Manifest is nil")
	}
	if preview.Manifest.Connector.Name != ref.FQN.String() {
		t.Errorf("Manifest.Connector.Name = %q, want %q", preview.Manifest.Connector.Name, ref.FQN.String())
	}
	if !strings.HasPrefix(preview.Hash, "sha256:") {
		t.Errorf("Hash = %q, want sha256: prefix", preview.Hash)
	}
	if !preview.SignatureVerified {
		t.Error("SignatureVerified = false, want true")
	}
	if preview.AlreadyInstalled {
		t.Error("AlreadyInstalled = true on a fresh store, want false")
	}

	// Crucial: the store must not have committed anything.
	if has, _ := inst.Store.HasHash(preview.Hash); has {
		t.Error("store has the hash after Preview; Preview must not commit")
	}
	if _, ok := inst.Store.Lookup(ref); ok {
		t.Error("store index has the ref after Preview; Preview must not commit")
	}
}

// TestInstaller_Preview_AlreadyInstalledShortCircuitsWithExpectedHash
// covers the offline-reinstall path: the operator declared the hash
// (typical when sync drives the install via an action's pinned
// `[[requires.connectors]] hash`) and the store already has that
// hash. Preview must short-circuit without re-fetching and still
// return the manifest from the on-disk entry so the consent prompt
// has something to render.
func TestInstaller_Preview_AlreadyInstalledShortCircuitsWithExpectedHash(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, expectedHash, fetcher := happyPathInstaller(t, ref)

	// First install commits the entry.
	if _, err := inst.Install(context.Background(), InstallRequest{Ref: ref}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	fetcherCallsBefore := len(fetcher.calls)

	preview, err := inst.Preview(context.Background(), InstallRequest{
		Ref:          ref,
		ExpectedHash: expectedHash,
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !preview.AlreadyInstalled {
		t.Error("AlreadyInstalled = false, want true on second Preview of same hash")
	}
	if preview.Manifest == nil {
		t.Fatal("Manifest is nil; offline-reinstall path should still return manifest")
	}
	if len(fetcher.calls) != fetcherCallsBefore {
		t.Errorf("fetcher called %d times during offline-reinstall preview; want 0 (offline path)", len(fetcher.calls)-fetcherCallsBefore)
	}
}

// TestInstaller_Preview_AlreadyInstalledWithoutExpectedHash covers
// the path where the operator didn't supply expected_hash, but the
// fetched bytes hash to something already in the store. Preview must
// still report AlreadyInstalled=true so the CLI can skip the prompt.
func TestInstaller_Preview_AlreadyInstalledWithoutExpectedHash(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, _, _ := happyPathInstaller(t, ref)

	if _, err := inst.Install(context.Background(), InstallRequest{Ref: ref}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	preview, err := inst.Preview(context.Background(), InstallRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !preview.AlreadyInstalled {
		t.Error("AlreadyInstalled = false; expected true after a real install of the same bytes")
	}
}

// TestInstaller_Preview_SignatureFailureSurfacesAsError asserts the
// ADR-0007 "signature failure is a hard fail" contract: a tarball
// signed with a key the verifier doesn't trust returns a structured
// signature_failure error, NOT a preview with SignatureVerified=false.
// The CLI's --yes flag never gets to bypass this because the failure
// happens before the prompt fires.
func TestInstaller_Preview_SignatureFailureSurfacesAsError(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")

	// Build the canonical tarball but sign it with a key the
	// verifier won't trust.
	manifest := []byte(canonicalManifestFor(ref.FQN.String(), ref.Version))
	binary := []byte("BINARY")
	_, untrustedPriv, _ := ed25519.GenerateKey(rand.Reader)
	payload := append(append([]byte{}, binary...), manifest...)
	badSig := ed25519.Sign(untrustedPriv, payload)
	tarBytes := buildTarball(t, tarBinaryWASM, binary, manifest, badSig)

	resolver := DefaultResolver()
	url, _ := resolver.ResolveTarball(ref)
	fetcher := &fakeFetcher{bytesAt: map[string][]byte{url: tarBytes}}

	// Trust a different key — the one the tarball is signed with is
	// not in this keyring.
	keyring := NewEd25519Keyring()
	trustedPub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyring.Add(ref.FQN.Authority(), trustedPub)

	inst := &Installer{
		Resolver: resolver,
		Fetcher:  fetcher,
		Verifier: keyring,
		Store:    NewStore(t.TempDir()),
	}

	_, err := inst.Preview(context.Background(), InstallRequest{Ref: ref})
	if err == nil {
		t.Fatal("expected signature failure error")
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if aerr.Class != ClassSignatureFailure {
		t.Errorf("class = %q, want %q", aerr.Class, ClassSignatureFailure)
	}
}

// TestInstaller_Preview_HashMismatchReturnsError asserts the
// expected-hash check still fires during Preview: a caller-declared
// hash that doesn't match the fetched bytes surfaces hash_mismatch.
// Without this, a malicious mirror serving different bytes would
// pass through preview undetected.
func TestInstaller_Preview_HashMismatchReturnsError(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	inst, _, _ := happyPathInstaller(t, ref)

	_, err := inst.Preview(context.Background(), InstallRequest{
		Ref:          ref,
		ExpectedHash: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("expected hash mismatch error")
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if aerr.Class != ClassHashMismatch {
		t.Errorf("class = %q, want %q", aerr.Class, ClassHashMismatch)
	}
}

// TestInstaller_Preview_FQNMismatchReturnsError asserts the manifest-
// FQN sanity check still fires: a tarball whose manifest declares a
// different FQN than the one being installed surfaces fqn_mismatch.
// Same defense-in-depth as Install proper.
func TestInstaller_Preview_FQNMismatchReturnsError(t *testing.T) {
	ref := mustRef(t, "github://aileron/slack@1.2.0")

	// Build a tarball with a manifest declaring a different FQN.
	manifest := []byte(canonicalManifestFor("github://aileron/notion", "1.2.0"))
	binary := []byte("BINARY")
	sig, pub := signedManifest(t, binary, manifest)
	tarBytes := buildTarball(t, tarBinaryWASM, binary, manifest, sig)

	resolver := DefaultResolver()
	url, _ := resolver.ResolveTarball(ref)
	keyring := NewEd25519Keyring()
	keyring.Add(ref.FQN.Authority(), pub)

	inst := &Installer{
		Resolver: resolver,
		Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{url: tarBytes}},
		Verifier: keyring,
		Store:    NewStore(t.TempDir()),
	}

	_, err := inst.Preview(context.Background(), InstallRequest{Ref: ref})
	if err == nil {
		t.Fatal("expected FQN mismatch error")
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if aerr.Class != ClassFQNMismatch {
		t.Errorf("class = %q, want %q", aerr.Class, ClassFQNMismatch)
	}
}
