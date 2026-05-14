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

// fixtureDirs supplies per-directory entry contents for makeFixtureHub.
// Keys are filenames (e.g. "github_alice_a.yaml"); values are raw YAML
// bodies. Pass `nil` for a directory to leave it absent (Hub repo
// without that subdir at all).
type fixtureDirs struct {
	connectors map[string]string
	actions    map[string]string
	suites     map[string]string
}

// makeFixtureHub creates a temporary git repo on disk laid out like
// `aileron-connectors-hub`, with the supplied entries per directory.
// Returns a `file://` URL the hub.Client can clone from.
func makeFixtureHub(t *testing.T, dirs fixtureDirs) string {
	t.Helper()
	dir := t.TempDir()
	// Ensure git always has at least one file to commit, so the empty-
	// entries case still produces a valid repo.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	writeDir := func(subdir string, entries map[string]string) {
		if entries == nil {
			return
		}
		path := filepath.Join(dir, subdir)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", subdir, err)
		}
		for name, body := range entries {
			if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0o644); err != nil {
				t.Fatalf("write %s/%s: %v", subdir, name, err)
			}
		}
	}
	writeDir("connectors", dirs.connectors)
	writeDir("actions", dirs.actions)
	writeDir("suites", dirs.suites)

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

// connectorsOnly is a small convenience for tests that only seed the
// connectors/ directory.
func connectorsOnly(t *testing.T, entries map[string]string) string {
	return makeFixtureHub(t, fixtureDirs{connectors: entries})
}

