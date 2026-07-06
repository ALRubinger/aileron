package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// seedOwnerGrant writes an owner-level grant directly to the test
// keyring and returns the public key. Used by list/revoke tests that
// need a known owner grant without going through the network path.
func seedOwnerGrant(t *testing.T, ownerAuthority string) ed25519.PublicKey {
	t.Helper()
	pub, _ := genTestKey(t)
	path := cstore.DefaultKeyringPath()
	kr, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("seedOwnerGrant load: %v", err)
	}
	kr.AddOwner(ownerAuthority, pub)
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("seedOwnerGrant save: %v", err)
	}
	return pub
}

// seedPerRepoGrant writes a per-repo grant directly to the test keyring
// and returns the public key.
func seedPerRepoGrant(t *testing.T, authority string) ed25519.PublicKey {
	t.Helper()
	pub, _ := genTestKey(t)
	path := cstore.DefaultKeyringPath()
	kr, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("seedPerRepoGrant load: %v", err)
	}
	kr.Add(authority, pub)
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("seedPerRepoGrant save: %v", err)
	}
	return pub
}

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
	// Neutralize any ambient GitHub token so the default (token-absent)
	// keyring test path is the anonymous raw fetch. Tests that exercise the
	// authenticated Contents API set GH_TOKEN/GITHUB_TOKEN explicitly after
	// this, overriding the cleared value.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
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

	// The convention path is anonymous only when no token is set; clear
	// any ambient token so a developer/CI environment with GH_TOKEN or
	// GITHUB_TOKEN exported does not silently reroute the fetch to the
	// (unmocked) Contents API base.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

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

// withMockGitHubAPI stands up an httptest server that mimics the GitHub
// Contents API (`/repos/{owner}/{repo}/contents/{path}`) and points the
// CLI's githubAPIBase at it. The server serves body only when the request
// carries an `Authorization: Bearer <token>` header (private content), and
// 404s otherwise, so a test can prove the authenticated path is exercised.
// It records whether it was ever hit for tests that assert no network call.
func withMockGitHubAPI(t *testing.T, contentsPath string, body []byte) *bool {
	t.Helper()
	hit := new(bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hit = true
		if r.URL.Path != contentsPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "requires authentication", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	prev := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = prev })
	return hit
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
	// A full-FQN trust resolves the repo's own key but grants it
	// owner-level, so the success message names the owner authority.
	if !strings.Contains(stdout.String(), "Trusted publisher github://example\n") {
		t.Errorf("expected owner-level success message; got: %s", stdout.String())
	}

	keyringPath := filepath.Join(home, ".aileron", "keyring.json")
	kr, err := cstore.LoadKeyring(keyringPath)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://example", pub) {
		t.Errorf("keyring at %s does not contain the owner-level trusted key", keyringPath)
	}
	if len(kr.Keys("github://example/repo")) != 0 {
		t.Errorf("trust should not write a per-repo grant; got %d", len(kr.Keys("github://example/repo")))
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
	if !strings.Contains(stdout.String(), "Publisher already trusted") {
		t.Errorf("expected duplicate-add to print 'Publisher already trusted'; got: %s", stdout.String())
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
	// Rotation accumulates both keys under the owner-level grant.
	if !kr.HasOwnerKey("github://example", pub1) {
		t.Error("first key not preserved after rotation")
	}
	if !kr.HasOwnerKey("github://example", pub2) {
		t.Error("rotated key not added")
	}
	if got := len(kr.OwnerKeys("github://example")); got != 2 {
		t.Errorf("len(owner keys) = %d, want 2", got)
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
	if !strings.Contains(stderr.String(), "parse authority") {
		t.Errorf("expected parse error context; got: %s", stderr.String())
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

// TestKeyringTrust_PrivateRepoResolvesViaAuthenticatedAPI is the #2009
// regression: raw.githubusercontent.com 404s on private content, but with
// a token present the key resolves through the authenticated Contents API
// and trust succeeds. Fails before the fix (anonymous raw only) because
// raw returns 404.
func TestKeyringTrust_PrivateRepoResolvesViaAuthenticatedAPI(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	// raw returns 404 for every path (mirrors a private repo hidden from
	// anonymous raw).
	withMockGitHubRaw(t, map[string][]byte{})
	// The token flips the fetch onto the Contents API, which serves the key.
	t.Setenv("GH_TOKEN", "gh-secret")
	apiHit := withMockGitHubAPI(t, "/repos/acme/private-connector/contents/keys/publisher.pub", pemBytes)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "github://acme/private-connector"}, stdout, stderr)
	if rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !*apiHit {
		t.Error("authenticated Contents API was not called")
	}
	if !strings.Contains(stdout.String(), "Trusted publisher github://acme\n") {
		t.Errorf("expected owner-level success message; got: %s", stdout.String())
	}
	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("owner-level key not pinned after authenticated fetch")
	}
}

// TestKeyringTrust_GHTokenPreferredOverGitHubToken verifies GH_TOKEN wins
// when both are set (gh CLI precedence), matching resolveLatestRef.
func TestKeyringTrust_GHTokenPreferredOverGitHubToken(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{})
	t.Setenv("GH_TOKEN", "gh-wins")
	t.Setenv("GITHUB_TOKEN", "github-loses")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(pemBytes)
	}))
	t.Cleanup(srv.Close)
	prev := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = prev })

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://acme/repo"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if gotAuth != "Bearer gh-wins" {
		t.Errorf("Authorization = %q, want Bearer gh-wins", gotAuth)
	}
}

