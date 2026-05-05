package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// fakeLister is a test double for cstore.VersionLister. Each FQN can
// return its own versions or its own error to simulate per-connector
// success and failure independently.
type fakeLister struct {
	byFQN map[string]fakeListerEntry
}

type fakeListerEntry struct {
	versions []string
	err      error
}

func (f *fakeLister) ListVersions(_ context.Context, fqn cstore.FQN) ([]string, error) {
	e, ok := f.byFQN[fqn.String()]
	if !ok {
		return nil, errors.New("no fixture for " + fqn.String())
	}
	return e.versions, e.err
}

// newStoreWithRefs builds a cstore.Store rooted at a fresh temp
// directory and pre-populates its on-disk index with the given
// (FQN, version) pairs. Used for handler tests that only need the
// index — not real tarballs — to exercise ListInstalled.
func newStoreWithRefs(t *testing.T, refs ...string) *cstore.Store {
	t.Helper()
	root := t.TempDir()

	if len(refs) > 0 {
		indexDir := filepath.Join(root, "index")
		if err := os.MkdirAll(indexDir, 0o755); err != nil {
			t.Fatalf("mkdir index: %v", err)
		}
		// Write an index file in the canonical "<FQN>@<version>": "sha256:..."
		// shape. The store loads this verbatim via LoadIndex.
		var b strings.Builder
		b.WriteString("{\n")
		for i, ref := range refs {
			fmt.Fprintf(&b, "  %q: %q", ref, "sha256:deadbeef")
			if i != len(refs)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString("}\n")
		if err := os.WriteFile(filepath.Join(indexDir, "connectors.json"), []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write index: %v", err)
		}
	}

	store := cstore.NewStore(root)
	if err := store.LoadIndex(); err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	return store
}

// TestCheckConnectors_BareServerEmptyResults covers the dev / no-config
// path: the apiServer struct exists but no installer is wired. The
// endpoint must still return 200 with `{"results": []}` rather than
// panicking. Same robustness contract as `/v1/status` on a bare
// server.
func TestCheckConnectors_BareServerEmptyResults(t *testing.T) {
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	srv.CheckConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/connectors/check", nil), api.CheckConnectorsParams{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got api.ConnectorsCheckResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 0 {
		t.Errorf("Results = %v, want empty", got.Results)
	}
}

// TestCheckConnectors_UpdateAvailable covers the operator's primary
// signal: installed `0.0.5`, source has `0.0.6` → update_available=true,
// latest_version="0.0.6", available_versions=["0.0.6"]. This is the
// payload the CLI uses to decide whether to render "update available".
func TestCheckConnectors_UpdateAvailable(t *testing.T) {
	store := newStoreWithRefs(t, "github://ALRubinger/aileron-connector-google@0.0.5")

	lister := &fakeLister{byFQN: map[string]fakeListerEntry{
		"github://ALRubinger/aileron-connector-google": {versions: []string{"0.0.6", "0.0.5", "0.0.4"}},
	}}

	srv := &apiServer{
		installer:     &cstore.Installer{Store: store},
		versionLister: lister,
	}
	rec := httptest.NewRecorder()
	srv.CheckConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/connectors/check", nil), api.CheckConnectorsParams{})

	var got api.ConnectorsCheckResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)

	if len(got.Results) != 1 {
		t.Fatalf("Results = %v, want 1 entry", got.Results)
	}
	r := got.Results[0]
	if !r.UpdateAvailable {
		t.Error("UpdateAvailable = false, want true")
	}
	if r.LatestVersion == nil || *r.LatestVersion != "0.0.6" {
		t.Errorf("LatestVersion = %v, want pointer to 0.0.6", r.LatestVersion)
	}
	if r.AvailableVersions == nil || len(*r.AvailableVersions) != 1 || (*r.AvailableVersions)[0] != "0.0.6" {
		t.Errorf("AvailableVersions = %v, want [0.0.6]", r.AvailableVersions)
	}
	if r.CurrentVersion != "0.0.5" {
		t.Errorf("CurrentVersion = %q, want 0.0.5", r.CurrentVersion)
	}
}

