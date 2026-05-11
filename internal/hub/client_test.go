package hub_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/hub"
)

// makeFixtureHub creates a temporary git repo on disk laid out like
// `aileron-connectors-hub`, with one entry per supplied map item.
// Returns a `file://` URL the hub.Client can clone from.
func makeFixtureHub(t *testing.T, entries map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	connectors := filepath.Join(dir, "connectors")
	if err := os.MkdirAll(connectors, 0o755); err != nil {
		t.Fatalf("mkdir connectors: %v", err)
	}
	// Ensure git always has at least one file to commit, so the empty-
	// entries case still produces a valid repo.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for name, body := range entries {
		path := filepath.Join(connectors, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("add", "-A")
	runGit("commit", "-m", "seed")

	return "file://" + dir
}

func TestFetchAll_ReturnsAllEntriesSortedByFQN(t *testing.T) {
	url := makeFixtureHub(t, map[string]string{
		"github_bob_z.yaml": `
fqn: github://bob/z
description: Z
publisher_github: bob
key_url: https://example.com/bob.pub
release_pattern: v*
`,
		"github_alice_a.yaml": `
fqn: github://alice/a
description: A
publisher_github: alice
key_url: https://example.com/alice.pub
release_pattern: v*
`,
	})
	c := &hub.Client{URL: url}

	entries, err := c.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].FQN != "github://alice/a" || entries[1].FQN != "github://bob/z" {
		t.Fatalf("entries not sorted by FQN: %+v", entries)
	}
	if entries[0].PublisherGithub != "alice" || entries[1].Description != "Z" {
		t.Fatalf("entry fields not parsed: %+v", entries)
	}
}

func TestFetchAll_EmptyConnectorsDirReturnsNil(t *testing.T) {
	url := makeFixtureHub(t, map[string]string{})
	c := &hub.Client{URL: url}

	entries, err := c.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(entries))
	}
}

func TestFetchAll_UnreachableURLReturnsError(t *testing.T) {
	c := &hub.Client{URL: "file:///nonexistent/path/that/does/not/exist"}

	_, err := c.FetchAll(context.Background())
	if err == nil {
		t.Fatalf("expected error for unreachable URL, got nil")
	}
	if !strings.Contains(err.Error(), "hub:") {
		t.Fatalf("expected wrapped error with 'hub:' prefix, got %v", err)
	}
}