// TestKeyringTrust_PrivateRepoAPI404GivesGuidance verifies the
// authenticated-path 404 (e.g. token lacks repo access) names the
// convention path and hints at token access.
func TestKeyringTrust_PrivateRepoAPI404GivesGuidance(t *testing.T) {
	withTempHome(t)
	withMockGitHubRaw(t, map[string][]byte{})
	t.Setenv("GITHUB_TOKEN", "no-access")
	withMockGitHubAPI(t, "/repos/acme/nope/contents/keys/publisher.pub", nil) // 404s: path mismatch below

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "github://acme/other"}, stdout, stderr)
	if rc == 0 {
		t.Fatal("expected non-zero exit when the API path is absent")
	}
	if !strings.Contains(stderr.String(), "keys/publisher.pub") {
		t.Errorf("expected error to name the convention path; got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "read access") {
		t.Errorf("expected token-access hint; got: %s", stderr.String())
	}
}

// TestKeyringTrust_Anonymous404MentionsToken verifies the anonymous-path
// 404 now points the operator at GH_TOKEN/GITHUB_TOKEN for private repos.
func TestKeyringTrust_Anonymous404MentionsToken(t *testing.T) {
	withTempHome(t)
	withMockGitHubRaw(t, map[string][]byte{}) // 404 for every path, no token

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://acme/private"}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit on 404")
	}
	if !strings.Contains(stderr.String(), "GH_TOKEN") || !strings.Contains(stderr.String(), "GITHUB_TOKEN") {
		t.Errorf("expected 404 to mention GH_TOKEN/GITHUB_TOKEN; got: %s", stderr.String())
	}
}

// TestKeyringTrust_KeyFileGrantsOwnerTrustNoNetwork covers acceptance item
// 2: --key-file reads a local key and grants owner trust with no HTTP call.
func TestKeyringTrust_KeyFileGrantsOwnerTrustNoNetwork(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	keyPath := filepath.Join(t.TempDir(), "publisher.pub")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	// Point both bases at a server that fails the test if it is ever hit.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network call to %s", r.URL.Path)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(fail.Close)
	prevRaw, prevAPI := rawGitHubBase, githubAPIBase
	rawGitHubBase, githubAPIBase = fail.URL, fail.URL
	t.Cleanup(func() { rawGitHubBase, githubAPIBase = prevRaw, prevAPI })

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "--key-file", keyPath, "github://acme/connector"}, stdout, stderr)
	if rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Trusted publisher github://acme\n") {
		t.Errorf("expected owner-level success message; got: %s", stdout.String())
	}
	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("owner-level key not pinned from --key-file")
	}
}

