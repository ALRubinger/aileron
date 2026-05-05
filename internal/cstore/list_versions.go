package cstore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// VersionLister returns the SemVer versions a connector source has
// published. Per ADR-0004 §"connector check", the resolver layer is the
// only place that knows how to talk to a given source — listing
// available versions belongs alongside ResolveTarball.
//
// The contract:
//
//   - Returned versions are bare SemVer strings (no "v" prefix), the
//     same form callers pass to install.
//   - Pre-release filtering is the *caller's* responsibility — the
//     lister returns everything the source has, including pre-releases.
//     This keeps the resolver source-agnostic; the prerelease decision
//     is one place (the check handler) rather than per-implementation.
//   - Network/auth/parse failures are returned as-is; the check handler
//     surfaces them as per-connector errors so a single bad source
//     doesn't abort the whole sweep.
type VersionLister interface {
	ListVersions(ctx context.Context, fqn FQN) ([]string, error)
}

// MuxVersionLister dispatches by FQN scheme. Mirrors MuxResolver.
type MuxVersionLister map[string]VersionLister

// ListVersions implements VersionLister.
func (m MuxVersionLister) ListVersions(ctx context.Context, fqn FQN) ([]string, error) {
	l, ok := m[fqn.Scheme]
	if !ok {
		return nil, newError(ClassUnknownScheme, BoundaryRuntime, false,
			"version lister has no implementation for scheme %q", fqn.Scheme)
	}
	return l.ListVersions(ctx, fqn)
}

// DefaultVersionLister returns a multiplexing VersionLister that knows
// how to list versions for `github://`. `gitlab://` and `hub://` return
// a clear "not implemented" error — when the first connector using
// either scheme lands in the field, the matching lister can be wired
// here without touching call sites.
func DefaultVersionLister() VersionLister {
	return MuxVersionLister{
		"github": &GitHubReleasesLister{HTTPClient: defaultHTTPClient(), Token: os.Getenv("GITHUB_TOKEN")},
		"gitlab": notImplementedLister{scheme: "gitlab"},
		"hub":    notImplementedLister{scheme: "hub"},
	}
}

// notImplementedLister is a placeholder that returns a structured
// error. Lets the rest of the check pipeline run cleanly when an
// installed connector uses a scheme whose lister hasn't been wired yet.
type notImplementedLister struct{ scheme string }

func (n notImplementedLister) ListVersions(_ context.Context, _ FQN) ([]string, error) {
	return nil, fmt.Errorf("version listing for %q scheme is not yet implemented", n.scheme)
}

// GitHubReleasesLister queries GitHub's `/repos/{owner}/{repo}/releases`
// API and extracts SemVer versions from the release tags. For a
// single-artifact connector (no FQN subpath), the tag pattern is
// `v<semver>`; for a monorepo connector with subpath, the pattern is
// `<subpath>/v<semver>`. Both are produced by the same release flow
// the install pipeline already understands per ADR-0004.
//
// HTTPClient is injectable so tests can stub it; production uses a
// short-timeout client. Token, when set, is sent as a `Bearer` auth
// header — GitHub's anonymous rate limit (60 req/hour/IP) is enough
// for casual use but a token bumps it to 5,000/hour.
type GitHubReleasesLister struct {
	HTTPClient *http.Client
	Token      string

	// BaseURL overrides the GitHub API root for tests. Empty falls
	// back to the canonical https://api.github.com root.
	BaseURL string
}

// githubRelease is the subset of GitHub's release object we care about.
type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// ListVersions implements VersionLister. Returns every release tag
// that successfully parses to a SemVer for this FQN, sorted in
// descending SemVer order. Skips drafts. Pre-releases are kept; the
// caller decides whether to surface them.
func (g *GitHubReleasesLister) ListVersions(ctx context.Context, fqn FQN) ([]string, error) {
	base := g.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}

	// per_page=100 is GitHub's max. v0.x doesn't paginate — 100 most
	// recent releases is plenty for the operator's "is there a newer
	// version" question. If a connector has more than 100 releases,
	// older ones won't surface in `aileron connector check`; that's
	// acceptable since the check is interested in *newer* versions.
	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=100",
		strings.TrimRight(base, "/"),
		url.PathEscape(fqn.Owner),
		url.PathEscape(fqn.Repo))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("github releases: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}

	client := g.HTTPClient
	if client == nil {
		client = defaultHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Repo doesn't exist or is private — surface a stable error so
		// the check handler can show a useful message.
		return nil, fmt.Errorf("github releases: repository %s/%s not found", fqn.Owner, fqn.Repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases: HTTP %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("github releases: decode: %w", err)
	}

	versions := extractGitTagVersions(releases, fqn.Subpath)
	semverSortDesc(versions)
	return versions, nil
}

// extractGitTagVersions filters and parses release tags into bare
// SemVer strings, applying the monorepo subpath rule per ADR-0004:
//
//   - subpath == "":  tag is "v<semver>"            → strip leading "v"
//   - subpath != "":  tag is "<subpath>/v<semver>"  → strip prefix + "v"
//
// Drafts are skipped. Tags that don't match the pattern are skipped.
// Tags whose remaining version isn't valid SemVer are skipped.
func extractGitTagVersions(releases []githubRelease, subpath string) []string {
	prefix := "v"
	if subpath != "" {
		prefix = subpath + "/v"
	}

	out := make([]string, 0, len(releases))
	for _, r := range releases {
		if r.Draft {
			continue
		}
		if !strings.HasPrefix(r.TagName, prefix) {
			continue
		}
		v := strings.TrimPrefix(r.TagName, prefix)
		if !semver.IsValid("v" + v) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// semverSortDesc sorts a slice of bare SemVer strings (no "v" prefix)
// in descending order. The input is mutated.
func semverSortDesc(versions []string) {
	sort.Slice(versions, func(i, j int) bool {
		return semver.Compare("v"+versions[i], "v"+versions[j]) > 0
	})
}

// IsPrerelease reports whether v carries a SemVer pre-release suffix
// (e.g. "1.2.3-beta.1"). Detached from the source so the check
// handler can filter consistently regardless of where the version
// came from.
func IsPrerelease(v string) bool {
	return semver.Prerelease("v"+v) != ""
}

// CompareVersions returns -1, 0, or 1 if a is older than, equal to, or
// newer than b. Both are bare SemVer strings.
func CompareVersions(a, b string) int {
	return semver.Compare("v"+a, "v"+b)
}

// defaultHTTPClient is the production HTTP client for resolver lookups.
// The 10s timeout keeps `aileron connector check` snappy when a source
// is slow or unreachable — the handler runs the lookups serially today
// (parallelism is a Phase-2 concern), so each connector's check is
// bounded by this timeout.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}
