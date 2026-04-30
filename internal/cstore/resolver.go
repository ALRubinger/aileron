package cstore

import (
	"net/url"
	"strings"
)

// Resolver maps a connector reference to a concrete fetch URL. Per
// ADR-0004, each scheme has a fixed mapping the resolver knows as code;
// users do not configure it.
//
// A scheme that cannot answer ResolveTarball is not a viable Aileron
// source; an unknown scheme is a hard fail at install (caller's
// responsibility — Resolver returns ClassUnknownScheme).
type Resolver interface {
	// ResolveTarball returns the URL of the release tarball for the given
	// connector reference. Per ADR-0004 the tarball is always named
	// `aileron.tar.gz` for v1 schemes.
	ResolveTarball(ref Ref) (string, error)

	// ResolveManifest returns the URL of the manifest. For tarball-shipping
	// schemes the manifest lives inside the tarball; this method returns
	// the tarball URL so callers can extract it. For schemes that serve
	// the manifest separately (e.g. hub://), the URL points directly at it.
	ResolveManifest(ref Ref) (string, error)
}

// DefaultResolver returns a multiplexing resolver that routes by scheme,
// dispatching `github://` and `gitlab://` to GitTagResolver and `hub://`
// to HubResolver. Unknown schemes produce ClassUnknownScheme.
func DefaultResolver() Resolver {
	return MuxResolver{
		"github": &GitTagResolver{AssetURLFn: githubAssetURL},
		"gitlab": &GitTagResolver{AssetURLFn: gitlabAssetURL},
		"hub":    &HubResolver{BaseURL: "https://hub.aileron.dev"},
	}
}

// MuxResolver dispatches by FQN scheme.
type MuxResolver map[string]Resolver

// ResolveTarball implements Resolver. Returns ClassUnknownScheme for any
// FQN whose scheme is not registered.
func (m MuxResolver) ResolveTarball(ref Ref) (string, error) {
	r, ok := m[ref.FQN.Scheme]
	if !ok {
		return "", newError(ClassUnknownScheme, BoundaryRuntime, false,
			"resolver has no implementation for scheme %q", ref.FQN.Scheme)
	}
	return r.ResolveTarball(ref)
}

// ResolveManifest implements Resolver.
func (m MuxResolver) ResolveManifest(ref Ref) (string, error) {
	r, ok := m[ref.FQN.Scheme]
	if !ok {
		return "", newError(ClassUnknownScheme, BoundaryRuntime, false,
			"resolver has no implementation for scheme %q", ref.FQN.Scheme)
	}
	return r.ResolveManifest(ref)
}

// GitTagResolver implements Go-modules-style tag resolution for `github://`
// and `gitlab://` schemes per ADR-0004.
//
//   - Single-artifact repo (no subpath): tag is `v<version>`.
//   - Monorepo with subpath: tag is `<subpath>/v<version>`.
//
// AssetURLFn is the host-specific function that builds a release-asset URL
// given the repo, tag, and asset filename. This indirection is the only
// difference between GitHub and GitLab.
type GitTagResolver struct {
	AssetURLFn func(owner, repo, tag, asset string) string
}

const releaseAsset = "aileron.tar.gz"

// ResolveTarball implements Resolver. Builds the tag from FQN+version and
// hands off to AssetURLFn.
func (g *GitTagResolver) ResolveTarball(ref Ref) (string, error) {
	tag := gitTag(ref)
	return g.AssetURLFn(ref.FQN.Owner, ref.FQN.Repo, tag, releaseAsset), nil
}

// ResolveManifest returns the tarball URL — git-host schemes ship the
// manifest inside the tarball.
func (g *GitTagResolver) ResolveManifest(ref Ref) (string, error) {
	return g.ResolveTarball(ref)
}

// gitTag builds the release tag string per ADR-0004 §"github:// and
// gitlab://": single-artifact repos use `v<version>`; multi-artifact repos
// (with subpath) use `<subpath>/v<version>`.
func gitTag(ref Ref) string {
	if ref.FQN.Subpath == "" {
		return "v" + ref.Version
	}
	return ref.FQN.Subpath + "/v" + ref.Version
}

// githubAssetURL is the GitHub release-asset URL pattern documented in
// ADR-0004: https://github.com/<owner>/<repo>/releases/download/<tag>/<asset>
//
// The tag is path-escaped because tags containing `/` (monorepo subpath
// tags) must be URL-escaped to disambiguate from path separators per the
// ADR's note on tag-name URL encoding.
func githubAssetURL(owner, repo, tag, asset string) string {
	return "https://github.com/" + owner + "/" + repo + "/releases/download/" + url.PathEscape(tag) + "/" + asset
}

// gitlabAssetURL is the GitLab release-asset URL pattern documented in
// ADR-0004: https://gitlab.com/<owner>/<repo>/-/releases/<tag>/downloads/<asset>
func gitlabAssetURL(owner, repo, tag, asset string) string {
	return "https://gitlab.com/" + owner + "/" + repo + "/-/releases/" + url.PathEscape(tag) + "/downloads/" + asset
}

// HubResolver implements `hub://` scheme resolution per ADR-0004.
//
// The Hub serves artifacts directly through its HTTP API:
//
//	GET <BaseURL>/api/v1/<owner>/<...path>/<artifact>/<version>/aileron.tar.gz
//	GET <BaseURL>/api/v1/<owner>/<...path>/<artifact>/<version>/manifest
type HubResolver struct {
	BaseURL string
}

// ResolveTarball implements Resolver.
func (h *HubResolver) ResolveTarball(ref Ref) (string, error) {
	return h.endpoint(ref, "aileron.tar.gz"), nil
}

// ResolveManifest implements Resolver. The Hub serves the manifest
// separately from the tarball at the `manifest` endpoint, so callers may
// fetch it without downloading the full tarball when they only need
// metadata.
func (h *HubResolver) ResolveManifest(ref Ref) (string, error) {
	return h.endpoint(ref, "manifest"), nil
}

func (h *HubResolver) endpoint(ref Ref, leaf string) string {
	base := strings.TrimRight(h.BaseURL, "/")
	path := ref.FQN.Owner + "/" + ref.FQN.Repo
	if ref.FQN.Subpath != "" {
		path += "/" + ref.FQN.Subpath
	}
	return base + "/api/v1/" + path + "/" + ref.Version + "/" + leaf
}