// TestKeyringTrust_KeyFileBareOwner verifies --key-file also accepts a
// bare owner authority (which ParseFQN rejects), granting owner trust.
func TestKeyringTrust_KeyFileBareOwner(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	keyPath := filepath.Join(t.TempDir(), "publisher.pub")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "--key-file", keyPath, "github://acme"}, stdout, stderr)
	if rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("owner-level key not pinned from --key-file for bare owner")
	}
}

// TestKeyringTrust_KeyFileMissingFileErrors verifies a bad --key-file path
// fails cleanly with context.
func TestKeyringTrust_KeyFileMissingFileErrors(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "--key-file", filepath.Join(t.TempDir(), "absent.pub"), "github://acme/repo"}, stdout, stderr)
	if rc == 0 {
		t.Fatal("expected non-zero exit for missing key file")
	}
	if !strings.Contains(stderr.String(), "read key file") {
		t.Errorf("expected read-file error context; got: %s", stderr.String())
	}
}

// TestKeyringTrust_KeyFileGarbageErrors verifies a --key-file that is not a
// valid key fails with decode context.
func TestKeyringTrust_KeyFileGarbageErrors(t *testing.T) {
	withTempHome(t)
	keyPath := filepath.Join(t.TempDir(), "publisher.pub")
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "--key-file", keyPath, "github://acme/repo"}, stdout, stderr)
	if rc == 0 {
		t.Fatal("expected non-zero exit for garbage key file")
	}
	if !strings.Contains(stderr.String(), "parse key file") {
		t.Errorf("expected parse-file error context; got: %s", stderr.String())
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
	// Trust writes owner-level grants, so list groups under the owner
	// authorities `github://a` and `github://b`, sorted.
	idxA := strings.Index(out, "github://a ")
	idxB := strings.Index(out, "github://b ")
	if idxA < 0 || idxB < 0 {
		t.Fatalf("expected both owner authorities in output; got: %s", out)
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
	// trust github://example/repo grants owner-level trust for
	// github://example, so revocation targets the owner authority.
	if rc := runKeyring([]string{"revoke", "github://example"}, stdout, stderr); rc != 0 {
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

// --- keyring trust: owner-level grant from a full FQN ---

// TestKeyringTrust_FullFQNWritesOwnerGrantNotPerRepo asserts the
// headline #1418 behavior: trusting a full per-repo FQN fetches that
// repo's own key but records an owner-level grant (under "owners"), with
// no per-repo entry, so the grant covers every connector the publisher
// ships.
func TestKeyringTrust_FullFQNWritesOwnerGrantNotPerRepo(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/connector/HEAD/keys/publisher.pub": pemBytes,
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://acme/connector"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}

	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("expected owner-level grant under github://acme")
	}
	if len(kr.Keys("github://acme/connector")) != 0 {
		t.Error("expected NO per-repo grant under github://acme/connector")
	}

	// Re-running is a no-op (no duplicate owner key).
	stdout.Reset()
	stderr.Reset()
	if rc := runKeyring([]string{"trust", "github://acme/connector"}, stdout, stderr); rc != 0 {
		t.Fatalf("re-trust rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Publisher already trusted") {
		t.Errorf("expected no-op message; got: %s", stdout.String())
	}
	kr2, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if got := len(kr2.OwnerKeys("github://acme")); got != 1 {
		t.Errorf("owner keys = %d after re-trust, want 1 (no duplicate)", got)
	}
}

// --- keyring trust: bare owner resolved via the Hub ---

// withMockHub points fetchHubConnectors' transport (bindingAPIBaseURL,
// via AILERON_API_URL) at an in-process server serving /v1/hub/connectors
// with the supplied entries.
func withMockHub(t *testing.T, entries []hubConnectorEntry) {
	t.Helper()
	body, err := json.Marshal(hubConnectorList{Connectors: entries})
	if err != nil {
		t.Fatalf("marshal hub list: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hub/connectors" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
}

func TestKeyringTrust_BareOwnerResolvesViaHubKeyURL(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	// The raw server serves the connector's own publisher.pub; the Hub
	// entry's key_url points at it.
	setBody := withMockGitHubRaw(t, map[string][]byte{
		"/acme/connector/HEAD/keys/publisher.pub": pemBytes,
	})
	_ = setBody
	keyURL := rawGitHubBase + "/acme/connector/HEAD/keys/publisher.pub"
	withMockHub(t, []hubConnectorEntry{
		{FQN: "github://acme/connector", PublisherGithub: "acme", KeyURL: keyURL},
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://acme"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("expected owner-level grant for github://acme from Hub key_url")
	}
}

// TestKeyringTrust_BareOwnerTrailingSlashResolvesAsOwner asserts
// `github://acme/` is treated as the acme owner (routed to Hub
// resolution), not a per-repo authority with an empty repo.
func TestKeyringTrust_BareOwnerTrailingSlashResolvesAsOwner(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/connector/HEAD/keys/publisher.pub": pemBytes,
	})
	keyURL := rawGitHubBase + "/acme/connector/HEAD/keys/publisher.pub"
	withMockHub(t, []hubConnectorEntry{
		{FQN: "github://acme/connector", PublisherGithub: "acme", KeyURL: keyURL},
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"trust", "github://acme/"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("trailing-slash owner should write a canonical github://acme grant")
	}
	if len(kr.OwnerKeys("github://acme/")) != 0 {
		t.Error("grant key must be canonical (no trailing slash)")
	}
}

// TestKeyringTrust_BareOwnerNoHubEntryErrorsWithGuidance asserts that a
// bare owner with no resolvable Hub key_url fails with guidance and never
// guesses a profile-repo raw path.
func TestKeyringTrust_BareOwnerNoHubEntryErrorsWithGuidance(t *testing.T) {
	home := withTempHome(t)
	// Raw server records every request so we can assert no <owner>/<owner>
	// profile-repo path is ever fetched.
	var rawPaths []string
	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawPaths = append(rawPaths, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(rawSrv.Close)
	prev := rawGitHubBase
	rawGitHubBase = rawSrv.URL
	t.Cleanup(func() { rawGitHubBase = prev })

	// Hub returns an entry for a different owner / one with no key_url.
	withMockHub(t, []hubConnectorEntry{
		{FQN: "github://other/connector", PublisherGithub: "other", KeyURL: rawSrv.URL + "/other/connector/HEAD/keys/publisher.pub"},
		{FQN: "github://acme/connector", PublisherGithub: "acme", KeyURL: ""},
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "github://acme"}, stdout, stderr)
	if rc == 0 {
		t.Fatal("expected non-zero exit when no Hub key_url resolves for the owner")
	}
	if !strings.Contains(stderr.String(), "keyring trust github://acme/<repo>") {
		t.Errorf("expected guidance to trust via a specific connector; got: %s", stderr.String())
	}
	// Never fetched a guessed <owner>/<owner> profile path.
	for _, p := range rawPaths {
		if strings.Contains(p, "/acme/acme/") {
			t.Errorf("must not fetch a guessed profile-repo path; got request to %q", p)
		}
	}
	// Keyring unchanged.
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if len(kr.Authorities()) != 0 {
		t.Errorf("keyring should be unchanged; got %v", kr.Authorities())
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

// --- keyring list grouped by owner ---

func TestKeyringList_GroupsByOwner(t *testing.T) {
	withTempHome(t)
	ownerPub := seedOwnerGrant(t, "github://acme")
	repo1Pub := seedPerRepoGrant(t, "github://acme/conn1")
	repo2Pub := seedPerRepoGrant(t, "github://acme/conn2")
	otherPub := seedPerRepoGrant(t, "github://other/conn")
	_ = ownerPub
	_ = repo1Pub
	_ = repo2Pub
	_ = otherPub

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	out := stdout.String()

	// Owners sorted: github://acme header before github://other header.
	idxAcme := strings.Index(out, "github://acme ")
	idxOther := strings.Index(out, "github://other ")
	if idxAcme < 0 || idxOther < 0 {
		t.Fatalf("expected both owner headers; got: %s", out)
	}
	if idxAcme > idxOther {
		t.Errorf("expected acme before other; got: %s", out)
	}
	// Owner-level fingerprint shown under the acme header.
	if !strings.Contains(out, "(1 owner key)") {
		t.Errorf("expected owner-key count line; got: %s", out)
	}
	// Per-repo grants nested under acme, sorted.
	idxConn1 := strings.Index(out, "github://acme/conn1")
	idxConn2 := strings.Index(out, "github://acme/conn2")
	if idxConn1 < 0 || idxConn2 < 0 {
		t.Fatalf("expected both per-repo entries under acme; got: %s", out)
	}
	if !(idxAcme < idxConn1 && idxConn1 < idxConn2 && idxConn2 < idxOther) {
		t.Errorf("expected acme header < conn1 < conn2 < other header; got: %s", out)
	}
}

// TestKeyringList_OwnerWithOnlyPerRepoGrantsRendersHeader asserts an
// owner that has only per-repo grants (no owner-level key) still gets a
// group header, with no owner-level fingerprint line.
func TestKeyringList_OwnerWithOnlyPerRepoGrantsRendersHeader(t *testing.T) {
	withTempHome(t)
	seedPerRepoGrant(t, "github://acme/conn1")

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"list"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "github://acme  (0 owner keys)") {
		t.Errorf("expected owner header with zero owner keys; got: %s", out)
	}
	if !strings.Contains(out, "github://acme/conn1") {
		t.Errorf("expected per-repo entry under the owner; got: %s", out)
	}
}

func TestKeyringList_WrongArgCount(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"list", "extra"}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit for extra arg")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage hint; got: %s", stderr.String())
	}
}

// --- keyring revoke: owner and --key forms ---

func TestKeyringRevoke_OwnerRemovesOwnerGrantLeavesPerRepo(t *testing.T) {
	home := withTempHome(t)
	seedOwnerGrant(t, "github://acme")
	repoPub := seedPerRepoGrant(t, "github://acme/conn")

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke", "github://acme"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revoked publisher github://acme") {
		t.Errorf("expected revoke message; got: %s", stdout.String())
	}
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if len(kr.OwnerKeys("github://acme")) != 0 {
		t.Error("owner grant should be gone")
	}
	if !kr.HasKey("github://acme/conn", repoPub) {
		t.Error("per-repo grant under acme should be untouched")
	}
}

func TestKeyringRevoke_OwnerNoGrantIsNoChange(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke", "github://acme"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no change") {
		t.Errorf("expected no-change message; got: %s", stdout.String())
	}
}

func TestKeyringRevoke_KeyFlagRemovesAcrossOwnerAndPerRepo(t *testing.T) {
	home := withTempHome(t)
	// Same key registered under an owner AND a per-repo authority, plus
	// an unrelated key that must survive.
	shared, _ := genTestKey(t)
	other, _ := genTestKey(t)
	path := cstore.DefaultKeyringPath()
	kr, _ := cstore.LoadKeyring(path)
	kr.AddOwner("github://acme", shared)
	kr.Add("github://acme/conn", shared)
	kr.Add("github://acme/conn", other)
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	fp := fingerprint(shared)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke", "--key", fp}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revoked key") {
		t.Errorf("expected key-revoke message; got: %s", stdout.String())
	}
	kr2, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if kr2.HasOwnerKey("github://acme", shared) {
		t.Error("shared key should be removed from owner authority")
	}
	if kr2.HasKey("github://acme/conn", shared) {
		t.Error("shared key should be removed from per-repo authority")
	}
	if !kr2.HasKey("github://acme/conn", other) {
		t.Error("unrelated key under the per-repo authority must survive")
	}
}

func TestKeyringRevoke_KeyEqualsFormParsesIdentically(t *testing.T) {
	withTempHome(t)
	pub := seedOwnerGrant(t, "github://acme")
	fp := fingerprint(pub)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke", "--key=" + fp}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revoked key") {
		t.Errorf("expected key-revoke message; got: %s", stdout.String())
	}
}

