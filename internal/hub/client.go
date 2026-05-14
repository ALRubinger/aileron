// Package hub is the daemon's client for the public Aileron discovery
// Hub (ADR-0013).
//
// The Hub is a public GitHub repo at `aileron-connectors-hub` whose
// catalog holds three entry types, one YAML pointer per published
// artifact:
//
//   - `connectors/*.yaml` — community-published connectors, each
//     pointing at the connector's canonical `github://OWNER/REPO` FQN.
//   - `actions/*.yaml` — published action templates, each pointing at
//     the action's canonical `github://OWNER/REPO/actions/NAME` FQN
//     and naming the connector it depends on.
//   - `suites/*.yaml` — published action suites, each enumerating its
//     member action FQNs plus the derived `connectors_required` list.
//
// Per #486, v0.x has no persisted cache and no `api.github.com` calls.
// Every Hub query shallow-clones the repo into a tmpdir, parses the
// entries for the requested directory, and discards the clone. A
// server-side metadata service that would re-introduce caching and
// popularity signals is tracked in #614.
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
	"sort"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
	"gopkg.in/yaml.v3"
)

// ConnectorEntry is one Hub connector listing. Matches the YAML files
// committed to `aileron-connectors-hub/connectors/*.yaml`. Field tags
// align with both the YAML files and the OpenAPI `HubConnectorEntry`
// schema.
type ConnectorEntry struct {
	FQN             string `yaml:"fqn" json:"fqn"`
	Description     string `yaml:"description" json:"description"`
	PublisherGithub string `yaml:"publisher_github" json:"publisher_github"`
	KeyURL          string `yaml:"key_url" json:"key_url"`
	ReleasePattern  string `yaml:"release_pattern" json:"release_pattern"`
}

// ActionEntry is one Hub action-template listing. Matches the YAML
// files committed to `aileron-connectors-hub/actions/*.yaml`. Action
// entries are discovery metadata; the canonical action template (TOML
// frontmatter + Markdown body per ADR-0003) is fetched from the
// publisher's repo at install time, not from the Hub.
type ActionEntry struct {
	FQN             string   `yaml:"fqn" json:"fqn"`
	Description     string   `yaml:"description" json:"description"`
	PublisherGithub string   `yaml:"publisher_github" json:"publisher_github"`
	ConnectorFQN    string   `yaml:"connector_fqn" json:"connector_fqn"`
	Intents         []string `yaml:"intents,omitempty" json:"intents,omitempty"`
	Category        string   `yaml:"category,omitempty" json:"category,omitempty"`
}

// SuiteEntry is one Hub action-suite listing. Matches the YAML files
// committed to `aileron-connectors-hub/suites/*.yaml`. A suite entry
// names its member actions and the connectors those actions transitively
// require. The authoritative `suite.toml` (per #564) still lives in the
// publisher's repo; the Hub entry is the discovery pointer.
type SuiteEntry struct {
	FQN                string   `yaml:"fqn" json:"fqn"`
	Description        string   `yaml:"description" json:"description"`
	PublisherGithub    string   `yaml:"publisher_github" json:"publisher_github"`
	MemberActions      []string `yaml:"member_actions" json:"member_actions"`
	ConnectorsRequired []string `yaml:"connectors_required,omitempty" json:"connectors_required,omitempty"`
	Category           string   `yaml:"category,omitempty" json:"category,omitempty"`
}

// ErrNotFound signals that a requested FQN has no matching Hub entry
// in the requested catalog (connectors, actions, or suites).
var ErrNotFound = errors.New("hub: entry not found")

// Client fetches entries from a configured Hub git URL.
//
// Concurrent calls are safe: each Fetch* shallow-clones into its own
// tmpdir.
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

// FetchAllConnectors shallow-clones the Hub repo, parses every YAML
// file under `connectors/`, and returns the entries sorted by FQN.
// The clone directory is deleted before return.
//
// A failed clone (network down, URL wrong, repo missing) returns a
// wrapped error suitable for surfacing as 503 Hub-unreachable.
func (c *Client) FetchAllConnectors(ctx context.Context) ([]ConnectorEntry, error) {
	return fetchAll[ConnectorEntry](ctx, c, "connectors")
}

// FetchAllActions shallow-clones the Hub repo, parses every YAML file
// under `actions/`, and returns the entries sorted by FQN.
func (c *Client) FetchAllActions(ctx context.Context) ([]ActionEntry, error) {
	return fetchAll[ActionEntry](ctx, c, "actions")
}

// FetchAllSuites shallow-clones the Hub repo, parses every YAML file
// under `suites/`, and returns the entries sorted by FQN.
func (c *Client) FetchAllSuites(ctx context.Context) ([]SuiteEntry, error) {
	return fetchAll[SuiteEntry](ctx, c, "suites")
}

// FetchConnectorByFQN returns the connector entry whose FQN matches
// exactly, or ErrNotFound.
func (c *Client) FetchConnectorByFQN(ctx context.Context, fqn string) (ConnectorEntry, error) {
	return fetchByFQN(ctx, c.FetchAllConnectors, fqn, func(e ConnectorEntry) string { return e.FQN })
}

