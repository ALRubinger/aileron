package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
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

// withTempHome points the home directory at a fresh temp dir for the
// test, so the CLI's DefaultKeyringPath() lands inside the test's
// filesystem and the test does not pollute the user's real ~/.aileron.
func withTempHome(t *testing.T) string {
	t.Helper()
	return setTestHome(t)
}

// withMockGitHubRaw stands up an httptest server that mimics
// raw.githubusercontent.com for one or more
// `<owner>/<repo>/HEAD/keys/publisher.pub` paths and points the CLI
// at it for the duration of the test. Returns a setter the test can
// use to swap a path's body mid-test (e.g. to simulate publisher key
// rotation between two `keyring trust` calls).
func withMockGitHubRaw(t *testing.T, paths map[string][]byte) func(path string, body []byte) {
	t.Helper()

	bodies := make(map[string][]byte, len(paths))
	for k, v := range paths {
		bodies[k] = v
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	prev := rawGitHubBase
	rawGitHubBase = srv.URL
	t.Cleanup(func() { rawGitHubBase = prev })

	return func(path string, body []byte) {
		bodies[path] = body
	}
}

// --- decodePublicKey ---

func TestDecodePublicKey_PEMHappyPath(t *testing.T) {
	pub, pemBytes := genTestKey(t)
	got, err := decodePublicKey(pemBytes)
	if err != nil {
		t.Fatalf("decodePublicKey: %v", err)
	}
	if !ed25519.PublicKey(got).Equal(pub) {
		t.Errorf("returned key does not match generated public key")
	}
}

func TestDecodePublicKey_RawBase64(t *testing.T) {
	pub, _ := genTestKey(t)
	encoded := base64.StdEncoding.EncodeToString(pub) + "\n"
	got, err := decodePublicKey([]byte(encoded))
	if err != nil {
		t.Fatalf("decodePublicKey: %v", err)
	}
	if !ed25519.PublicKey(got).Equal(pub) {
		t.Errorf("returned key does not match generated public key")
	}
}

func TestDecodePublicKey_GarbageContents(t *testing.T) {
	if _, err := decodePublicKey([]byte("not a key")); err == nil {
		t.Fatal("expected error for garbage contents")
	}
}

// --- keyring trust ---

func TestKeyringTrust_AddsKeyAndPersists(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/example/repo/HEAD/keys/publisher.pub": pemBytes,
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring(
		[]string{"trust", "github://example/repo"},
		stdout, stderr,
	)
	if rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Trusted publisher github://example/repo") {
		t.Errorf("expected success message; got: %s", stdout.String())
	}

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
	withMockGitHubRaw(t, map[string][]byte{
		"/example/repo/HEAD/keys/publisher.pub": pemBytes,
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://example/repo"}, stdout, stderr); rc != 0 {
		t.Fatalf("first trust: rc = %d; stderr = %s", rc, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	rc := runKeyring([]string{"trust", "github://example/repo"}, stdout, stderr)
	if rc != 0 {
		t.Fatalf("re-trust rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Known publisher already trusted") {
		t.Errorf("expected duplicate-add to print 'Known publisher already trusted'; got: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "✓ Trusted publisher") {
		t.Errorf("re-trust should not print success message; got: %s", stdout.String())
	}
}

func TestKeyringTrust_RotationAddsAlongsideExisting(t *testing.T) {
	home := withTempHome(t)
	pub1, pem1 := genTestKey(t)
	pub2, pem2 := genTestKey(t)
	setBody := withMockGitHubRaw(t, map[string][]byte{
		"/example/repo/HEAD/keys/publisher.pub": pem1,
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://example/repo"}, stdout, stderr); rc != 0 {
		t.Fatalf("first trust: rc = %d; stderr = %s", rc, stderr.String())
	}

	// Publisher rotates: same convention path, new key bytes.
	setBody("/example/repo/HEAD/keys/publisher.pub", pem2)
	stdout.Reset()
	stderr.Reset()
	if rc := runKeyring([]string{"trust", "github://example/repo"}, stdout, stderr); rc != 0 {
		t.Fatalf("rotated trust: rc = %d; stderr = %s", rc, stderr.String())
	}

	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasKey("github://example/repo", pub1) {
		t.Error("first key not preserved after rotation")
	}
	if !kr.HasKey("github://example/repo", pub2) {
		t.Error("rotated key not added")
	}
	if got := len(kr.Keys("github://example/repo")); got != 2 {
		t.Errorf("len(keys) = %d, want 2", got)
	}
}

func TestKeyringTrust_RejectsEmptyAuthority(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", ""}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit for empty authority")
	}
}

func TestKeyringTrust_WrongArgCount(t *testing.T) {
	cases := [][]string{
		{"trust"},
		{"trust", "github://example/repo", "extra-arg"},
	}
	for _, args := range cases {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if rc := runKeyring(args, stdout, stderr); rc == 0 {
			t.Errorf("args=%v: expected non-zero exit", args)
		}
		if !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("args=%v: expected usage hint; got: %s", args, stderr.String())
		}
	}
}

func TestKeyringTrust_RejectsNonGitHubScheme(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "gitlab://example/repo"}, stdout, stderr)
	if rc == 0 {
		t.Fatal("expected non-zero exit for non-github scheme")
	}
	if !strings.Contains(stderr.String(), "github://") {
		t.Errorf("expected error to point at github:// support; got: %s", stderr.String())
	}
}

func TestKeyringTrust_InvalidAuthority(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "not-a-uri"}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit for malformed authority")
	}
	if !strings.Contains(stderr.String(), "fetch publisher key") {
		t.Errorf("expected fetch error context; got: %s", stderr.String())
	}
}

func TestKeyringTrust_MissingConventionPath(t *testing.T) {
	withTempHome(t)
	withMockGitHubRaw(t, map[string][]byte{}) // every path returns 404

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "github://nope/no-such-repo"}, stdout, stderr)
	if rc == 0 {
		t.Fatal("expected non-zero exit when convention path is absent")
	}
	if !strings.Contains(stderr.String(), "404") {
		t.Errorf("expected 404 in error; got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "keys/publisher.pub") {
		t.Errorf("expected error to name the convention path; got: %s", stderr.String())
	}
}

func TestKeyringTrust_GarbageBody(t *testing.T) {
	withTempHome(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/owner/repo/HEAD/keys/publisher.pub": []byte("not a key"),
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://owner/repo"}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit for garbage body")
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
	withMockGitHubRaw(t, map[string][]byte{
		"/b/second/HEAD/keys/publisher.pub": pem1,
		"/a/first/HEAD/keys/publisher.pub":  pem2,
	})

	for _, authority := range []string{"github://b/second", "github://a/first"} {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if rc := runKeyring([]string{"trust", authority}, stdout, stderr); rc != 0 {
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
	withMockGitHubRaw(t, map[string][]byte{
		"/example/repo/HEAD/keys/publisher.pub": pemBytes,
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://example/repo"}, stdout, stderr); rc != 0 {
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