func TestKeyringRevoke_UnknownKeyIsNoChange(t *testing.T) {
	withTempHome(t)
	seedOwnerGrant(t, "github://acme")
	unknown, _ := genTestKey(t)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke", "--key", fingerprint(unknown)}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no change") {
		t.Errorf("expected no-change message; got: %s", stdout.String())
	}
}

func TestKeyringRevoke_BothFormsIsUsageError(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"revoke", "github://acme", "--key", "sha256:abc"}, stdout, stderr)
	if rc == 0 {
		t.Fatal("expected usage error when both authority and --key are given")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage hint; got: %s", stderr.String())
	}
}

func TestKeyringRevoke_NeitherFormIsUsageError(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke"}, stdout, stderr); rc == 0 {
		t.Fatal("expected usage error when neither authority nor --key is given")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage hint; got: %s", stderr.String())
	}
}

// --- install bridge: ensureAuthorityTrusted writes owner grants ---

// TestEnsureAuthorityTrusted_AutoYesWritesOwnerGrantSingleFetch asserts
// the #563 single-install flow now writes an owner-level grant from one
// fetch of the per-repo connector key.
func TestEnsureAuthorityTrusted_AutoYesWritesOwnerGrantSingleFetch(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/acme/conn/HEAD/keys/publisher.pub" {
			fetches++
			w.Write(pemBytes)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	prev := rawGitHubBase
	rawGitHubBase = srv.URL
	t.Cleanup(func() { rawGitHubBase = prev })

	var stdout, stderr bytes.Buffer
	if err := ensureAuthorityTrustedErr(t, "github://acme/conn", true, &stdout, &stderr); err != nil {
		t.Fatalf("ensureAuthorityTrusted: %v; stderr=%s", err, stderr.String())
	}
	if fetches != 1 {
		t.Errorf("expected exactly one key fetch, got %d", fetches)
	}
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("expected owner-level grant after auto-yes accept")
	}
}

