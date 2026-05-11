// Package hub is the daemon's client for the public Aileron connector
// discovery Hub (ADR-0013).
//
// The Hub is a public GitHub repo at `aileron-connectors-hub` whose
// `connectors/` directory holds one YAML entry per community-published
// connector. Each entry points at the connector's canonical
// `github://OWNER/REPO` FQN — the Hub stores no binaries.
//
// Per #486, v0.x has no persisted cache and no `api.github.com` calls.
// Every Hub query shallow-clones the repo into a tmpdir, parses the
// entries, and discards the clone. A server-side metadata service
// that would re-introduce caching and popularity signals is tracked
// in #614.
package hub

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
	"gopkg.in/yaml.v3"
)

// Entry is one Hub connector listing. Matches the YAML files committed
// to `aileron-connectors-hub/connectors/*.yaml`. Field tags align with
// both the YAML files and the OpenAPI `HubConnectorEntry` schema.
type Entry struct {
	FQN             string `yaml:"fqn" json:"fqn"`
	Description     string `yaml:"description" json:"description"`
	PublisherGithub string `yaml:"publisher_github" json:"publisher_github"`
	KeyURL          string `yaml:"key_url" json:"key_url"`
	ReleasePattern  string `yaml:"release_pattern" json:"release_pattern"`
}

// ErrNotFound signals that a requested FQN has no matching Hub entry.
var ErrNotFound = errors.New("hub: entry not found")

// Client fetches entries from a configured Hub git URL.
//
// Concurrent calls are safe: each FetchAll clones into its own tmpdir.
type Client struct {
	// URL is a git-clonable Hub URL. file://, https://, and ssh URLs
	// all work — useful for tests that point at a local fixture repo.
	URL string

	// HTTP is used for fetching publisher key bytes from `key_url`.
	// nil means use http.DefaultClient.
	HTTP *http.Client

	// CloneTimeout bounds the shallow-clone subprocess. Zero means
	// 30 seconds.
	CloneTimeout time.Duration
}

// FetchAll shallow-clones the Hub repo, parses every YAML file under
// `connectors/`, and returns the entries sorted by FQN. The clone
// directory is deleted before return.
//
// A failed clone (network down, URL wrong, repo missing) returns a
// wrapped error suitable for surfacing as 503 Hub-unreachable.
func (c *Client) FetchAll(ctx context.Context) ([]Entry, error) {
	dir, err := os.MkdirTemp("", "aileron-hub-*")
	if err != nil {
		return nil, fmt.Errorf("hub: tmpdir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := c.shallowClone(ctx, dir); err != nil {
		return nil, err
	}

	connectorsDir := filepath.Join(dir, "connectors")
	files, err := os.ReadDir(connectorsDir)
	if err != nil {
		// A Hub repo with no connectors/ directory is treated as empty
		// rather than an error. The list endpoint returns an empty list;
		// the show/install-decision endpoints return ErrNotFound.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("hub: read connectors dir: %w", err)
	}

	var entries []Entry
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(connectorsDir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("hub: read %s: %w", name, err)
		}
		var e Entry
		if err := yaml.Unmarshal(b, &e); err != nil {
			return nil, fmt.Errorf("hub: parse %s: %w", name, err)
		}
		entries = append(entries, e)
	}

	sortEntries(entries)
	return entries, nil
}

// FetchByFQN returns the entry whose FQN matches exactly, or
// ErrNotFound if no such entry exists in the Hub.
func (c *Client) FetchByFQN(ctx context.Context, fqn string) (Entry, error) {
	entries, err := c.FetchAll(ctx)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if e.FQN == fqn {
			return e, nil
		}
	}
	return Entry{}, ErrNotFound
}

// FetchPublisherKey downloads the PEM-encoded ed25519 public key at
// keyURL and returns the parsed key plus its fingerprint string. The
// fingerprint format matches `aileron keyring trust` output:
// `sha256:<base64-no-padding, first 22 chars>`.
func (c *Client) FetchPublisherKey(ctx context.Context, keyURL string) (ed25519.PublicKey, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, keyURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("hub: build key request: %w", err)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("hub: fetch key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("hub: fetch key %s: %s", keyURL, resp.Status)
	}
	pemBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("hub: read key body: %w", err)
	}
	pub, err := cstore.ParsePEMPublicKey(pemBytes)
	if err != nil {
		return nil, "", fmt.Errorf("hub: parse key: %w", err)
	}
	return pub, Fingerprint(pub), nil
}

// Fingerprint formats a key's SHA-256 hash to match the
// `aileron keyring trust` output: `sha256:<22 chars base64-no-padding>`.
// Long enough to disambiguate keys at a glance, short enough to fit
// on a terminal line. Same shape as the canonical ssh-keygen idioms.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + base64.RawStdEncoding.EncodeToString(sum[:])[:22]
}

// shallowClone runs `git clone --depth 1 <URL> <dir>` with the given
// timeout. The dir must already exist and be empty (MkdirTemp gives
// an empty dir; git clones into a subpath of dir to avoid the "must
// be empty" check). The clone goes into dir/repo, and the caller
// reads from there via the connectorsDir path.
func (c *Client) shallowClone(ctx context.Context, dir string) error {
	timeout := c.CloneTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Clone directly into dir, not a subdir, because dir was just
	// created and is empty. `git clone <url> <empty-dir>` works.
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--single-branch", c.URL, dir)
	cmd.Env = append(os.Environ(),
		// Avoid prompting on auth failures. Public Hub is read-only
		// over HTTPS, so any password prompt indicates a bad URL or
		// network breakage — fail loud instead of hanging.
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hub: clone %s: %w: %s", c.URL, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sortEntries(entries []Entry) {
	// Stable alphabetical by FQN for deterministic list output across
	// runs. Callers (search filter, footprint computation) rely on
	// this only weakly, but tests pin it for reproducibility.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j-1].FQN > entries[j].FQN; j-- {
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}
}

// FilterByKeyword returns the subset of entries whose FQN or
// description contains q (substring, case-insensitive). An empty q
// returns all entries.
func FilterByKeyword(entries []Entry, q string) []Entry {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return entries
	}
	var out []Entry
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.FQN), q) ||
			strings.Contains(strings.ToLower(e.Description), q) {
			out = append(out, e)
		}
	}
	return out
}

// PublisherFootprint returns the FQNs of every entry by the same
// publisher as the supplied entry, excluding the entry's own FQN.
// Used to surface "publisher's other connectors" context in the
// install-decision payload (#487).
func PublisherFootprint(entries []Entry, e Entry) []string {
	var out []string
	for _, other := range entries {
		if other.PublisherGithub == e.PublisherGithub && other.FQN != e.FQN {
			out = append(out, other.FQN)
		}
	}
	return out
}
