package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// genTestKey returns a fresh ed25519 keypair plus its PEM-encoded
// public key (the form `openssl pkey -pubout` produces).
func genTestKey(t *testing.T) (ed25519.PublicKey, []byte) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return pub, pemBytes
}

// writeKey writes data to a freshly-named temp file inside dir and
// returns the path.
func writeKey(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// withTempHome points $HOME at a fresh temp dir for the test, so the
// CLI's DefaultKeyringPath() lands inside the test's filesystem and
// the test does not pollute the user's real ~/.aileron.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// --- readPublicKey ---

func TestReadPublicKey_PEMHappyPath(t *testing.T) {
	dir := t.TempDir()
	pub, pemBytes := genTestKey(t)
	path := writeKey(t, dir, "publisher.pub", pemBytes)

	got, err := readPublicKey(path)
	if err != nil {
		t.Fatalf("readPublicKey: %v", err)
	}
	if !ed25519.PublicKey(got).Equal(pub) {
		t.Errorf("returned key does not match generated public key")
	}
}

func TestReadPublicKey_RawBase64(t *testing.T) {
	dir := t.TempDir()
	pub, _ := genTestKey(t)
	encoded := base64.StdEncoding.EncodeToString(pub)
	path := writeKey(t, dir, "publisher.b64", []byte(encoded+"\n"))

	got, err := readPublicKey(path)
	if err != nil {
		t.Fatalf("readPublicKey: %v", err)
	}
	if !ed25519.PublicKey(got).Equal(pub) {
		t.Errorf("returned key does not match generated public key")
	}
}

func TestReadPublicKey_MissingFile(t *testing.T) {
	if _, err := readPublicKey(filepath.Join(t.TempDir(), "nope.pub")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadPublicKey_GarbageContents(t *testing.T) {
	dir := t.TempDir()
	path := writeKey(t, dir, "garbage", []byte("not a key"))
	if _, err := readPublicKey(path); err == nil {
		t.Fatal("expected error for garbage file contents")
	}
}

// --- keyring trust ---

func TestKeyringTrust_AddsKeyAndPersists(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	keyPath := writeKey(t, t.TempDir(), "publisher.pub", pemBytes)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring(
		[]string{"trust", "github://example/repo", keyPath},
		stdout, stderr,
	)
	if rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Trusted publisher github://example/repo") {
		t.Errorf("expected success message; got: %s", stdout.String())
	}

	// Reload from disk and verify the key is persisted.
	keyringPath := filepath.Join(home, ".aileron", "keyring.json")
	kr, err := cstore.LoadKeyring(keyringPath)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasKey("github://example/repo", pub) {
		t.Errorf("keyring at %s does not contain the trusted key", keyringPath)
	}
}

func TestKeyringTrust_DuplicateAddIsNoOp(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	keyPath := writeKey(t, t.TempDir(), "publisher.pub", pemBytes)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://example/repo", keyPath}, stdout, stderr); rc != 0 {
		t.Fatalf("first trust: rc = %d; stderr = %s", rc, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	rc := runKeyring([]string{"trust", "github://example/repo", keyPath}, stdout, stderr)
	if rc != 0 {
		t.Fatalf("re-trust rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Already trusted") {
		t.Errorf("expected duplicate-add to print 'Already trusted'; got: %s", stdout.String())
	}
}

func TestKeyringTrust_RotationAddsAlongsideExisting(t *testing.T) {
	home := withTempHome(t)
	pub1, pem1 := genTestKey(t)
	pub2, pem2 := genTestKey(t)
	dir := t.TempDir()
	key1 := writeKey(t, dir, "k1.pub", pem1)
	key2 := writeKey(t, dir, "k2.pub", pem2)

	for _, kp := range []string{key1, key2} {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if rc := runKeyring([]string{"trust", "github://example/repo", kp}, stdout, stderr); rc != 0 {
			t.Fatalf("trust %s: rc = %d; stderr = %s", kp, rc, stderr.String())
		}
	}

	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasKey("github://example/repo", pub1) {
		t.Error("first key not preserved after second trust")
	}
	if !kr.HasKey("github://example/repo", pub2) {
		t.Error("second key not added")
	}
	if got := len(kr.Keys("github://example/repo")); got != 2 {
		t.Errorf("len(keys) = %d, want 2", got)
	}
}

func TestKeyringTrust_RejectsEmptyAuthority(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	keyPath := writeKey(t, t.TempDir(), "publisher.pub", pemBytes)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "", keyPath}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit for empty authority")
	}
}

func TestKeyringTrust_WrongArgCount(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "only-one-arg"}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit for missing pubkey-file arg")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage hint; got: %s", stderr.String())
	}
}

