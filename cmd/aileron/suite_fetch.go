package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// suite_fetch.go: helpers for `aileron action add-suite` against a
// remote `<scheme>://<owner>/<repo>/<file-path>@<ref>` source. Local
// (filesystem) manifests bypass everything here — see
// runActionAddSuiteLocal in main.go.
//
// Three operations:
//
//   - parseSuiteSource splits the user's argument into FQN + raw ref
//     and validates the ref shape.
//   - resolveLatestRef turns a raw ref of "latest" into a concrete
//     git tag via the GitHub releases API.
//   - fetchSuiteTOML pulls the file's bytes from raw.githubusercontent.com.
//
// Test seams: rawGitHubBase (defined in keyring.go) and githubAPIBase
// (defined here) are package-level vars so httptest servers can stub
// either GitHub surface.

// githubAPIBase is the base URL for GitHub's REST API. Overridable
// in tests so resolveLatestRef can be exercised without hitting
// production. Mirrors rawGitHubBase in keyring.go.
var githubAPIBase = "https://api.github.com"

// suiteFetchTimeout caps the per-request timeout for both the
// releases API call and the raw TOML fetch. 15s matches the
// publisherKeyFetchTimeout pattern: enough for slow networks,
// short enough that a stalled host doesn't leave the user staring
// at a quiet terminal.
const suiteFetchTimeout = 15 * time.Second

// suiteTOMLMaxBytes caps the suite manifest at 64 KB. Real suite
// manifests are a few KB; the cap defends against a misconfigured
// server returning megabytes. Mirrors fetchPublisherKey's 64 KB cap.
const suiteTOMLMaxBytes = 64 * 1024

// reSHA matches a 40-character lowercase hex SHA, the form
// returned by `git rev-parse HEAD`. Short SHAs (7+ chars) are NOT
// accepted — full SHAs are unambiguous, abbreviated ones can
// collide between branches.
var reSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// parseSuiteSource splits a user-supplied `<FQN-URL>@<ref>` into
// the FQN half (file location) and the raw ref string. The split
// is on the LAST `@` so tags containing `@` (rare but legal) survive,
// the same way cstore.ParseRef does it.
//
// Validates that the FQN parses as an Aileron FQN and that the ref
// is one of:
//   - the literal `latest`
//   - a 40-char lowercase hex SHA
//   - any non-empty tag string
//
// The ref is NOT resolved here — `latest` survives as `latest` and
// gets resolved by resolveLatestRef when the caller is ready.
func parseSuiteSource(raw string) (cstore.FQN, string, error) {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return cstore.FQN{}, "", fmt.Errorf("source %q: missing @<ref> suffix (use @latest, @<release-tag>, or @<sha>)", raw)
	}
	fqnStr := raw[:at]
	ref := raw[at+1:]
	if ref == "" {
		return cstore.FQN{}, "", fmt.Errorf("source %q: empty ref after @", raw)
	}
	fqn, err := cstore.ParseFQN(fqnStr)
	if err != nil {
		return cstore.FQN{}, "", fmt.Errorf("source %q: %w", raw, err)
	}
	if fqn.Subpath == "" {
		return cstore.FQN{}, "", fmt.Errorf("source %q: missing file path after the repo (e.g. /suite.toml)", raw)
	}
	// Ref shape: latest, 40-hex SHA, or any non-empty tag. We don't
	// reject "weird" tag names because GitHub allows a wide range
	// (slashes, dots, dashes, underscores). The actual tag
	// existence is verified by the raw fetch downstream.
	if ref != "latest" && !reSHA.MatchString(ref) {
		// Anything else is treated as a tag. No further validation.
		_ = ref
	}
	return fqn, ref, nil
}

// resolveLatestRef calls GitHub's `releases/latest` endpoint and
// returns the `tag_name`. This is the most-recent non-draft, non-
// prerelease release; users who want prereleases pin via @<tag>.
//
// Honors GITHUB_TOKEN for higher rate limits; otherwise the call
// is anonymous (60 req/hour/IP, plenty for casual use).
//
// Failure modes:
//   - 404 (no releases or repo missing) → structured error pointing
//     at the @<tag> / @<sha> alternatives.
//   - 403 / 429 (rate-limited) → structured error suggesting
//     GITHUB_TOKEN.
//   - Network / decode failure → wrapped error.
func resolveLatestRef(ctx context.Context, fqn cstore.FQN) (string, error) {
	if fqn.Scheme != "github" {
		return "", fmt.Errorf("@latest is only supported for github:// sources (got %s://)", fqn.Scheme)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases/latest",
		strings.TrimRight(githubAPIBase, "/"),
		url.PathEscape(fqn.Owner),
		url.PathEscape(fqn.Repo),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: suiteFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound:
		return "", fmt.Errorf("no published releases for %s/%s — pin with @<release-tag> or @<sha> instead of @latest",
			fqn.Owner, fqn.Repo)
	case http.StatusForbidden, http.StatusTooManyRequests:
		return "", fmt.Errorf("github releases API rate-limited (HTTP %d) — set GITHUB_TOKEN to raise the limit",
			resp.StatusCode)
	default:
		return "", fmt.Errorf("github releases API returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode releases response: %w", err)
	}
	if body.TagName == "" {
		return "", fmt.Errorf("github releases API returned empty tag_name")
	}
	return body.TagName, nil
}

// fetchSuiteTOML pulls the suite manifest's bytes from
// raw.githubusercontent.com using the supplied (already-resolved)
// ref. Caps the response at suiteTOMLMaxBytes; an oversize body
// is treated as a malformed manifest.
//
// Anonymous fetch — public files don't need auth, and adding a
// token here would surprise users who set GITHUB_TOKEN expecting
// it to apply only to API calls.
func fetchSuiteTOML(ctx context.Context, fqn cstore.FQN, ref string) ([]byte, error) {
	if fqn.Scheme != "github" {
		return nil, fmt.Errorf("remote suite fetch is only supported for github:// sources (got %s://)", fqn.Scheme)
	}
	endpoint := fmt.Sprintf("%s/%s/%s/%s/%s",
		strings.TrimRight(rawGitHubBase, "/"),
		url.PathEscape(fqn.Owner),
		url.PathEscape(fqn.Repo),
		url.PathEscape(ref),
		fqn.Subpath, // intentionally NOT escaped: subpath includes legitimate `/`
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	client := &http.Client{Timeout: suiteFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no suite manifest at %s — check the file path and ref", endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, suiteTOMLMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read suite manifest: %w", err)
	}
	if len(body) > suiteTOMLMaxBytes {
		return nil, fmt.Errorf("suite manifest at %s exceeds %d byte cap", endpoint, suiteTOMLMaxBytes)
	}
	return body, nil
}

// suiteVersionFromRef converts a resolved git ref (e.g. "v0.0.6")
// into a bare SemVer string ("0.0.6") suitable for path-form
// inheritance. Returns ok=false when the ref isn't a v-prefixed
// SemVer — typical for raw SHAs, where path-form inheritance can't
// happen.
//
// Single-artifact connectors release with `v<semver>` tags per
// ADR-0004. Monorepo connectors with subpath-prefixed tags
// (`<sub>/v<semver>`) are out of v1 scope; users on those layouts
// pin with FQN-form entries.
func suiteVersionFromRef(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "v") {
		return "", false
	}
	bare := strings.TrimPrefix(ref, "v")
	if !cstore.IsStrictSemver(bare) {
		return "", false
	}
	return bare, true
}