// TestCheckConnectors_UpToDate covers the happy path where the
// operator already has the newest stable: latest_version is set, but
// update_available is false and available_versions is omitted (no
// newer versions to install). The CLI uses this to render "up to date".
func TestCheckConnectors_UpToDate(t *testing.T) {
	store := newStoreWithRefs(t, "github://acme/connector@1.2.0")

	lister := &fakeLister{byFQN: map[string]fakeListerEntry{
		"github://acme/connector": {versions: []string{"1.2.0", "1.1.0", "1.0.0"}},
	}}

	srv := &apiServer{
		installer:     &cstore.Installer{Store: store},
		versionLister: lister,
	}
	rec := httptest.NewRecorder()
	srv.CheckConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/connectors/check", nil), api.CheckConnectorsParams{})

	var got api.ConnectorsCheckResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)

	r := got.Results[0]
	if r.UpdateAvailable {
		t.Error("UpdateAvailable = true, want false (already on latest)")
	}
	if r.LatestVersion == nil || *r.LatestVersion != "1.2.0" {
		t.Errorf("LatestVersion = %v, want pointer to 1.2.0", r.LatestVersion)
	}
	if r.AvailableVersions != nil {
		t.Errorf("AvailableVersions = %v, want omitted", *r.AvailableVersions)
	}
}

// TestCheckConnectors_PrereleaseExcludedByDefault asserts that
// pre-releases (1.3.0-beta.1) are not considered when the request
// doesn't set include_prerelease. Operators on stable should not see
// "update available" for a beta they didn't ask for.
func TestCheckConnectors_PrereleaseExcludedByDefault(t *testing.T) {
	store := newStoreWithRefs(t, "github://acme/connector@1.2.0")

	lister := &fakeLister{byFQN: map[string]fakeListerEntry{
		"github://acme/connector": {versions: []string{"1.3.0-beta.1", "1.2.0", "1.1.0"}},
	}}

	srv := &apiServer{
		installer:     &cstore.Installer{Store: store},
		versionLister: lister,
	}
	rec := httptest.NewRecorder()
	srv.CheckConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/connectors/check", nil), api.CheckConnectorsParams{})

	var got api.ConnectorsCheckResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)

	r := got.Results[0]
	if r.UpdateAvailable {
		t.Error("UpdateAvailable = true, want false (only beta is newer)")
	}
	if r.LatestVersion == nil || *r.LatestVersion != "1.2.0" {
		t.Errorf("LatestVersion = %v, want pointer to 1.2.0 (latest stable)", r.LatestVersion)
	}
}

// TestCheckConnectors_PrereleaseIncludedWhenRequested asserts the
// `include_prerelease=true` flag flips the behavior: betas now count
// as candidates for `latest_version` and `update_available`. This is
// the path the CLI's `--include-prerelease` flag drives.
func TestCheckConnectors_PrereleaseIncludedWhenRequested(t *testing.T) {
	store := newStoreWithRefs(t, "github://acme/connector@1.2.0")

	lister := &fakeLister{byFQN: map[string]fakeListerEntry{
		"github://acme/connector": {versions: []string{"1.3.0-beta.1", "1.2.0", "1.1.0"}},
	}}

	srv := &apiServer{
		installer:     &cstore.Installer{Store: store},
		versionLister: lister,
	}
	rec := httptest.NewRecorder()
	include := true
	srv.CheckConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/connectors/check", nil), api.CheckConnectorsParams{IncludePrerelease: &include})

	var got api.ConnectorsCheckResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)

	r := got.Results[0]
	if !r.UpdateAvailable {
		t.Error("UpdateAvailable = false, want true (beta is newer)")
	}
	if r.LatestVersion == nil || *r.LatestVersion != "1.3.0-beta.1" {
		t.Errorf("LatestVersion = %v, want pointer to 1.3.0-beta.1", r.LatestVersion)
	}
}