func TestFetchByFQN_ReturnsMatchingEntry(t *testing.T) {
	url := makeFixtureHub(t, map[string]string{
		"github_alice_a.yaml": `
fqn: github://alice/a
description: A connector
publisher_github: alice
key_url: https://example.com/alice.pub
release_pattern: v*
`,
	})
	c := &hub.Client{URL: url}

	entry, err := c.FetchByFQN(context.Background(), "github://alice/a")
	if err != nil {
		t.Fatalf("FetchByFQN: %v", err)
	}
	if entry.FQN != "github://alice/a" || entry.Description != "A connector" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

func TestFetchByFQN_ReturnsErrNotFoundWhenMissing(t *testing.T) {
	url := makeFixtureHub(t, map[string]string{
		"github_alice_a.yaml": `
fqn: github://alice/a
description: A
publisher_github: alice
key_url: https://example.com/alice.pub
release_pattern: v*
`,
	})
	c := &hub.Client{URL: url}

	_, err := c.FetchByFQN(context.Background(), "github://nobody/missing")
	if err != hub.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFilterByKeyword_MatchesFQNAndDescription(t *testing.T) {
	entries := []hub.Entry{
		{FQN: "github://alice/google", Description: "Google Workspace"},
		{FQN: "github://bob/slack", Description: "Slack messaging"},
		{FQN: "github://charlie/x", Description: "Random thing about google"},
	}
	got := hub.FilterByKeyword(entries, "google")
	if len(got) != 2 {
		t.Fatalf("expected 2 google matches, got %d: %+v", len(got), got)
	}
	// Case-insensitive
	got = hub.FilterByKeyword(entries, "SLACK")
	if len(got) != 1 || got[0].FQN != "github://bob/slack" {
		t.Fatalf("case-insensitive match failed: %+v", got)
	}
	// Empty keyword returns all
	got = hub.FilterByKeyword(entries, "")
	if len(got) != 3 {
		t.Fatalf("empty keyword should return all, got %d", len(got))
	}
}

func TestPublisherFootprint_ReturnsSiblingFQNsExcludingSelf(t *testing.T) {
	entries := []hub.Entry{
		{FQN: "github://alice/one", PublisherGithub: "alice"},
		{FQN: "github://alice/two", PublisherGithub: "alice"},
		{FQN: "github://alice/three", PublisherGithub: "alice"},
		{FQN: "github://bob/x", PublisherGithub: "bob"},
	}
	got := hub.PublisherFootprint(entries, entries[0])
	if len(got) != 2 {
		t.Fatalf("expected 2 siblings, got %d: %v", len(got), got)
	}
	for _, fqn := range got {
		if fqn == entries[0].FQN {
			t.Fatalf("footprint should exclude self FQN")
		}
		if !strings.HasPrefix(fqn, "github://alice/") {
			t.Fatalf("footprint should only contain alice's FQNs, got %s", fqn)
		}
	}
}

func TestFingerprint_FormatMatchesKeyringTrustCLI(t *testing.T) {
	// The fingerprint format is `sha256:<22 chars base64-no-padding>`
	// — same shape as `aileron keyring trust` output (per
	// cmd/aileron/keyring.go fingerprint()). Validate length, prefix,
	// and stability across calls.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	got := hub.Fingerprint(pub)
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("expected sha256: prefix, got %q", got)
	}
	if len(got) != len("sha256:")+22 {
		t.Fatalf("expected length %d, got %d (%q)", len("sha256:")+22, len(got), got)
	}
	again := hub.Fingerprint(pub)
	if again != got {
		t.Fatalf("fingerprint not stable: %q vs %q", got, again)
	}
}

func TestFetchPublisherKey_ReturnsPEMParsedKeyAndFingerprint(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	pemBytes := pemEncodePublic(t, pub)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(pemBytes)
	}))
	defer srv.Close()

	c := &hub.Client{URL: "file:///unused", HTTP: srv.Client()}
	gotPub, gotFingerprint, err := c.FetchPublisherKey(context.Background(), srv.URL+"/publisher.pub")
	if err != nil {
		t.Fatalf("FetchPublisherKey: %v", err)
	}
	if !pub.Equal(gotPub) {
		t.Fatalf("parsed key does not match generated key")
	}
	if gotFingerprint != hub.Fingerprint(pub) {
		t.Fatalf("fingerprint mismatch: got %q want %q", gotFingerprint, hub.Fingerprint(pub))
	}
}

func TestFetchAll_MalformedYAMLReturnsError(t *testing.T) {
	url := makeFixtureHub(t, map[string]string{
		"bad.yaml": "this: is: not: valid: yaml: at: all: [",
	})
	c := &hub.Client{URL: url}

	_, err := c.FetchAll(context.Background())
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected error to mention parse failure, got %v", err)
	}
}

func TestFetchAll_NoConnectorsDirReturnsEmpty(t *testing.T) {
	// Build a fixture without the connectors/ subdir at all (e.g. a
	// freshly-minted Hub repo that hasn't been seeded). The contract
	// says we serve "no entries" rather than erroring — list endpoints
	// return [], lookup endpoints return ErrNotFound.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("empty hub\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("add", "-A")
	runGit("commit", "-m", "seed")

	c := &hub.Client{URL: "file://" + dir}
	entries, err := c.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(entries))
	}
}

func TestFetchPublisherKey_NonPEMBodyReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not a PEM block at all"))
	}))
	defer srv.Close()

	c := &hub.Client{URL: "file:///unused", HTTP: srv.Client()}
	_, _, err := c.FetchPublisherKey(context.Background(), srv.URL+"/key.pub")
	if err == nil {
		t.Fatalf("expected PEM parse error, got nil")
	}
}

func TestFetchPublisherKey_ReturnsErrorOnHTTP404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := &hub.Client{URL: "file:///unused", HTTP: srv.Client()}
	_, _, err := c.FetchPublisherKey(context.Background(), srv.URL+"/missing.pub")
	if err == nil {
		t.Fatalf("expected error on 404, got nil")
	}
}

// pemEncodePublic wraps an ed25519 public key in the SubjectPublicKeyInfo
// PEM form that cstore.ParsePEMPublicKey accepts.
func pemEncodePublic(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
