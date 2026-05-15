package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/hub"
)

// ListHubConnectors returns Hub entries, optionally filtered by a
// case-insensitive keyword on FQN and description (`?q=`). Decisions
// behind the shape and no-cache fetch strategy are ratified in
// ADR-0013 and #486.
func (s *apiServer) ListHubConnectors(w http.ResponseWriter, r *http.Request, params api.ListHubConnectorsParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	entries, err := s.hub.FetchAllConnectors(r.Context())
	if err != nil {
		s.log.Warn("hub: fetch failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	q := ""
	if params.Q != nil {
		q = *params.Q
	}
	filtered := hub.FilterConnectorsByKeyword(entries, q)
	writeJSON(w, http.StatusOK, api.HubConnectorList{
		Connectors: toAPIConnectorEntries(filtered),
	})
}

// GetHubConnector looks up a single Hub entry by FQN (passed as a
// query parameter since FQNs carry `://` and `/`, which don't sit
// well in Go ServeMux single-segment path patterns).
func (s *apiServer) GetHubConnector(w http.ResponseWriter, r *http.Request, params api.GetHubConnectorParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	if strings.TrimSpace(params.Fqn) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "fqn is required")
		return
	}
	entry, err := s.hub.FetchConnectorByFQN(r.Context(), params.Fqn)
	if err != nil {
		if errors.Is(err, hub.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no Hub entry for "+params.Fqn)
			return
		}
		s.log.Warn("hub: fetch failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toAPIConnectorEntry(entry))
}

// GetHubInstallDecision returns the payload the install-time prompt
// (CLI y/N and webapp modal) renders. Combines the Hub entry, the
// publisher's current key fingerprint, local keyring trust state, and
// the publisher's other Hub connectors as informational context.
// Shape resolved in #487.
func (s *apiServer) GetHubInstallDecision(w http.ResponseWriter, r *http.Request, params api.GetHubInstallDecisionParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	if strings.TrimSpace(params.Fqn) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "fqn is required")
		return
	}
	entries, err := s.hub.FetchAllConnectors(r.Context())
	if err != nil {
		s.log.Warn("hub: fetch failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	var entry hub.ConnectorEntry
	found := false
	for _, e := range entries {
		if e.FQN == params.Fqn {
			entry = e
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no Hub entry for "+params.Fqn)
		return
	}

	_, fingerprint, err := s.hub.FetchPublisherKey(r.Context(), entry.KeyURL)
	if err != nil {
		s.log.Warn("hub: fetch publisher key failed", "key_url", entry.KeyURL, "error", err)
		writeError(w, http.StatusBadGateway, "key_fetch_failed", err.Error())
		return
	}

	decision := s.buildInstallDecision(r.Context(), entries, entry, fingerprint)
	writeJSON(w, http.StatusOK, decision)
}

// buildInstallDecision projects a per-authority trust panel into the
// flat connector-level HubInstallDecision shape. The per-authority
// computation lives in buildAuthority so the composite action/suite
// endpoints can share it.
func (s *apiServer) buildInstallDecision(_ context.Context, entries []hub.ConnectorEntry, entry hub.ConnectorEntry, fingerprint string) api.HubInstallDecision {
	auth := s.buildAuthority(entries, entry, fingerprint)
	return api.HubInstallDecision{
		Fqn:                auth.Fqn,
		Description:        entry.Description,
		PublisherGithub:    auth.PublisherGithub,
		Fingerprint:        auth.Fingerprint,
		TrustState:         auth.TrustState,
		PublisherFootprint: auth.PublisherFootprint,
		RiskIndicators:     auth.RiskIndicators,
	}
}

// buildAuthority combines the connector entry + fingerprint with the
// local keyring's view of the publisher to produce trust_state,
// publisher_footprint, and risk_indicators. Trust granularity in v0.x
// is strictly per-repo (per-FQN) per ADR-0013; publisher framing here
// is informational context, not a trust target. Used by both the
// connector-level install-decision and the composite action/suite
// install-decision endpoints.
func (s *apiServer) buildAuthority(entries []hub.ConnectorEntry, entry hub.ConnectorEntry, fingerprint string) api.HubInstallAuthority {
	footprint := hub.PublisherFootprint(entries, entry)

	kr, _ := cstore.LoadKeyring(s.resolveKeyringPath())
	trustState := api.HubTrustStateUnknown
	conflictFQN := ""
	if kr != nil {
		// Already-trusted: keyring carries a key for this exact FQN
		// whose fingerprint matches the current publisher key. The
		// presence of *any* key for the FQN is enough — `aileron
		// keyring trust` only writes a key if the user has explicitly
		// confirmed it, so any keyring entry counts as consent.
		for _, k := range kr.Keys(entry.FQN) {
			if hub.Fingerprint(k) == fingerprint {
				trustState = api.HubTrustStateAlreadyTrusted
				break
			}
		}
		// Conflict: a sibling FQN by the same publisher_github carries
		// a trusted key, and that key's fingerprint differs from the
		// one this entry's publisher is about to be installed with.
		// Indicates a key rotation, MITM, or impersonation — surface
		// in red so the user notices.
		if trustState == api.HubTrustStateUnknown {
			for _, sibling := range footprint {
				for _, k := range kr.Keys(sibling) {
					if hub.Fingerprint(k) != fingerprint {
						trustState = api.HubTrustStateConflict
						conflictFQN = sibling
						break
					}
				}
				if trustState == api.HubTrustStateConflict {
					break
				}
			}
		}
	}

	risks := buildRiskIndicators(kr, footprint, trustState, conflictFQN)

	return api.HubInstallAuthority{
		Fqn:                entry.FQN,
		PublisherGithub:    entry.PublisherGithub,
		Fingerprint:        fingerprint,
		TrustState:         trustState,
		PublisherFootprint: footprint,
		RiskIndicators:     risks,
	}
}

func buildRiskIndicators(kr *cstore.Ed25519Keyring, footprint []string, trustState api.HubTrustState, conflictFQN string) []string {
	var risks []string
	trustedSiblings := 0
	if kr != nil {
		for _, sibling := range footprint {
			if len(kr.Keys(sibling)) > 0 {
				trustedSiblings++
			}
		}
	}
	switch {
	case trustState == api.HubTrustStateConflict:
		risks = append(risks, "Key fingerprint differs from one you trust for a sibling repo ("+conflictFQN+")")
	case trustState == api.HubTrustStateAlreadyTrusted:
		// Already trusted at this FQN: the "first install" framing would
		// be a lie (the user has this exact connector). Surface sibling
		// trust as informational context, otherwise emit nothing — the
		// already_trusted state itself carries the reassurance.
		if trustedSiblings > 0 {
			risks = append(risks, pluralizeTrustedSiblings(trustedSiblings))
		}
	case trustedSiblings == 0:
		risks = append(risks, "First connector by this publisher you've installed")
	default:
		risks = append(risks, pluralizeTrustedSiblings(trustedSiblings))
	}
	return risks
}

func pluralizeTrustedSiblings(n int) string {
	if n == 1 {
		return "Publisher has 1 other connector you already trust"
	}
	return "Publisher has multiple other connectors you already trust"
}

func (s *apiServer) resolveKeyringPath() string {
	if s.keyringPath != "" {
		return s.keyringPath
	}
	return cstore.DefaultKeyringPath()
}

func toAPIConnectorEntries(entries []hub.ConnectorEntry) []api.HubConnectorEntry {
	out := make([]api.HubConnectorEntry, len(entries))
	for i, e := range entries {
		out[i] = toAPIConnectorEntry(e)
	}
	return out
}

func toAPIConnectorEntry(e hub.ConnectorEntry) api.HubConnectorEntry {
	return api.HubConnectorEntry{
		Fqn:             e.FQN,
		Description:     e.Description,
		PublisherGithub: e.PublisherGithub,
		KeyUrl:          e.KeyURL,
		ReleasePattern:  e.ReleasePattern,
	}
}

func toAPIActionEntries(entries []hub.ActionEntry) []api.HubActionEntry {
	out := make([]api.HubActionEntry, len(entries))
	for i, e := range entries {
		out[i] = toAPIActionEntry(e)
	}
	return out
}

func toAPIActionEntry(e hub.ActionEntry) api.HubActionEntry {
	intents := append([]string(nil), e.Intents...)
	return api.HubActionEntry{
		Fqn:             e.FQN,
		Description:     e.Description,
		PublisherGithub: e.PublisherGithub,
		ConnectorFqn:    e.ConnectorFQN,
		Intents:         &intents,
		Category:        stringPtrOrNil(e.Category),
	}
}

func toAPISuiteEntries(entries []hub.SuiteEntry) []api.HubSuiteEntry {
	out := make([]api.HubSuiteEntry, len(entries))
	for i, e := range entries {
		out[i] = toAPISuiteEntry(e)
	}
	return out
}

func toAPISuiteEntry(e hub.SuiteEntry) api.HubSuiteEntry {
	connectors := append([]string(nil), e.ConnectorsRequired...)
	return api.HubSuiteEntry{
		Fqn:                e.FQN,
		Description:        e.Description,
		PublisherGithub:    e.PublisherGithub,
		MemberActions:      append([]string(nil), e.MemberActions...),
		ConnectorsRequired: &connectors,
		Category:           stringPtrOrNil(e.Category),
	}
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ListHubActions returns Hub action entries, optionally filtered by a
// case-insensitive keyword on FQN and description (`?q=`). Mirrors
// ListHubConnectors but reads the actions/ directory of the Hub repo.
func (s *apiServer) ListHubActions(w http.ResponseWriter, r *http.Request, params api.ListHubActionsParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	entries, err := s.hub.FetchAllActions(r.Context())
	if err != nil {
		s.log.Warn("hub: fetch actions failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	q := ""
	if params.Q != nil {
		q = *params.Q
	}
	filtered := hub.FilterActionsByKeyword(entries, q)
	writeJSON(w, http.StatusOK, api.HubActionList{
		Actions: toAPIActionEntries(filtered),
	})
}

// GetHubAction looks up a single Hub action entry by FQN.
func (s *apiServer) GetHubAction(w http.ResponseWriter, r *http.Request, params api.GetHubActionParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	if strings.TrimSpace(params.Fqn) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "fqn is required")
		return
	}
	entry, err := s.hub.FetchActionByFQN(r.Context(), params.Fqn)
	if err != nil {
		if errors.Is(err, hub.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no Hub action entry for "+params.Fqn)
			return
		}
		s.log.Warn("hub: fetch action failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toAPIActionEntry(entry))
}

// ListHubSuites returns Hub suite entries, optionally filtered by a
// case-insensitive keyword on FQN and description (`?q=`).
func (s *apiServer) ListHubSuites(w http.ResponseWriter, r *http.Request, params api.ListHubSuitesParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	entries, err := s.hub.FetchAllSuites(r.Context())
	if err != nil {
		s.log.Warn("hub: fetch suites failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	q := ""
	if params.Q != nil {
		q = *params.Q
	}
	filtered := hub.FilterSuitesByKeyword(entries, q)
	writeJSON(w, http.StatusOK, api.HubSuiteList{
		Suites: toAPISuiteEntries(filtered),
	})
}

// GetHubSuite looks up a single Hub suite entry by FQN.
func (s *apiServer) GetHubSuite(w http.ResponseWriter, r *http.Request, params api.GetHubSuiteParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	if strings.TrimSpace(params.Fqn) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "fqn is required")
		return
	}
	entry, err := s.hub.FetchSuiteByFQN(r.Context(), params.Fqn)
	if err != nil {
		if errors.Is(err, hub.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no Hub suite entry for "+params.Fqn)
			return
		}
		s.log.Warn("hub: fetch suite failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toAPISuiteEntry(entry))
}

// GetHubActionInstallDecision returns a composite install-decision
// payload for an action install. The action's declared connector_fqn
// becomes the single `authorities[]` entry. Composite payloads were
// ratified in ADR-0013's action-first amendment; tracked in #709.
func (s *apiServer) GetHubActionInstallDecision(w http.ResponseWriter, r *http.Request, params api.GetHubActionInstallDecisionParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	if strings.TrimSpace(params.Fqn) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "fqn is required")
		return
	}

	ctx := r.Context()
	action, err := s.hub.FetchActionByFQN(ctx, params.Fqn)
	if err != nil {
		if errors.Is(err, hub.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no Hub action entry for "+params.Fqn)
			return
		}
		s.log.Warn("hub: fetch action failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	if strings.TrimSpace(action.ConnectorFQN) == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_action_entry", "action entry has no connector_fqn")
		return
	}

	connectors, err := s.hub.FetchAllConnectors(ctx)
	if err != nil {
		s.log.Warn("hub: fetch connectors failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	connectorEntry, ok := findConnector(connectors, action.ConnectorFQN)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "missing_connector", "action depends on "+action.ConnectorFQN+", which is not listed in the Hub")
		return
	}
	authority, status, errInfo := s.buildAuthorityForConnector(ctx, connectors, connectorEntry)
	if errInfo.code != "" {
		writeError(w, status, errInfo.code, errInfo.message)
		return
	}

	out := api.HubActionInstallDecision{
		Kind:            api.HubActionInstallDecisionKindAction,
		Fqn:             action.FQN,
		Description:     action.Description,
		PublisherGithub: action.PublisherGithub,
		ConnectorFqn:    action.ConnectorFQN,
		Authorities:     []api.HubInstallAuthority{authority},
	}
	if v := s.latestStableVersionFor(ctx, action.FQN); v != "" {
		out.LatestVersion = &v
	}
	writeJSON(w, http.StatusOK, out)
}

// GetHubSuiteInstallDecision returns a composite install-decision
// payload for a suite install. Walks the suite's dependency closure
// (preferring its declared `connectors_required`; otherwise deriving
// from each member action's `connector_fqn`) and returns one
// authority per unique connector.
func (s *apiServer) GetHubSuiteInstallDecision(w http.ResponseWriter, r *http.Request, params api.GetHubSuiteInstallDecisionParams) {
	if s.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "hub_disabled", "hub client not configured")
		return
	}
	if strings.TrimSpace(params.Fqn) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "fqn is required")
		return
	}

	ctx := r.Context()
	suite, err := s.hub.FetchSuiteByFQN(ctx, params.Fqn)
	if err != nil {
		if errors.Is(err, hub.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no Hub suite entry for "+params.Fqn)
			return
		}
		s.log.Warn("hub: fetch suite failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}

	connectorFQNs, status, errInfo := s.resolveSuiteConnectorClosure(ctx, suite)
	if errInfo.code != "" {
		writeError(w, status, errInfo.code, errInfo.message)
		return
	}

	connectors, err := s.hub.FetchAllConnectors(ctx)
	if err != nil {
		s.log.Warn("hub: fetch connectors failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}

	authorities := make([]api.HubInstallAuthority, 0, len(connectorFQNs))
	for _, cfqn := range connectorFQNs {
		entry, ok := findConnector(connectors, cfqn)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, "missing_connector", "suite depends on "+cfqn+", which is not listed in the Hub")
			return
		}
		auth, status, errInfo := s.buildAuthorityForConnector(ctx, connectors, entry)
		if errInfo.code != "" {
			writeError(w, status, errInfo.code, errInfo.message)
			return
		}
		authorities = append(authorities, auth)
	}

	out := api.HubSuiteInstallDecision{
		Kind:            "suite",
		Fqn:             suite.FQN,
		Description:     suite.Description,
		PublisherGithub: suite.PublisherGithub,
		MemberActions:   append([]string(nil), suite.MemberActions...),
		Authorities:     authorities,
	}
	if v := s.latestStableVersionFor(ctx, suite.FQN); v != "" {
		out.LatestVersion = &v
	}
	writeJSON(w, http.StatusOK, out)
}

// latestStableVersionFor resolves the latest non-prerelease SemVer
// tag from the source repo backing the supplied FQN. Returns "" on
// any resolution failure (no published releases, network hiccup,
// scheme without a wired version lister) — the webapp install modal
// gracefully degrades to "no version available" rather than failing
// the entire install-decision render. Mirrors the prerelease filter
// the connector-check handler applies at handlers_connectors_check.go.
//
// The supplied FQN may be a full action FQN (with `/actions/<name>`
// subpath) or a suite FQN (`/suite` or `/suites/<x>`). The version
// lister keys on the repo, which lives at `Authority()`, so the
// helper re-parses Authority() to get a clean FQN for the lister.
func (s *apiServer) latestStableVersionFor(ctx context.Context, rawFQN string) string {
	lister := s.versionLister
	if lister == nil {
		lister = cstore.DefaultVersionLister()
	}
	parsed, err := cstore.ParseFQN(rawFQN)
	if err != nil {
		return ""
	}
	repoFQN, err := cstore.ParseFQN(parsed.Authority())
	if err != nil {
		return ""
	}
	versions, err := lister.ListVersions(ctx, repoFQN)
	if err != nil {
		return ""
	}
	for _, v := range versions {
		if !cstore.IsPrerelease(v) {
			return v
		}
	}
	return ""
}

// resolveSuiteConnectorClosure returns the deduplicated, order-preserving
// list of connector FQNs the suite depends on. Prefers the suite's
// declared `connectors_required`; otherwise walks `member_actions` and
// collects each action's `connector_fqn`.
func (s *apiServer) resolveSuiteConnectorClosure(ctx context.Context, suite hub.SuiteEntry) ([]string, int, errInfo) {
	if len(suite.ConnectorsRequired) > 0 {
		return uniqueStrings(suite.ConnectorsRequired), 0, errInfo{}
	}
	if len(suite.MemberActions) == 0 {
		return nil, http.StatusUnprocessableEntity, errInfo{"invalid_suite_entry", "suite has no member_actions and no connectors_required"}
	}
	actions, err := s.hub.FetchAllActions(ctx)
	if err != nil {
		s.log.Warn("hub: fetch actions failed", "error", err)
		return nil, http.StatusServiceUnavailable, errInfo{"hub_unreachable", err.Error()}
	}
	byFQN := make(map[string]hub.ActionEntry, len(actions))
	for _, a := range actions {
		byFQN[a.FQN] = a
	}
	var fqns []string
	for _, memberFQN := range suite.MemberActions {
		action, ok := byFQN[memberFQN]
		if !ok {
			return nil, http.StatusUnprocessableEntity, errInfo{"missing_member_action", "suite member action " + memberFQN + " is not listed in the Hub"}
		}
		if strings.TrimSpace(action.ConnectorFQN) == "" {
			return nil, http.StatusUnprocessableEntity, errInfo{"invalid_action_entry", "member action " + memberFQN + " has no connector_fqn"}
		}
		fqns = append(fqns, action.ConnectorFQN)
	}
	return uniqueStrings(fqns), 0, errInfo{}
}

// buildAuthorityForConnector fetches the publisher key for the given
// connector entry and assembles the per-authority trust panel. Returns
// an errInfo with a non-empty code on failure; the HTTP status code
// follows the same shape as the connector-level install-decision
// endpoint (502 for key-fetch failures, 503 if the Hub is unreachable
// when fetching).
func (s *apiServer) buildAuthorityForConnector(ctx context.Context, connectors []hub.ConnectorEntry, entry hub.ConnectorEntry) (api.HubInstallAuthority, int, errInfo) {
	_, fingerprint, err := s.hub.FetchPublisherKey(ctx, entry.KeyURL)
	if err != nil {
		s.log.Warn("hub: fetch publisher key failed", "key_url", entry.KeyURL, "error", err)
		return api.HubInstallAuthority{}, http.StatusBadGateway, errInfo{"key_fetch_failed", err.Error()}
	}
	return s.buildAuthority(connectors, entry, fingerprint), 0, errInfo{}
}

// findConnector returns the connector entry whose FQN matches fqn,
// and false if no match exists.
func findConnector(entries []hub.ConnectorEntry, fqn string) (hub.ConnectorEntry, bool) {
	for _, e := range entries {
		if e.FQN == fqn {
			return e, true
		}
	}
	return hub.ConnectorEntry{}, false
}

// uniqueStrings returns the deduplicated input in original order.
func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// errInfo carries the code and human-readable message for an HTTP
// error response. An empty code signals "no error".
type errInfo struct{ code, message string }