// TestCheckConnectors_PerConnectorErrorDoesNotAbort is the ADR-0004
// "offline-failing per connector" contract: connector A's source is
// unreachable, connector B's source is fine. The response must
// include both — A with `error` set, B with normal results.
func TestCheckConnectors_PerConnectorErrorDoesNotAbort(t *testing.T) {
	store := newStoreWithRefs(t, "github://acme/down@1.0.0", "github://acme/up@1.0.0")

	lister := &fakeLister{byFQN: map[string]fakeListerEntry{
		"github://acme/down": {err: errors.New("dial tcp: connection refused")},
		"github://acme/up":   {versions: []string{"1.1.0", "1.0.0"}},
	}}

	srv := &apiServer{
		installer:     &cstore.Installer{Store: store},
		versionLister: lister,
	}
	rec := httptest.NewRecorder()
	srv.CheckConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/connectors/check", nil), api.CheckConnectorsParams{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (per-connector errors don't abort)", rec.Code)
	}
	var got api.ConnectorsCheckResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)

	if len(got.Results) != 2 {
		t.Fatalf("Results = %v, want 2 entries", got.Results)
	}
	// Order is by canonical key: "github://acme/down" < "github://acme/up".
	if got.Results[0].Error == nil {
		t.Error("expected error on first result (down)")
	}
	if got.Results[1].Error != nil {
		t.Errorf("unexpected error on second result (up): %v", *got.Results[1].Error)
	}
	if !got.Results[1].UpdateAvailable {
		t.Error("expected UpdateAvailable=true on the working connector")
	}
}

// TestCheckConnectors_NoNewerVersionsButLatestPopulated covers the
// "you're on a non-latest stable" case: installed 1.0.0, source has
// 1.0.0 only. Latest is 1.0.0, no newer versions, no update available.
func TestCheckConnectors_NoNewerVersionsButLatestPopulated(t *testing.T) {
	store := newStoreWithRefs(t, "github://acme/connector@1.0.0")

	lister := &fakeLister{byFQN: map[string]fakeListerEntry{
		"github://acme/connector": {versions: []string{"1.0.0"}},
	}}

	srv := &apiServer{
		installer:     &cstore.Installer{Store: store},
		versionLister: lister,
	}
	rec := httptest.NewRecorder()
	srv.CheckConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/connectors/check", nil), api.CheckConnectorsParams{})

	var got api.ConnectorsCheckResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	r := got.Results[0]
	if r.UpdateAvailable {
		t.Error("UpdateAvailable = true, want false")
	}
	if r.LatestVersion == nil || *r.LatestVersion != "1.0.0" {
		t.Errorf("LatestVersion = %v, want pointer to 1.0.0", r.LatestVersion)
	}
	if r.AvailableVersions != nil {
		t.Errorf("AvailableVersions = %v, want omitted", *r.AvailableVersions)
	}
}

// TestCheckConnectors_DefaultListerWiredWhenNil asserts the handler
// falls back to cstore.DefaultVersionLister when the server's
// versionLister field is nil. This guards production callers (where
// the field is wired) from regressing if the field ever becomes
// optional.
func TestCheckConnectors_DefaultListerWiredWhenNil(t *testing.T) {
	store := newStoreWithRefs(t, "hub://acme/connector@1.0.0")

	srv := &apiServer{
		installer:     &cstore.Installer{Store: store},
		versionLister: nil, // forces fallback
	}
	rec := httptest.NewRecorder()
	srv.CheckConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/connectors/check", nil), api.CheckConnectorsParams{})

	var got api.ConnectorsCheckResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Results) != 1 {
		t.Fatalf("Results = %v, want 1 entry", got.Results)
	}
	// The hub:// scheme isn't yet wired in DefaultVersionLister, so we
	// expect a clear "not implemented" error — proving the fallback
	// happened (not a nil-deref).
	if got.Results[0].Error == nil {
		t.Fatal("expected error from default lister fallback")
	}
}
