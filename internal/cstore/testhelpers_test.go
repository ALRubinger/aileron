package cstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

// buildTarball assembles a connector tarball in-memory in the canonical
// layout demanded by ADR-0004. Tests that exercise the install pipeline
// pass the returned bytes through ExtractTarball or feed them via a fake
// Fetcher. Pass `binaryName` as tarBinaryWASM or tarBinaryNative; pass any
// signature bytes (the verifier owns signature checking, not the
// extractor).
func buildTarball(t *testing.T, binaryName string, binary, manifest, signature []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		body []byte
	}{
		{binaryName, binary},
		{tarManifestFile, manifest},
		{tarSignatureFile, signature},
	} {
		hdr := &tar.Header{
			Name:    e.name,
			Mode:    0o644,
			Size:    int64(len(e.body)),
			ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// signedManifest produces a (manifest, signature, public key) triple where
// signature is a valid ed25519 detached signature over `binary || manifest`.
// The keyring tests use this shape; the install pipeline tests use it to
// drive a real Verifier through a happy-path install.
func signedManifest(t *testing.T, binary, manifest []byte) (signature []byte, pub ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	payload := append(append([]byte{}, binary...), manifest...)
	signature = ed25519.Sign(priv, payload)
	return signature, pub
}

// canonicalManifestFor returns a minimal valid manifest TOML for the given
// FQN and version. Test fixtures use this rather than hard-coded strings
// so it's easy to vary the fields.
func canonicalManifestFor(fqn, version string) string {
	return `[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "test"
`
}