// TestEnsureAuthorityTrusted_OwnerGrantCoversSiblingNoFetch asserts that
// once an owner grant exists, a different repo under the same owner is a
// no-op with no prompt and no fetch.
func TestEnsureAuthorityTrusted_OwnerGrantCoversSiblingNoFetch(t *testing.T) {
	withTempHome(t)
	seedOwnerGrant(t, "github://acme")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	prev := rawGitHubBase
	rawGitHubBase = srv.URL
	t.Cleanup(func() { rawGitHubBase = prev })

	var stdout, stderr bytes.Buffer
	if err := ensureAuthorityTrustedErr(t, "github://acme/other-conn", false, &stdout, &stderr); err != nil {
		t.Fatalf("expected nil (already owner-trusted), got %v", err)
	}
	if hits != 0 {
		t.Errorf("expected zero fetches when owner grant covers the repo, got %d", hits)
	}
	if strings.Contains(stdout.String(), "is not yet trusted") {
		t.Errorf("should not prompt when owner grant exists; got: %s", stdout.String())
	}
}

// TestEnsureAuthorityTrusted_PerRepoPinDoesNotCoverSibling asserts a
// standalone per-repo pin satisfies that exact repo but NOT a sibling
// under the same owner (per-repo pin does not read as owner-trust).
func TestEnsureAuthorityTrusted_PerRepoPinDoesNotCoverSibling(t *testing.T) {
	withTempHome(t)
	seedPerRepoGrant(t, "github://acme/conn")

	// Exact repo: no-op, no prompt.
	var stdout, stderr bytes.Buffer
	if err := ensureAuthorityTrustedErr(t, "github://acme/conn", false, &stdout, &stderr); err != nil {
		t.Fatalf("pinned repo should be a no-op, got %v", err)
	}
	if strings.Contains(stdout.String(), "is not yet trusted") {
		t.Errorf("pinned repo should not prompt; got: %s", stdout.String())
	}

	// Sibling repo: prompts (decline aborts). Empty stdin => declines.
	stdout.Reset()
	stderr.Reset()
	err := ensureAuthorityTrustedErr(t, "github://acme/sibling", false, &stdout, &stderr)
	if err == nil {
		t.Fatal("sibling should prompt and abort on empty stdin")
	}
	if !strings.Contains(stdout.String(), "is not yet trusted") {
		t.Errorf("expected sibling prompt; got: %s", stdout.String())
	}
}