// FetchActionByFQN returns the action entry whose FQN matches exactly,
// or ErrNotFound.
func (c *Client) FetchActionByFQN(ctx context.Context, fqn string) (ActionEntry, error) {
	return fetchByFQN(ctx, c.FetchAllActions, fqn, func(e ActionEntry) string { return e.FQN })
}

// FetchSuiteByFQN returns the suite entry whose FQN matches exactly,
// or ErrNotFound.
func (c *Client) FetchSuiteByFQN(ctx context.Context, fqn string) (SuiteEntry, error) {
	return fetchByFQN(ctx, c.FetchAllSuites, fqn, func(e SuiteEntry) string { return e.FQN })
}

// fetchAll is the generic per-directory walker. T is the entry type;
// subdir is the directory under the Hub repo root to walk
// ("connectors", "actions", "suites"). Returns entries sorted by FQN.
func fetchAll[T any](ctx context.Context, c *Client, subdir string) ([]T, error) {
	dir, err := os.MkdirTemp("", "aileron-hub-*")
	if err != nil {
		return nil, fmt.Errorf("hub: tmpdir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := c.shallowClone(ctx, dir); err != nil {
		return nil, err
	}

	target := filepath.Join(dir, subdir)
	files, err := os.ReadDir(target)
	if err != nil {
		// A Hub repo with no subdir is treated as empty rather than an
		// error. The list endpoints return an empty list; the
		// show/install-decision endpoints return ErrNotFound.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("hub: read %s dir: %w", subdir, err)
	}

	var entries []T
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(target, name)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("hub: read %s: %w", name, err)
		}
		var e T
		if err := yaml.Unmarshal(b, &e); err != nil {
			return nil, fmt.Errorf("hub: parse %s: %w", name, err)
		}
		entries = append(entries, e)
	}

	sort.Slice(entries, func(i, j int) bool {
		return fqnOf(entries[i]) < fqnOf(entries[j])
	})
	return entries, nil
}

// fqnOf extracts the FQN field from any of the entry types. The three
// entry structs all carry a `FQN string` field; this helper bridges the
// generic fetchAll/sort without forcing each entry type to implement an
// interface. Adding a fourth entry type requires extending the switch.
func fqnOf(e any) string {
	switch v := e.(type) {
	case ConnectorEntry:
		return v.FQN
	case ActionEntry:
		return v.FQN
	case SuiteEntry:
		return v.FQN
	default:
		return ""
	}
}

// fetchByFQN runs a list-fetcher and linearly scans for the requested
// FQN. ErrNotFound when no entry matches. The function-pointer form
// lets the three public Fetch*ByFQN methods share the same scan loop
// without a per-type implementation.
func fetchByFQN[T any](
	ctx context.Context,
	fetchAll func(context.Context) ([]T, error),
	fqn string,
	getFQN func(T) string,
) (T, error) {
	var zero T
	entries, err := fetchAll(ctx)
	if err != nil {
		return zero, err
	}
	for _, e := range entries {
		if getFQN(e) == fqn {
			return e, nil
		}
	}
	return zero, ErrNotFound
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

// FilterConnectorsByKeyword returns the subset of entries whose FQN or
// description contains q (substring, case-insensitive). An empty q
// returns all entries.
func FilterConnectorsByKeyword(entries []ConnectorEntry, q string) []ConnectorEntry {
	return filterByKeyword(entries, q,
		func(e ConnectorEntry) string { return e.FQN },
		func(e ConnectorEntry) string { return e.Description })
}

// FilterActionsByKeyword returns the subset of action entries whose FQN
// or description contains q (substring, case-insensitive). Empty q
// returns all entries.
func FilterActionsByKeyword(entries []ActionEntry, q string) []ActionEntry {
	return filterByKeyword(entries, q,
		func(e ActionEntry) string { return e.FQN },
		func(e ActionEntry) string { return e.Description })
}

// FilterSuitesByKeyword returns the subset of suite entries whose FQN
// or description contains q (substring, case-insensitive). Empty q
// returns all entries.
func FilterSuitesByKeyword(entries []SuiteEntry, q string) []SuiteEntry {
	return filterByKeyword(entries, q,
		func(e SuiteEntry) string { return e.FQN },
		func(e SuiteEntry) string { return e.Description })
}

func filterByKeyword[T any](entries []T, q string, fqn, desc func(T) string) []T {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return entries
	}
	var out []T
	for _, e := range entries {
		if strings.Contains(strings.ToLower(fqn(e)), q) ||
			strings.Contains(strings.ToLower(desc(e)), q) {
			out = append(out, e)
		}
	}
	return out
}

// PublisherFootprint returns the FQNs of every connector entry by the
// same publisher as the supplied entry, excluding the entry's own FQN.
// Used to surface "publisher's other connectors" context in the
// install-decision payload (#487). Trust-state semantics are
// connector-scoped, so this helper stays connector-typed.
func PublisherFootprint(entries []ConnectorEntry, e ConnectorEntry) []string {
	var out []string
	for _, other := range entries {
		if other.PublisherGithub == e.PublisherGithub && other.FQN != e.FQN {
			out = append(out, other.FQN)
		}
	}
	return out
}
