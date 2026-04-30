package cstore

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// File names inside the tarball, fixed by ADR-0004.
const (
	tarManifestFile  = "manifest.toml"
	tarSignatureFile = "signature.sig"
	tarBinaryWASM    = "connector.wasm"
	tarBinaryNative  = "connector.bin"
)

// Tarball holds the raw bytes of a connector tarball after extraction:
// the binary, its manifest, and the detached signature.
type Tarball struct {
	// BinaryName is the on-disk filename of the binary as it appeared in
	// the archive (one of tarBinaryWASM or tarBinaryNative).
	BinaryName string

	// Binary is the connector binary bytes.
	Binary []byte

	// Manifest is the raw manifest.toml bytes.
	Manifest []byte

	// Signature is the detached signature bytes (signature.sig).
	Signature []byte
}

// CanonicalHashHex returns the SHA-256 hex digest of `Binary || Manifest`,
// signature excluded, per ADR-0004's canonical hash input. Callers that
// surface this value should prefix with `sha256:` to match the
// declared-hash format used in action and connector manifests.
func (t Tarball) CanonicalHashHex() string {
	h := sha256.New()
	h.Write(t.Binary)
	h.Write(t.Manifest)
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalHash returns the canonical hash with the `sha256:` prefix.
func (t Tarball) CanonicalHash() string {
	return "sha256:" + t.CanonicalHashHex()
}

// ExtractTarball reads a gzipped tar from r, validates the layout demanded
// by ADR-0004 (exactly one binary + one manifest.toml + one signature.sig),
// and returns the decoded artifact. Returns ClassMalformedTarball on any
// layout violation.
func ExtractTarball(r io.Reader) (*Tarball, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
			"gzip reader: %s", err.Error())
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	out := &Tarball{}

	for {
		hdr, hErr := tr.Next()
		if errors.Is(hErr, io.EOF) {
			break
		}
		if hErr != nil {
			return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
				"read tar entry: %s", hErr.Error())
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			// Per ADR-0004 the archive contains regular files only. Skip
			// directories silently (some tar producers prepend an entry for
			// the archive root) but reject anything else.
			if hdr.Typeflag == tar.TypeDir {
				continue
			}
			return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
				"unexpected tar entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
		// Reject path-traversal: the layout is flat (single level).
		name := filepath.Base(hdr.Name)
		if name != hdr.Name && hdr.Name != "./"+name {
			return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
				"tar entry %q is not at archive root (nested paths are not allowed)", hdr.Name)
		}
		body, rErr := io.ReadAll(tr)
		if rErr != nil {
			return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
				"read tar entry %q: %s", name, rErr.Error())
		}
		switch name {
		case tarBinaryWASM, tarBinaryNative:
			if out.Binary != nil {
				return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
					"tarball contains more than one connector binary")
			}
			out.BinaryName = name
			out.Binary = body
		case tarManifestFile:
			out.Manifest = body
		case tarSignatureFile:
			out.Signature = body
		default:
			return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
				"unexpected tar entry %q (expected one of %s, %s, %s, %s)",
				name, tarBinaryWASM, tarBinaryNative, tarManifestFile, tarSignatureFile)
		}
	}

	if out.Binary == nil {
		return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
			"tarball missing connector binary (one of %s, %s)", tarBinaryWASM, tarBinaryNative)
	}
	if out.Manifest == nil {
		return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
			"tarball missing %s", tarManifestFile)
	}
	if out.Signature == nil {
		return nil, newError(ClassMalformedTarball, BoundaryExternal, false,
			"tarball missing %s", tarSignatureFile)
	}
	return out, nil
}

// writeTarball writes the tarball's three files to dir. Used by the
// install pipeline to populate the temp directory before atomic rename.
// Files are written with mode 0o600 (binary) / 0o644 (text); the directory
// must already exist.
func (t *Tarball) writeTo(dir string) error {
	if err := writeFile(filepath.Join(dir, t.BinaryName), t.Binary, 0o600); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, tarManifestFile), t.Manifest, 0o644); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, tarSignatureFile), t.Signature, 0o644)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// canonicalRefKey is the index-file key for an FQN+version pair: the
// canonical compact form per ADR-0002 (`<FQN>@<version>`), normalized so
// the index file output is deterministic across runs.
func canonicalRefKey(ref Ref) string {
	return strings.TrimSpace(ref.String())
}