// --- keyring list ---

func TestKeyringList_EmptyKeyring(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No trusted publishers") {
		t.Errorf("expected empty-state message; got: %s", stdout.String())
	}
}

func TestKeyringList_PrintsTrustedAuthoritiesSorted(t *testing.T) {
	withTempHome(t)
	_, pem1 := genTestKey(t)
	_, pem2 := genTestKey(t)
	dir := t.TempDir()
	for authority, body := range map[string][]byte{
		"github://b/second": pem1,
		"github://a/first":  pem2,
	} {
		path := writeKey(t, dir, strings.ReplaceAll(authority, "/", "_")+".pub", body)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if rc := runKeyring([]string{"trust", authority, path}, stdout, stderr); rc != 0 {
			t.Fatalf("seed %q: stderr = %s", authority, stderr.String())
		}
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("list rc = %d; stderr = %s", rc, stderr.String())
	}
	out := stdout.String()
	idxA := strings.Index(out, "github://a/first")
	idxB := strings.Index(out, "github://b/second")
	if idxA < 0 || idxB < 0 {
		t.Fatalf("expected both authorities in output; got: %s", out)
	}
	if idxA > idxB {
		t.Errorf("expected sorted order (a before b); got: %s", out)
	}
}

// --- keyring revoke ---

func TestKeyringRevoke_RemovesAuthority(t *testing.T) {
	home := withTempHome(t)
	_, pemBytes := genTestKey(t)
	keyPath := writeKey(t, t.TempDir(), "publisher.pub", pemBytes)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://example/repo", keyPath}, stdout, stderr); rc != 0 {
		t.Fatalf("trust: rc = %d; stderr = %s", rc, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if rc := runKeyring([]string{"revoke", "github://example/repo"}, stdout, stderr); rc != 0 {
		t.Fatalf("revoke rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revoked publisher") {
		t.Errorf("expected revoke success message; got: %s", stdout.String())
	}

	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if len(kr.Authorities()) != 0 {
		t.Errorf("expected empty keyring after revoke; got authorities = %v", kr.Authorities())
	}
}

func TestKeyringRevoke_AbsentAuthorityIsHarmless(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke", "github://nope/nope"}, stdout, stderr); rc != 0 {
		t.Fatalf("revoke of absent authority should succeed quietly; rc = %d, stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Not trusted") {
		t.Errorf("expected 'Not trusted' message; got: %s", stdout.String())
	}
}

// --- dispatch ---

func TestRunKeyring_NoSubcommand(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring(nil, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit when no subcommand given")
	}
}

func TestRunKeyring_UnknownSubcommand(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"sniff"}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit for unknown subcommand")
	}
	if !strings.Contains(stderr.String(), "unknown keyring subcommand") {
		t.Errorf("expected 'unknown keyring subcommand' message; got: %s", stderr.String())
	}
}

// --- fingerprint helper ---

func TestFingerprint_StableAndShort(t *testing.T) {
	pub, _ := genTestKey(t)
	a := fingerprint(pub)
	b := fingerprint(pub)
	if a != b {
		t.Errorf("fingerprint not stable: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Errorf("expected sha256: prefix; got %q", a)
	}
	if got := len(a); got > 32 {
		t.Errorf("fingerprint too long for terminal display: %d chars (%q)", got, a)
	}
}
