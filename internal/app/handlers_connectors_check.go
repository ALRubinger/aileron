package app

import (
	"context"
	"net/http"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// CheckConnectors implements GET /v1/connectors/check (ADR-0004 §"sync,
// check, status"). For each (FQN, version) pair installed in the
// content-addressed store, it queries the connector's source for newer
// versions and reports the result.
//
// Per ADR-0004, the check is "offline-failing per connector": a single
// source that's unreachable does not abort the sweep. The error is
// surfaced inside that connector's result so `aileron connector check`
// can render a per-row "(check failed)" without losing the rest of
// the report.
//
// Pre-releases are excluded by default — `available_versions` and
// `latest_version` only consider stable releases. Setting
// `include_prerelease=true` flips that.
func (s *apiServer) CheckConnectors(w http.ResponseWriter, r *http.Request, params api.CheckConnectorsParams) {
	includePrerelease := params.IncludePrerelease != nil && *params.IncludePrerelease

	// Empty installer or store → empty results, not an error. Same
	// shape as `/v1/status` returning zero counts on a bare server:
	// the surface stays well-formed even when nothing's wired.
	var refs []cstore.Ref
	if s.installer != nil && s.installer.Store != nil {
		refs = s.installer.Store.ListInstalled()
	}

	lister := s.versionLister
	if lister == nil {
		lister = cstore.DefaultVersionLister()
	}

	results := make([]api.ConnectorCheckResult, 0, len(refs))
	for _, ref := range refs {
		results = append(results, checkOneConnector(r.Context(), lister, ref, includePrerelease))
	}

	writeJSON(w, http.StatusOK, api.ConnectorsCheckResponse{Results: results})
}

// checkOneConnector queries one (FQN, version) pair against its
// source. Returns a populated result with `error` set when the source
// can't answer. The function never returns a Go error — per-connector
// failures are surfaced inside the response object so the sweep
// continues regardless.
func checkOneConnector(ctx context.Context, lister cstore.VersionLister, ref cstore.Ref, includePrerelease bool) api.ConnectorCheckResult {
	result := api.ConnectorCheckResult{
		Fqn:             ref.FQN.String(),
		CurrentVersion:  ref.Version,
		UpdateAvailable: false,
	}

	all, err := lister.ListVersions(ctx, ref.FQN)
	if err != nil {
		msg := err.Error()
		result.Error = &msg
		return result
	}

	// Filter pre-releases when the caller didn't opt in.
	filtered := all
	if !includePrerelease {
		filtered = filtered[:0]
		for _, v := range all {
			if !cstore.IsPrerelease(v) {
				filtered = append(filtered, v)
			}
		}
	}

	// Available versions — those strictly newer than what's installed.
	// `all` (and therefore `filtered`) is sorted descending by SemVer
	// per the lister contract; preserve that order in the response.
	newer := make([]string, 0, len(filtered))
	for _, v := range filtered {
		if cstore.CompareVersions(v, ref.Version) > 0 {
			newer = append(newer, v)
		}
	}

	// Latest version: the head of the post-prerelease-filter list, or
	// nothing when the source has no published versions matching this
	// connector. We expose latest even when it's not newer than what's
	// installed — it's useful operator info ("you're on the latest").
	if len(filtered) > 0 {
		latest := filtered[0]
		result.LatestVersion = &latest
		if cstore.CompareVersions(latest, ref.Version) > 0 {
			result.UpdateAvailable = true
		}
	}

	if len(newer) > 0 {
		result.AvailableVersions = &newer
	}

	return result
}
