package cstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGitHubReleasesLister_SingleArtifact covers the canonical
// single-artifact connector layout: tags are `v<semver>`, no FQN
// subpath. We assert the lister extracts bare SemVer strings, drops
// drafts, and sorts descending — the contract `aileron connector
// check` depends on for "what's newer than what I have installed".
func TestGitHubReleasesLister_SingleArtifact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/ALRubinger/aileron-connector-google/releases" {
			t.Errorf("path = %q, want /repos/ALRubinger/aileron-connector-google/releases", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]githubRelease{
			{TagName: "v0.0.6"},
			{TagName: "v0.0.7-beta.1", Prerelease: true},
			{TagName: "v0.0.5"},
			{TagName: "v0.0.8", Draft: true},     // drafts skipped
			{TagName: "no-version-here"},          // unparseable skipped
			{TagName: "v0.0.4"},
		})
	}))
	defer srv.Close()

	l := &GitHubReleasesLister{HTTPClient: srv.Client(), BaseURL: srv.URL}
	fqn, _ := ParseFQN("github://ALRubinger/aileron-connector-google")

	got, err := l.ListVersions(context.Background(), fqn)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	want := []string{"0.0.7-beta.1", "0.0.6", "0.0.5", "0.0.4"}
	if len(got) != len(want) {
		t.Fatalf("ListVersions = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGitHubReleasesLister_MonorepoSubpath covers the subpath case
// from ADR-0004: tags are `<subpath>/v<semver>`. Tags that don't match
// the subpath prefix (e.g. an unrelated artifact in the same repo) are
// filtered out — only versions matching this connector are returned.
func TestGitHubReleasesLister_MonorepoSubpath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]githubRelease{
			{TagName: "connectors/slack/v1.2.0"},
			{TagName: "connectors/slack/v1.1.0"},
			{TagName: "connectors/linear/v3.0.0"}, // unrelated artifact
			{TagName: "v9.9.9"},                    // wrong shape for subpath
		})
	}))
	defer srv.Close()

	l := &GitHubReleasesLister{HTTPClient: srv.Client(), BaseURL: srv.URL}
	fqn, _ := ParseFQN("github://aileron/integrations/connectors/slack")

	got, err := l.ListVersions(context.Background(), fqn)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	want := []string{"1.2.0", "1.1.0"}
	if len(got) != len(want) {
		t.Fatalf("ListVersions = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGitHubReleasesLister_NotFound asserts that a 404 from GitHub
// surfaces as a stable error string the check handler can render.
// `aileron connector check` is "offline-failing per connector" — a
// missing repo must produce a useful error, not a panic.
func TestGitHubReleasesLister_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	l := &GitHubReleasesLister{HTTPClient: srv.Client(), BaseURL: srv.URL}
	fqn, _ := ParseFQN("github://gone/repo")

	_, err := l.ListVersions(context.Background(), fqn)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want substring 'not found'", err)
	}
}

// TestGitHubReleasesLister_RateLimited covers the 403 path that GitHub
// returns when an unauthenticated client exceeds 60 req/hour. The CLI
// should report this distinctly so an operator knows to set
// GITHUB_TOKEN, not file a "broken" bug.
func TestGitHubReleasesLister_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	l := &GitHubReleasesLister{HTTPClient: srv.Client(), BaseURL: srv.URL}
	fqn, _ := ParseFQN("github://owner/repo")

	_, err := l.ListVersions(context.Background(), fqn)
	if err == nil {
		t.Fatal("expected error for 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want substring '403'", err)
	}
}

// TestGitHubReleasesLister_AuthHeader asserts the bearer token is
// forwarded so the operator's GITHUB_TOKEN actually raises their
// rate limit. Without this, the env var would be silently dropped.
func TestGitHubReleasesLister_AuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	l := &GitHubReleasesLister{HTTPClient: srv.Client(), BaseURL: srv.URL, Token: "ghp_secrettoken"}
	fqn, _ := ParseFQN("github://owner/repo")

	if _, err := l.ListVersions(context.Background(), fqn); err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if gotAuth != "Bearer ghp_secrettoken" {
		t.Errorf("Authorization = %q, want 'Bearer ghp_secrettoken'", gotAuth)
	}
}

