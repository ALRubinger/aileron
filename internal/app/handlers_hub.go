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
	entries, err := s.hub.FetchAll(r.Context())
	if err != nil {
		s.log.Warn("hub: fetch failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	q := ""
	if params.Q != nil {
		q = *params.Q
	}
	filtered := hub.FilterByKeyword(entries, q)
	writeJSON(w, http.StatusOK, api.HubConnectorList{
		Connectors: toAPIEntries(filtered),
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
	entry, err := s.hub.FetchByFQN(r.Context(), params.Fqn)
	if err != nil {
		if errors.Is(err, hub.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no Hub entry for "+params.Fqn)
			return
		}
		s.log.Warn("hub: fetch failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toAPIEntry(entry))
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
	entries, err := s.hub.FetchAll(r.Context())
	if err != nil {
		s.log.Warn("hub: fetch failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "hub_unreachable", err.Error())
		return
	}
	var entry hub.Entry
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

// buildInstallDecision combines the Hub entry + fingerprint with the
// local keyring's view of the publisher to produce trust_state,
// publisher_footprint, and risk_indicators. Trust granularity in v0.x
// is strictly per-repo (per-FQN) per ADR-0013; publisher framing here
// is informational context, not a trust target.
func (s *apiServer) buildInstallDecision(_ context.Context, entries []hub.Entry, entry hub.Entry, fingerprint string) api.HubInstallDecision {
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

	return api.HubInstallDecision{
		Fqn:                entry.FQN,
		Description:        entry.Description,
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

func toAPIEntries(entries []hub.Entry) []api.HubConnectorEntry {
	out := make([]api.HubConnectorEntry, len(entries))
	for i, e := range entries {
		out[i] = toAPIEntry(e)
	}
	return out
}

func toAPIEntry(e hub.Entry) api.HubConnectorEntry {
	return api.HubConnectorEntry{
		Fqn:             e.FQN,
		Description:     e.Description,
		PublisherGithub: e.PublisherGithub,
		KeyUrl:          e.KeyURL,
		ReleasePattern:  e.ReleasePattern,
	}
}
