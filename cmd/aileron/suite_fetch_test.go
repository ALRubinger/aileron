package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// --- parseSuiteSource ---

func TestParseSuiteSource_HappyPaths(t *testing.T) {
	tests := []struct {
		raw     string
		wantFQN string
		wantRef string
	}{
		{
			raw:     "github://acme/conn/suite.toml@latest",
			wantFQN: "github://acme/conn/suite.toml",
			wantRef: "latest",
		},
		{
			raw:     "github://acme/conn/suite.toml@v0.0.6",
			wantFQN: "github://acme/conn/suite.toml",
			wantRef: "v0.0.6",
		},
		{
			raw:     "github://acme/conn/suites/full.toml@a1b2c3d4e5f6789012345678901234567890abcd",
			wantFQN: "github://acme/conn/suites/full.toml",
			wantRef: "a1b2c3d4e5f6789012345678901234567890abcd",
		},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			fqn, ref, err := parseSuiteSource(tc.raw)
			if err != nil {
				t.Fatalf("parseSuiteSource(%q) error: %v", tc.raw, err)
			}
			if got := fqn.String(); got != tc.wantFQN {
				t.Errorf("FQN = %q, want %q", got, tc.wantFQN)
			}
			if ref != tc.wantRef {
				t.Errorf("ref = %q, want %q", ref, tc.wantRef)
			}
		})
	}
}

func TestParseSuiteSource_RejectsMissingAtRef(t *testing.T) {
	_, _, err := parseSuiteSource("github://acme/conn/suite.toml")
	if err == nil {
		t.Fatal("expected error for missing @<ref>")
	}
	if !strings.Contains(err.Error(), "@<ref>") {
		t.Errorf("error should mention @<ref>; got: %v", err)
	}
}

func TestParseSuiteSource_RejectsEmptyRef(t *testing.T) {
	_, _, err := parseSuiteSource("github://acme/conn/suite.toml@")
	if err == nil {
		t.Fatal("expected error for empty ref")
	}
}

func TestParseSuiteSource_RejectsMissingFilePath(t *testing.T) {
	// github://owner/repo with no file path inside is meaningless
	// for a suite source — there's nothing to fetch.
	_, _, err := parseSuiteSource("github://acme/conn@latest")
	if err == nil {
		t.Fatal("expected error for missing file path")
	}
	if !strings.Contains(err.Error(), "file path") {
		t.Errorf("error should mention file path; got: %v", err)
	}
}

func TestParseSuiteSource_RejectsBadScheme(t *testing.T) {
	_, _, err := parseSuiteSource("nope://acme/conn/suite.toml@latest")
	if err == nil {
		t.Fatal("expected error for bad scheme")
	}
}

// --- resolveLatestRef ---

// stubGitHubAPI swaps githubAPIBase for the test's lifetime, so
// resolveLatestRef hits the supplied httptest server instead of
// production.
func stubGitHubAPI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = prev })
}

func TestResolveLatestRef_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/conn/releases/latest" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.0.6","name":"v0.0.6"}`))
	}))
	defer srv.Close()
	stubGitHubAPI(t, srv)

	ref, err := resolveLatestRef(context.Background(), cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"})
	if err != nil {
		t.Fatalf("resolveLatestRef: %v", err)
	}
	if ref != "v0.0.6" {
		t.Errorf("ref = %q, want v0.0.6", ref)
	}
}

func TestResolveLatestRef_NoReleasesIsClearError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	stubGitHubAPI(t, srv)

	_, err := resolveLatestRef(context.Background(), cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"})
	if err == nil {
		t.Fatal("expected error when no releases")
	}
	for _, want := range []string{"@<release-tag>", "@<sha>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got: %v", want, err)
		}
	}
}

func TestResolveLatestRef_RateLimitedSuggestsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	stubGitHubAPI(t, srv)

	_, err := resolveLatestRef(context.Background(), cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("error should suggest GITHUB_TOKEN; got: %v", err)
	}
}

func TestResolveLatestRef_RejectsNonGitHubScheme(t *testing.T) {
	_, err := resolveLatestRef(context.Background(), cstore.FQN{Scheme: "gitlab", Owner: "acme", Repo: "conn"})
	if err == nil {
		t.Fatal("expected error on non-github scheme")
	}
}

// --- fetchSuiteTOML ---

// stubRawGitHub swaps rawGitHubBase for the test's lifetime so
// fetchSuiteTOML hits the supplied httptest server. Mirrors the
// pattern used by withMockGitHubRaw for keyring tests.
func stubRawGitHub(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := rawGitHubBase
	rawGitHubBase = srv.URL
	t.Cleanup(func() { rawGitHubBase = prev })
}

func TestFetchSuiteTOML_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acme/conn/v0.0.6/suite.toml" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("name = \"x\"\n"))
	}))
	defer srv.Close()
	stubRawGitHub(t, srv)

	body, err := fetchSuiteTOML(context.Background(),
		cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn", Subpath: "suite.toml"},
		"v0.0.6",
	)
	if err != nil {
		t.Fatalf("fetchSuiteTOML: %v", err)
	}
	if !strings.Contains(string(body), `name = "x"`) {
		t.Errorf("body = %q", body)
	}
}

func TestFetchSuiteTOML_404IsClearError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	stubRawGitHub(t, srv)

	_, err := fetchSuiteTOML(context.Background(),
		cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn", Subpath: "suite.toml"},
		"v0.0.6",
	)
	if err == nil {
		t.Fatal("expected 404 error")
	}
	if !strings.Contains(err.Error(), "no suite manifest") {
		t.Errorf("error should mention no suite manifest; got: %v", err)
	}
}

// --- suiteVersionFromRef ---

func TestSuiteVersionFromRef(t *testing.T) {
	tests := []struct {
		ref     string
		want    string
		wantOK  bool
	}{
		{"v0.0.6", "0.0.6", true},
		{"v1.2.3-rc.1", "1.2.3-rc.1", true},
		{"0.0.6", "", false},                                          // missing v prefix
		{"v0.0", "", false},                                           // not strict semver (3 segments)
		{"a1b2c3d4e5f6789012345678901234567890abcd", "", false},       // sha, not semver
		{"main", "", false},                                           // branch name
	}
	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			got, ok := suiteVersionFromRef(tc.ref)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("suiteVersionFromRef(%q) = (%q, %v), want (%q, %v)",
					tc.ref, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