func TestFetchAllConnectors_ReturnsAllEntriesSortedByFQN(t *testing.T) {
	url := connectorsOnly(t, map[string]string{
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

	entries, err := c.FetchAllConnectors(context.Background())
	if err != nil {
		t.Fatalf("FetchAllConnectors: %v", err)
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

func TestFetchAllConnectors_EmptyConnectorsDirReturnsNil(t *testing.T) {
	url := connectorsOnly(t, map[string]string{})
	c := &hub.Client{URL: url}

	entries, err := c.FetchAllConnectors(context.Background())
	if err != nil {
		t.Fatalf("FetchAllConnectors: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(entries))
	}
}

func TestFetchAllConnectors_UnreachableURLReturnsError(t *testing.T) {
	c := &hub.Client{URL: "file:///nonexistent/path/that/does/not/exist"}

	_, err := c.FetchAllConnectors(context.Background())
	if err == nil {
		t.Fatalf("expected error for unreachable URL, got nil")
	}
	if !strings.Contains(err.Error(), "hub:") {
		t.Fatalf("expected wrapped error with 'hub:' prefix, got %v", err)
	}
}

func TestFetchConnectorByFQN_ReturnsMatchingEntry(t *testing.T) {
	url := connectorsOnly(t, map[string]string{
		"github_alice_a.yaml": `
fqn: github://alice/a
description: A connector
publisher_github: alice
key_url: https://example.com/alice.pub
release_pattern: v*
`,
	})
	c := &hub.Client{URL: url}

	entry, err := c.FetchConnectorByFQN(context.Background(), "github://alice/a")
	if err != nil {
		t.Fatalf("FetchConnectorByFQN: %v", err)
	}
	if entry.FQN != "github://alice/a" || entry.Description != "A connector" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

func TestFetchConnectorByFQN_ReturnsErrNotFoundWhenMissing(t *testing.T) {
	url := connectorsOnly(t, map[string]string{
		"github_alice_a.yaml": `
fqn: github://alice/a
description: A
publisher_github: alice
key_url: https://example.com/alice.pub
release_pattern: v*
`,
	})
	c := &hub.Client{URL: url}

	_, err := c.FetchConnectorByFQN(context.Background(), "github://nobody/missing")
	if err != hub.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFetchAllActions_ReturnsAllEntriesSortedByFQN(t *testing.T) {
	url := makeFixtureHub(t, fixtureDirs{
		actions: map[string]string{
			"github_alice_b.yaml": `
fqn: github://alice/conn/actions/b
description: Action B
publisher_github: alice
connector_fqn: github://alice/conn
intents: ["do b", "perform b"]
category: communication
`,
			"github_alice_a.yaml": `
fqn: github://alice/conn/actions/a
description: Action A
publisher_github: alice
connector_fqn: github://alice/conn
intents: ["do a"]
category: communication
`,
		},
	})
	c := &hub.Client{URL: url}

	entries, err := c.FetchAllActions(context.Background())
	if err != nil {
		t.Fatalf("FetchAllActions: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].FQN != "github://alice/conn/actions/a" || entries[1].FQN != "github://alice/conn/actions/b" {
		t.Fatalf("entries not sorted by FQN: %+v", entries)
	}
	if entries[0].ConnectorFQN != "github://alice/conn" {
		t.Fatalf("connector_fqn not parsed: %+v", entries[0])
	}
	if len(entries[0].Intents) != 1 || entries[0].Intents[0] != "do a" {
		t.Fatalf("intents not parsed: %+v", entries[0])
	}
	if entries[0].Category != "communication" {
		t.Fatalf("category not parsed: %+v", entries[0])
	}
}

func TestFetchAllActions_MissingDirReturnsEmpty(t *testing.T) {
	// Hub repo with no actions/ directory at all should return an empty
	// list, mirroring the connectors-dir-missing contract.
	url := connectorsOnly(t, map[string]string{})
	c := &hub.Client{URL: url}

	entries, err := c.FetchAllActions(context.Background())
	if err != nil {
		t.Fatalf("FetchAllActions: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(entries))
	}
}

func TestFetchAllActions_UnreachableURLReturnsError(t *testing.T) {
	c := &hub.Client{URL: "file:///nonexistent/path/that/does/not/exist"}
	_, err := c.FetchAllActions(context.Background())
	if err == nil {
		t.Fatalf("expected error for unreachable URL, got nil")
	}
	if !strings.Contains(err.Error(), "hub:") {
		t.Fatalf("expected wrapped error with 'hub:' prefix, got %v", err)
	}
}

func TestFetchAllActions_MalformedYAMLReturnsError(t *testing.T) {
	url := makeFixtureHub(t, fixtureDirs{
		actions: map[string]string{"bad.yaml": "this: is: not: valid: yaml: [["},
	})
	c := &hub.Client{URL: url}
	_, err := c.FetchAllActions(context.Background())
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected error to mention parse failure, got %v", err)
	}
}

func TestFetchActionByFQN_PropagatesUnderlyingFetchError(t *testing.T) {
	// When the Hub repo can't be cloned, FetchActionByFQN should bubble
	// up the fetch error rather than returning ErrNotFound (which would
	// mask the connectivity issue as a 404).
	c := &hub.Client{URL: "file:///nonexistent/path"}
	_, err := c.FetchActionByFQN(context.Background(), "github://anywhere/anything")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if err == hub.ErrNotFound {
		t.Fatalf("expected wrapped fetch error, got ErrNotFound")
	}
}

func TestFetchActionByFQN_ReturnsMatchingEntryAndErrNotFound(t *testing.T) {
	url := makeFixtureHub(t, fixtureDirs{
		actions: map[string]string{
			"github_alice_a.yaml": `
fqn: github://alice/conn/actions/a
description: Action A
publisher_github: alice
connector_fqn: github://alice/conn
`,
		},
	})
	c := &hub.Client{URL: url}

	got, err := c.FetchActionByFQN(context.Background(), "github://alice/conn/actions/a")
	if err != nil {
		t.Fatalf("FetchActionByFQN: %v", err)
	}
	if got.Description != "Action A" {
		t.Fatalf("unexpected entry: %+v", got)
	}

	_, err = c.FetchActionByFQN(context.Background(), "github://nobody/missing")
	if err != hub.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFetchAllSuites_ReturnsAllEntriesSortedByFQN(t *testing.T) {
	url := makeFixtureHub(t, fixtureDirs{
		suites: map[string]string{
			"github_alice_suite.yaml": `
fqn: github://alice/conn/suite
description: Alice's suite
publisher_github: alice
member_actions:
  - github://alice/conn/actions/a
  - github://alice/conn/actions/b
connectors_required:
  - github://alice/conn
category: communication
`,
		},
	})
	c := &hub.Client{URL: url}

	entries, err := c.FetchAllSuites(context.Background())
	if err != nil {
		t.Fatalf("FetchAllSuites: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if len(entries[0].MemberActions) != 2 {
		t.Fatalf("member_actions not parsed: %+v", entries[0])
	}
	if len(entries[0].ConnectorsRequired) != 1 || entries[0].ConnectorsRequired[0] != "github://alice/conn" {
		t.Fatalf("connectors_required not parsed: %+v", entries[0])
	}
}

func TestFetchAllSuites_UnreachableURLReturnsError(t *testing.T) {
	c := &hub.Client{URL: "file:///nonexistent/path/that/does/not/exist"}
	_, err := c.FetchAllSuites(context.Background())
	if err == nil {
		t.Fatalf("expected error for unreachable URL, got nil")
	}
	if !strings.Contains(err.Error(), "hub:") {
		t.Fatalf("expected wrapped error with 'hub:' prefix, got %v", err)
	}
}

func TestFetchAllSuites_MalformedYAMLReturnsError(t *testing.T) {
	url := makeFixtureHub(t, fixtureDirs{
		suites: map[string]string{"bad.yaml": "this: is: not: valid: yaml: [["},
	})
	c := &hub.Client{URL: url}
	_, err := c.FetchAllSuites(context.Background())
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected error to mention parse failure, got %v", err)
	}
}

func TestFetchSuiteByFQN_PropagatesUnderlyingFetchError(t *testing.T) {
	c := &hub.Client{URL: "file:///nonexistent/path"}
	_, err := c.FetchSuiteByFQN(context.Background(), "github://anywhere/anything/suite")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if err == hub.ErrNotFound {
		t.Fatalf("expected wrapped fetch error, got ErrNotFound")
	}
}

func TestFetchSuiteByFQN_ReturnsMatchingEntryAndErrNotFound(t *testing.T) {
	url := makeFixtureHub(t, fixtureDirs{
		suites: map[string]string{
			"github_alice_suite.yaml": `
fqn: github://alice/conn/suite
description: Alice's suite
publisher_github: alice
member_actions:
  - github://alice/conn/actions/a
`,
		},
	})
	c := &hub.Client{URL: url}

	got, err := c.FetchSuiteByFQN(context.Background(), "github://alice/conn/suite")
	if err != nil {
		t.Fatalf("FetchSuiteByFQN: %v", err)
	}
	if got.PublisherGithub != "alice" {
		t.Fatalf("unexpected entry: %+v", got)
	}

	_, err = c.FetchSuiteByFQN(context.Background(), "github://nobody/missing")
	if err != hub.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFilterConnectorsByKeyword_MatchesFQNAndDescription(t *testing.T) {
	entries := []hub.ConnectorEntry{
		{FQN: "github://alice/google", Description: "Google Workspace"},
		{FQN: "github://bob/slack", Description: "Slack messaging"},
		{FQN: "github://charlie/x", Description: "Random thing about google"},
	}
	got := hub.FilterConnectorsByKeyword(entries, "google")
	if len(got) != 2 {
		t.Fatalf("expected 2 google matches, got %d: %+v", len(got), got)
	}
	// Case-insensitive
	got = hub.FilterConnectorsByKeyword(entries, "SLACK")
	if len(got) != 1 || got[0].FQN != "github://bob/slack" {
		t.Fatalf("case-insensitive match failed: %+v", got)
	}
	// Empty keyword returns all
	got = hub.FilterConnectorsByKeyword(entries, "")
	if len(got) != 3 {
		t.Fatalf("empty keyword should return all, got %d", len(got))
	}
}

func TestFilterActionsByKeyword_MatchesFQNAndDescription(t *testing.T) {
	entries := []hub.ActionEntry{
		{FQN: "github://alice/conn/actions/draft-email", Description: "Draft a Gmail"},
		{FQN: "github://bob/conn/actions/post-slack", Description: "Post a Slack message"},
	}
	got := hub.FilterActionsByKeyword(entries, "gmail")
	if len(got) != 1 || got[0].FQN != "github://alice/conn/actions/draft-email" {
		t.Fatalf("expected single gmail match, got %+v", got)
	}
	got = hub.FilterActionsByKeyword(entries, "")
	if len(got) != 2 {
		t.Fatalf("empty keyword should return all, got %d", len(got))
	}
}

func TestFilterSuitesByKeyword_MatchesFQNAndDescription(t *testing.T) {
	entries := []hub.SuiteEntry{
		{FQN: "github://alice/conn/suite", Description: "Gmail and Calendar"},
		{FQN: "github://bob/conn/suite", Description: "Slack essentials"},
	}
	got := hub.FilterSuitesByKeyword(entries, "calendar")
	if len(got) != 1 || got[0].FQN != "github://alice/conn/suite" {
		t.Fatalf("expected single calendar match, got %+v", got)
	}
}

func TestPublisherFootprint_ReturnsSiblingFQNsExcludingSelf(t *testing.T) {
	entries := []hub.ConnectorEntry{
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

func TestFetchAllConnectors_MalformedYAMLReturnsError(t *testing.T) {
	url := connectorsOnly(t, map[string]string{
		"bad.yaml": "this: is: not: valid: yaml: at: all: [",
	})
	c := &hub.Client{URL: url}

	_, err := c.FetchAllConnectors(context.Background())
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected error to mention parse failure, got %v", err)
	}
}

func TestFetchAllConnectors_NoConnectorsDirReturnsEmpty(t *testing.T) {
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
	entries, err := c.FetchAllConnectors(context.Background())
	if err != nil {
		t.Fatalf("FetchAllConnectors: %v", err)
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