// TestTrustStateEnsure_SuiteSinglePromptPerOwner asserts that within one
// run, ensure() for two connectors of the same owner fetches/writes once
// and the second short-circuits.
func TestTrustStateEnsure_SuiteSinglePromptPerOwner(t *testing.T) {
	withTempHome(t)
	pub, pemBytes := genTestKey(t)
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		w.Write(pemBytes)
	}))
	t.Cleanup(srv.Close)
	prev := rawGitHubBase
	rawGitHubBase = srv.URL
	t.Cleanup(func() { rawGitHubBase = prev })

	st := newTrustState()
	var stdout, stderr bytes.Buffer
	if err := st.ensure("github://acme/conn1", true, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := st.ensure("github://acme/conn2", true, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if fetches != 1 {
		t.Errorf("expected one fetch across two sibling connectors, got %d", fetches)
	}
	_ = pub
}

// ensureAuthorityTrustedErr is a thin test adapter that drives the
// install bridge with an empty stdin (so an un-accepted prompt declines)
// and discards the owner-covered bool, returning only the error.
func ensureAuthorityTrustedErr(t *testing.T, authority string, autoYes bool, stdout, stderr *bytes.Buffer) error {
	t.Helper()
	_, err := ensureAuthorityTrusted(authority, autoYes, strings.NewReader(""), stdout, stderr)
	return err
}

// TestKeyringRevoke_ExactPerRepoAuthority asserts revoking an exact
// per-repo authority (not a bare owner) removes just that per-repo grant
// via the Remove path.
func TestKeyringRevoke_ExactPerRepoAuthority(t *testing.T) {
	home := withTempHome(t)
	ownerPub := seedOwnerGrant(t, "github://acme")
	seedPerRepoGrant(t, "github://acme/conn")

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke", "github://acme/conn"}, stdout, stderr); rc != 0 {
		t.Fatalf("rc = %d; stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revoked publisher github://acme/conn") {
		t.Errorf("expected per-repo revoke message; got: %s", stdout.String())
	}
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if len(kr.Keys("github://acme/conn")) != 0 {
		t.Error("per-repo grant should be removed")
	}
	if !kr.HasOwnerKey("github://acme", ownerPub) {
		t.Error("owner grant must survive a per-repo revoke")
	}
}

// TestKeyringTrust_BareOwnerHubFetchFailsGuidance asserts that when the
// Hub query itself fails (daemon unreachable / non-200), trust errors
// with the same trust-via-connector guidance.
func TestKeyringTrust_BareOwnerHubFetchFailsGuidance(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rc := runKeyring([]string{"trust", "github://acme"}, stdout, stderr)
	if rc == 0 {
		t.Fatal("expected non-zero exit when the Hub query fails")
	}
	if !strings.Contains(stderr.String(), "keyring trust github://acme/<repo>") {
		t.Errorf("expected trust-via-connector guidance; got: %s", stderr.String())
	}
}

func TestKeyringRevoke_UnknownFlagIsUsageError(t *testing.T) {
	withTempHome(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if rc := runKeyring([]string{"revoke", "--bogus"}, stdout, stderr); rc == 0 {
		t.Fatal("expected non-zero exit for unknown flag")
	}
}
