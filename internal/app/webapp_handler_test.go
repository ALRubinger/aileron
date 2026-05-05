package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// TestWebappHandler_ServesIndexAtRoot covers the most basic load:
// GET / returns the embedded index.html with HTML content type and
// the no-store cache directive (so build-hash rolls aren't masked
// by an old shell pointing at stale `_app/` chunks).
func TestWebappHandler_ServesIndexAtRoot(t *testing.T) {
	srv := httptest.NewServer(webappHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html…", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body, _ := io.ReadAll(resp.Body)
	// The inlined "not built" stub or the real SvelteKit build both
	// satisfy this — both produce a doctype-html page. After
	// `task build:webapp` the real shell ships; before it, the
	// fallback in serveIndex covers.
	if !strings.Contains(strings.ToLower(string(body)), "<!doctype html>") {
		t.Errorf("body lacks doctype; got %q", body[:min(len(body), 200)])
	}
}

// TestServeIndex_FallsBackToStubWhenIndexMissing pins the fresh-clone
// behavior: when the embedded fs has no index.html (only a .gitkeep
// because nobody ran task build:webapp yet), serveIndex returns 200
// with the inlined "webapp not built" stub rather than 500. The
// stub names task build:webapp so the operator knows the next step.
func TestServeIndex_FallsBackToStubWhenIndexMissing(t *testing.T) {
	emptyFS := fstest.MapFS{
		".gitkeep": &fstest.MapFile{Data: []byte{}},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	serveIndex(emptyFS, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stub fallback)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html…", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", "Aileron webapp not built", "task build:webapp"} {
		if !strings.Contains(body, want) {
			t.Errorf("stub body missing %q; got first 200 bytes: %q", want, body[:min(len(body), 200)])
		}
	}
}

// TestServeIndex_PrefersRealIndexOverStub asserts that when the
// embedded fs DOES carry an index.html (post-build), serveIndex
// serves that file instead of the stub. Guards against a refactor
// where the fallback accidentally wins over the real shell.
func TestServeIndex_PrefersRealIndexOverStub(t *testing.T) {
	const realShell = "<!doctype html><html><body>real shell</body></html>"
	withIndex := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(realShell)},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	serveIndex(withIndex, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != realShell {
		t.Errorf("body = %q, want %q (stub leaked through)", got, realShell)
	}
}

// TestWebappHandler_SPAFallbackForUnknownRoutes asserts the SvelteKit
// SPA contract: client-side routes (e.g. /approvals, /settings/foo)
// that have no matching file in the embedded build still serve
// index.html with a 200, and the Svelte router renders the right
// page after hydration. Without this fallback, the browser's direct
// load of /approvals would 404 — only / would be reachable.
func TestWebappHandler_SPAFallbackForUnknownRoutes(t *testing.T) {
	srv := httptest.NewServer(webappHandler())
	t.Cleanup(srv.Close)

	for _, route := range []string{"/approvals", "/some/unknown/route"} {
		t.Run(route, func(t *testing.T) {
			resp, err := http.Get(srv.URL + route)
			if err != nil {
				t.Fatalf("GET %s: %v", route, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 (SPA fallback)", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html…", ct)
			}
		})
	}
}

// TestWebappHandler_AssetMissingReturns404 covers the contrary case
// to SPA fallback: a request that LOOKS like an asset (has a file
// extension in the final segment) and isn't found returns 404. We
// must NOT serve index.html for a missing JS/CSS/image asset — the
// browser would try to interpret HTML as JavaScript and break the
// page. Honest 404s preserve the diagnostic signal.
func TestWebappHandler_AssetMissingReturns404(t *testing.T) {
	srv := httptest.NewServer(webappHandler())
	t.Cleanup(srv.Close)

	cases := []string{
		"/_app/missing.js",
		"/_app/missing.css",
		"/missing.svg",
		"/_app/missing.json",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (missing asset)", resp.StatusCode)
			}
		})
	}
}

// TestWebappHandler_RefusesNonGetMethods asserts the read-only
// contract. The webapp serves static assets; POST / PUT / DELETE
// against this surface are bugs. Returning 405 surfaces them
// cleanly instead of producing surprising behavior under the
// catch-all mount.
func TestWebappHandler_RefusesNonGetMethods(t *testing.T) {
	srv := httptest.NewServer(webappHandler())
	t.Cleanup(srv.Close)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, srv.URL+"/", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s /: %v", method, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", resp.StatusCode)
			}
		})
	}
}

// TestWebappHandler_HEADWorks asserts that HEAD requests succeed
// alongside GET. http.FileServer handles HEAD correctly out of the
// box; this test guards against a refactor accidentally restricting
// to GET-only.
func TestWebappHandler_HEADWorks(t *testing.T) {
	srv := httptest.NewServer(webappHandler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD body = %d bytes, want 0", len(body))
	}
}

// TestWebappHandler_DoesNotShadowAPIPaths asserts the load-bearing
// route-precedence contract: when the webapp handler is mounted at
// `/` alongside specific `/v1/*` handlers on the same `http.ServeMux`,
// the specific handler always wins. Without this, the webapp's SPA
// fallback would intercept every API request and serve HTML instead
// of forwarding to the API.
//
// Drives the contract through a real `http.ServeMux` rather than the
// webapp handler in isolation — that's the wiring that matters.
func TestWebappHandler_DoesNotShadowAPIPaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/", webappHandler())

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// /v1/health → API handler wins.
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"status":"ok"}` {
		t.Errorf("/v1/health body = %q, want JSON ok", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("/v1/health Content-Type = %q, want JSON", ct)
	}

	// /approvals → SPA fallback (webapp handler).
	resp2, err := http.Get(srv.URL + "/approvals")
	if err != nil {
		t.Fatalf("GET /approvals: %v", err)
	}
	defer resp2.Body.Close()
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("/approvals Content-Type = %q, want HTML", ct)
	}
}

// TestHasAssetExtension covers the asset-vs-route classifier directly.
// Each case represents a real path the handler will see at runtime;
// the classifier's correctness gates the SPA-fallback-vs-404 split.
func TestHasAssetExtension(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"_app/version.json", true},
		{"_app/immutable/chunks/foo.abc123.js", true},
		{"favicon.svg", true},
		{"index.html", true},
		{"approvals", false},
		{"some/nested/route", false},
		{"settings/profile", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := hasAssetExtension(c.path); got != c.want {
				t.Errorf("hasAssetExtension(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
