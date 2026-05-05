package app

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// webappHandler serves the embedded local webapp at the gateway
// origin under `aileron launch`. Mounted at `/` as a catch-all after
// every API and docs route is registered, so Go's `http.ServeMux`
// gives more-specific paths their handlers first and only un-matched
// paths reach the webapp. The agent's `/v1/*` API, `/auth/*` flows,
// `/docs`, and `/openapi.yaml` are never shadowed.
//
// SPA fallback: SvelteKit's `adapter-static` generates one index.html
// plus chunked assets under `_app/`. Direct navigation to a
// client-side route (e.g. `/approvals`) has no matching file in the
// embedded fs — without a fallback, the browser would 404. Instead,
// we serve `index.html` for any path that doesn't look like a static
// asset, and the client-side router renders the right page after
// hydration.
//
// Asset detection: paths whose final segment carries a file
// extension (e.g. `.js`, `.css`, `.svg`, `.json`) are treated as
// real assets — when missing, return 404 honestly rather than
// silently serving index.html (which would return JS-shaped HTML
// and break the import).
//
// The handler is read-only — it accepts only GET and HEAD. Any other
// method returns 405. ServeMux's `mux.Handle("/", ...)` doesn't
// itself enforce method, so the handler does it inline.
func webappHandler() http.Handler {
	dist, err := fs.Sub(webappFS, "webapp_dist")
	if err != nil {
		// Static-asset extraction can only fail if the embed
		// directive at compile time pointed at a non-existent
		// path — that's a programmer error, not a runtime one.
		// Panic so a misbuilt binary doesn't silently 404 every
		// request.
		panic("webapp: extracting embed sub-fs: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Trim the leading slash for embed.FS lookup; embed paths
		// are relative ("index.html", "_app/version.json").
		reqPath := strings.TrimPrefix(r.URL.Path, "/")
		// Index requests always route through serveIndex so the
		// `Cache-Control: no-store` header is set consistently —
		// http.FileServer doesn't set Cache-Control by default,
		// which would let the browser cache a stale shell whose
		// `_app/` chunk hashes no longer exist after a rebuild.
		if reqPath == "" || reqPath == "index.html" {
			serveIndex(dist, w, r)
			return
		}
		if _, err := fs.Stat(dist, reqPath); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// Path not found in the bundle. SPA fallback only applies
		// to "page-shaped" paths — those without a file extension.
		// Asset paths (e.g. /_app/foo.js) that miss should 404
		// honestly so the browser doesn't try to interpret an HTML
		// document as JavaScript.
		if hasAssetExtension(reqPath) {
			http.NotFound(w, r)
			return
		}
		// Otherwise: serve index.html with a 200 and let the
		// client-side router resolve the path.
		serveIndex(dist, w, r)
	})
}

// serveIndex serves index.html with a sensible Content-Type and
// the no-store cache directive that prevents stale shells when the
// build hash changes. Assets under `_app/` carry their own
// content-hashed filenames, so they're safe to cache long-term —
// only the shell needs to be no-store.
func serveIndex(dist fs.FS, w http.ResponseWriter, r *http.Request) {
	body, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		http.Error(w, "webapp index missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// hasAssetExtension reports whether the request path looks like a
// static asset rather than a client-side route. The check is
// intentionally simple — any final-segment dot is an extension. This
// matches the SvelteKit static build's shape (every emitted asset
// has an extension; routes like `/approvals` don't). False positives
// on routes that happen to contain dots aren't a correctness problem
// for this app; they'd 404 instead of falling through, which is
// recoverable by hitting the root and using client-side nav.
//
// Empty input returns false (treated as a route, not an asset). The
// caller already rewrites "" → "index.html" before this check, so
// empty never reaches us in production; the empty branch exists for
// defensive symmetry under refactoring.
func hasAssetExtension(reqPath string) bool {
	if reqPath == "" {
		return false
	}
	base := path.Base(reqPath)
	if base == "." || base == "/" {
		return false
	}
	return strings.Contains(base, ".")
}