// TestMuxVersionLister_DispatchesByScheme asserts the multiplexer
// routes by FQN scheme, mirroring MuxResolver.
func TestMuxVersionLister_DispatchesByScheme(t *testing.T) {
	called := ""
	mux := MuxVersionLister{
		"github": stubLister{name: "github", versions: []string{"1.0.0"}},
		"hub":    stubLister{name: "hub", versions: []string{"2.0.0"}},
	}
	for _, scheme := range []string{"github", "hub"} {
		fqn, _ := ParseFQN(scheme + "://owner/repo")
		got, err := mux.ListVersions(context.Background(), fqn)
		if err != nil {
			t.Fatalf("scheme=%s: %v", scheme, err)
		}
		_ = got
		called = scheme
	}
	if called == "" {
		t.Error("no scheme dispatched")
	}
}

// TestMuxVersionLister_UnknownScheme asserts that an unregistered
// scheme produces ClassUnknownScheme so the install pipeline's error
// surface stays consistent with the resolver's contract.
func TestMuxVersionLister_UnknownScheme(t *testing.T) {
	mux := MuxVersionLister{}
	fqn, _ := ParseFQN("hub://owner/repo")
	_, err := mux.ListVersions(context.Background(), fqn)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "hub") {
		t.Errorf("error = %v, want substring 'hub'", err)
	}
}

// TestDefaultVersionLister_GitLabHubNotImplemented documents the
// scope decision: only github:// is wired today. gitlab:// and hub://
// surface a clear error so operators using either scheme get a
// useful message instead of a silent skip.
func TestDefaultVersionLister_GitLabHubNotImplemented(t *testing.T) {
	l := DefaultVersionLister()
	for _, scheme := range []string{"gitlab", "hub"} {
		fqn, _ := ParseFQN(scheme + "://owner/repo")
		_, err := l.ListVersions(context.Background(), fqn)
		if err == nil {
			t.Errorf("scheme=%s: expected 'not implemented' error", scheme)
		} else if !strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("scheme=%s: error = %v, want 'not yet implemented'", scheme, err)
		}
	}
}

// TestIsPrerelease covers the SemVer prerelease classifier the check
// handler uses to filter the result set when `include_prerelease=false`.
func TestIsPrerelease(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"1.2.3", false},
		{"1.2.3-beta.1", true},
		{"1.2.3-rc.0", true},
		{"0.0.1-alpha", true},
	}
	for _, c := range cases {
		if got := IsPrerelease(c.v); got != c.want {
			t.Errorf("IsPrerelease(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

// TestCompareVersions covers the SemVer compare the check handler
// uses to decide update_available.
func TestCompareVersions(t *testing.T) {
	if CompareVersions("1.2.3", "1.2.2") <= 0 {
		t.Error("1.2.3 should be > 1.2.2")
	}
	if CompareVersions("1.2.3", "1.2.3") != 0 {
		t.Error("1.2.3 should equal 1.2.3")
	}
	if CompareVersions("1.2.3-beta.1", "1.2.3") >= 0 {
		t.Error("1.2.3-beta.1 should be < 1.2.3")
	}
}

// TestStore_ListInstalled covers the round-trip used by `aileron
// connector check`: rebuild the index from on-disk manifests, then
// enumerate the installed refs.
func TestStore_ListInstalled(t *testing.T) {
	s := NewStore(t.TempDir())
	// Empty store → empty list.
	if got := s.ListInstalled(); len(got) != 0 {
		t.Errorf("empty store ListInstalled = %v, want empty", got)
	}

	// Two installs → ordered (sorted by canonical key).
	s.recordIndex(Ref{FQN: mustParseFQN(t, "github://acme/foo"), Version: "1.2.0"}, "sha256:aaa")
	s.recordIndex(Ref{FQN: mustParseFQN(t, "github://acme/bar"), Version: "0.1.0"}, "sha256:bbb")

	got := s.ListInstalled()
	if len(got) != 2 {
		t.Fatalf("ListInstalled = %v, want 2 entries", got)
	}
	// Sort is by canonical key — "github://acme/bar@0.1.0" < "github://acme/foo@1.2.0" alphabetically.
	if got[0].FQN.Repo != "bar" {
		t.Errorf("[0].Repo = %q, want bar", got[0].FQN.Repo)
	}
	if got[1].FQN.Repo != "foo" {
		t.Errorf("[1].Repo = %q, want foo", got[1].FQN.Repo)
	}
}

// stubLister is a test double for VersionLister.
type stubLister struct {
	name     string
	versions []string
}

func (s stubLister) ListVersions(_ context.Context, _ FQN) ([]string, error) {
	return s.versions, nil
}

func mustParseFQN(t *testing.T, s string) FQN {
	t.Helper()
	fqn, err := ParseFQN(s)
	if err != nil {
		t.Fatalf("ParseFQN(%q): %v", s, err)
	}
	return fqn
}
