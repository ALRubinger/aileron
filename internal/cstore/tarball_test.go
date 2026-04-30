package cstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestExtractTarball_AcceptsCanonicalLayout(t *testing.T) {
	binary := []byte("BIN")
	manifest := []byte("MAN")
	signature := []byte("SIG")
	data := buildTarball(t, tarBinaryWASM, binary, manifest, signature)

	got, err := ExtractTarball(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}
	if got.BinaryName != tarBinaryWASM {
		t.Errorf("BinaryName = %q", got.BinaryName)
	}
	if !bytes.Equal(got.Binary, binary) {
		t.Errorf("Binary = %q", got.Binary)
	}
	if !bytes.Equal(got.Manifest, manifest) {
		t.Errorf("Manifest = %q", got.Manifest)
	}
	if !bytes.Equal(got.Signature, signature) {
		t.Errorf("Signature = %q", got.Signature)
	}
}

func TestExtractTarball_AcceptsNativeBinary(t *testing.T) {
	data := buildTarball(t, tarBinaryNative, []byte("x"), []byte("y"), []byte("z"))
	tb, err := ExtractTarball(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}
	if tb.BinaryName != tarBinaryNative {
		t.Errorf("BinaryName = %q, want %q", tb.BinaryName, tarBinaryNative)
	}
}

func TestExtractTarball_RejectsMissingFiles(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) []byte
		want  string
	}{
		{
			"missing binary",
			func(t *testing.T) []byte {
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				tw := tar.NewWriter(gz)
				_ = tw.WriteHeader(&tar.Header{Name: tarManifestFile, Mode: 0o644, Size: 1})
				_, _ = tw.Write([]byte("x"))
				_ = tw.WriteHeader(&tar.Header{Name: tarSignatureFile, Mode: 0o644, Size: 1})
				_, _ = tw.Write([]byte("x"))
				_ = tw.Close()
				_ = gz.Close()
				return buf.Bytes()
			},
			"connector binary",
		},
		{
			"missing manifest",
			func(t *testing.T) []byte {
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				tw := tar.NewWriter(gz)
				_ = tw.WriteHeader(&tar.Header{Name: tarBinaryWASM, Mode: 0o644, Size: 1})
				_, _ = tw.Write([]byte("x"))
				_ = tw.WriteHeader(&tar.Header{Name: tarSignatureFile, Mode: 0o644, Size: 1})
				_, _ = tw.Write([]byte("x"))
				_ = tw.Close()
				_ = gz.Close()
				return buf.Bytes()
			},
			tarManifestFile,
		},
		{
			"missing signature",
			func(t *testing.T) []byte {
				return buildTarball(t, tarBinaryWASM, []byte("x"), []byte("y"), nil)[:0]
			},
			tarSignatureFile,
		},
	}
	// "missing signature" needs a tarball with no signature file at all,
	// so build it directly:
	cases[2].build = func(t *testing.T) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: tarBinaryWASM, Mode: 0o644, Size: 1})
		_, _ = tw.Write([]byte("x"))
		_ = tw.WriteHeader(&tar.Header{Name: tarManifestFile, Mode: 0o644, Size: 1})
		_, _ = tw.Write([]byte("y"))
		_ = tw.Close()
		_ = gz.Close()
		return buf.Bytes()
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.build(t)
			_, err := ExtractTarball(bytes.NewReader(data))
			if err == nil {
				t.Fatalf("ExtractTarball accepted %s; want error containing %q", tc.name, tc.want)
			}
			var aerr *Error
			if !errors.As(err, &aerr) || aerr.Class != ClassMalformedTarball {
				t.Errorf("err class = %v, want ClassMalformedTarball", aerr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q; want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestExtractTarball_RejectsExtraEntry(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{tarBinaryWASM, tarManifestFile, tarSignatureFile, "EXTRA"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1})
		_, _ = tw.Write([]byte("x"))
	}
	_ = tw.Close()
	_ = gz.Close()
	_, err := ExtractTarball(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("ExtractTarball accepted extra entry; want error")
	}
}

func TestExtractTarball_RejectsTwoBinaries(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{tarBinaryWASM, tarBinaryNative, tarManifestFile, tarSignatureFile} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1})
		_, _ = tw.Write([]byte("x"))
	}
	_ = tw.Close()
	_ = gz.Close()
	_, err := ExtractTarball(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("ExtractTarball accepted two binaries; want error")
	}
}

func TestExtractTarball_RejectsNestedPath(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "sub/connector.wasm", Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	_, err := ExtractTarball(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("ExtractTarball accepted nested path; want error")
	}
}

func TestExtractTarball_RejectsNonGzip(t *testing.T) {
	_, err := ExtractTarball(bytes.NewReader([]byte("not gzip")))
	if err == nil {
		t.Fatal("ExtractTarball accepted non-gzip data; want error")
	}
}

func TestTarball_CanonicalHashIsBinaryConcatManifestSHA256(t *testing.T) {
	// ADR-0004: the canonical hash input is `connector.* || manifest.toml`,
	// signature excluded. This test holds the formula to its specification:
	// any change to the formula must also change this test, and vice versa.
	binary := []byte("hello")
	manifest := []byte("world")
	tb := &Tarball{
		BinaryName: tarBinaryWASM,
		Binary:     binary,
		Manifest:   manifest,
		Signature:  []byte("ignored-on-purpose"),
	}

	want := sha256.Sum256(append(append([]byte{}, binary...), manifest...))
	wantHex := hex.EncodeToString(want[:])

	if got := tb.CanonicalHashHex(); got != wantHex {
		t.Errorf("CanonicalHashHex() = %q, want %q", got, wantHex)
	}
	if got, exp := tb.CanonicalHash(), "sha256:"+wantHex; got != exp {
		t.Errorf("CanonicalHash() = %q, want %q", got, exp)
	}
}

func TestTarball_CanonicalHashIsSignatureIndependent(t *testing.T) {
	// Same binary+manifest but different signature bytes must produce the
	// same hash. This is what makes signature replacement (key rotation,
	// re-signing) safe without invalidating action-file hash references.
	a := &Tarball{Binary: []byte("X"), Manifest: []byte("Y"), Signature: []byte("sig-a")}
	b := &Tarball{Binary: []byte("X"), Manifest: []byte("Y"), Signature: []byte("sig-b")}
	if a.CanonicalHash() != b.CanonicalHash() {
		t.Errorf("hashes differ across signatures: %q vs %q", a.CanonicalHash(), b.CanonicalHash())
	}
}
