package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// Sync implements POST /v1/sync — the third sub-feature of issue #364
// (ratifying ADR-0004 §"sync, check, status").
//
// Flow:
//
//  1. Walk every action in `~/.aileron/actions/`.
//  2. Collect the deduplicated set of (FQN, version) connector
//     references each action declares in `[[requires.connectors]]`.
//  3. For each, check the cstore. Already installed → record on
//     `already_installed`. Missing → drive `cstore.Installer.Install`
//     and record on `installed`. Per-connector failures land on
//     `install_failures` and do not abort the sweep.
//  4. For every installed connector (post-step-3), enumerate
//     `[capabilities.credential]` and cross-reference with existing
//     bindings. Capabilities with no binding land on `unbound`.
//
// Idempotent: re-running with no source changes yields zero new
// installs and zero new unbound. Per-connector errors are surfaced
// inside the response per ADR-0004's "offline-failing per connector"
// contract.
func (s *apiServer) Sync(w http.ResponseWriter, r *http.Request) {
	resp := api.SyncResponse{
		ActionsSeen:      0,
		Required:         []api.ConnectorRef{},
		Installed:        []api.InstalledConnector{},
		AlreadyInstalled: []api.ConnectorRef{},
		InstallFailures:  []api.ConnectorInstallFailure{},
		Unbound:          []api.UnboundCapability{},
	}

	// Decode the request body — empty body is fine (auto_install
	// defaults to true, which is the only behavior v1 supports
	// anyway). Per the spec the field is currently informational;
	// keeping the decoder so the wire-shape contract is enforced.
	if r.Body != nil && r.ContentLength != 0 {
		var req api.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request",
				"could not decode sync request: "+err.Error())
			return
		}
		// req.AutoInstall is intentionally unused — v1 has no
		// consent prompt. The flag is accepted for forward
		// compatibility per the OpenAPI contract.
	}

	// Step 1+2: walk actions, collect required refs.
	required := s.collectRequiredConnectors()
	resp.ActionsSeen = s.actionCountForSync()
	resp.Required = required

	// Step 3: install missing connectors.
	for _, ref := range required {
		s.syncOneConnector(r.Context(), ref, &resp)
	}

	// Step 4: gap analysis on bindings — for every installed
	// connector (every required ref now in the cstore), enumerate
	// declared credential capabilities and see which lack bindings.
	resp.Unbound = s.computeUnboundCapabilities(r.Context(), required)

	writeJSON(w, http.StatusOK, resp)
}

// collectRequiredConnectors walks the action store and returns the
// deduplicated set of (FQN, version) connector references each action
// declares in `[[requires.connectors]]`. Order is sorted ascending by
// (FQN, version) for deterministic response output.
func (s *apiServer) collectRequiredConnectors() []api.ConnectorRef {
	if s.actions == nil {
		return []api.ConnectorRef{}
	}
	seen := map[string]api.ConnectorRef{}
	for _, a := range s.actions.List() {
		if a.Manifest == nil {
			continue
		}
		for _, rc := range a.Manifest.Requires.Connectors {
			if rc.Name == "" || rc.Version == "" {
				continue
			}
			key := rc.Name + "@" + rc.Version
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = api.ConnectorRef{Fqn: rc.Name, Version: rc.Version}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]api.ConnectorRef, 0, len(keys))
	for _, k := range keys {
		out = append(out, seen[k])
	}
	return out
}

// actionCountForSync returns the count of actions the sweep walked.
// Distinct from `actionCount()` only in name — kept separate so the
// status surface and the sync surface evolve independently.
func (s *apiServer) actionCountForSync() int {
	if s.actions == nil {
		return 0
	}
	return len(s.actions.List())
}

// syncOneConnector handles step 3 for a single (FQN, version) ref:
// look it up in the cstore. Already installed → append to
// `already_installed`. Missing → drive the install pipeline and
// append to `installed` or `install_failures`. Mutates resp.
func (s *apiServer) syncOneConnector(ctx context.Context, ref api.ConnectorRef, resp *api.SyncResponse) {
	parsed, err := cstore.ParseFQN(ref.Fqn)
	if err != nil {
		resp.InstallFailures = append(resp.InstallFailures, api.ConnectorInstallFailure{
			Fqn: ref.Fqn, Version: ref.Version, Error: err.Error(),
		})
		return
	}
	if s.installer == nil || s.installer.Store == nil {
		resp.InstallFailures = append(resp.InstallFailures, api.ConnectorInstallFailure{
			Fqn: ref.Fqn, Version: ref.Version,
			Error: "connector installer is not configured on this server",
		})
		return
	}
	cref := cstore.Ref{FQN: parsed, Version: ref.Version}

	// Already installed → idempotent no-op.
	if _, ok := s.installer.Store.Lookup(cref); ok {
		resp.AlreadyInstalled = append(resp.AlreadyInstalled, ref)
		return
	}

	// Missing → drive the install pipeline.
	res, ierr := s.installer.Install(ctx, cstore.InstallRequest{Ref: cref})
	if ierr != nil {
		resp.InstallFailures = append(resp.InstallFailures, api.ConnectorInstallFailure{
			Fqn: ref.Fqn, Version: ref.Version, Error: ierr.Error(),
		})
		return
	}
	resp.Installed = append(resp.Installed, api.InstalledConnector{
		Fqn:              ref.Fqn,
		Version:          ref.Version,
		Hash:             res.Hash,
		EntryDir:         res.EntryDir,
		AlreadyInstalled: &res.AlreadyInstalled,
	})
}

// computeUnboundCapabilities walks the connectors named in `required`,
// reads each one's manifest, and returns the credential capabilities
// for which no binding currently exists. Connectors that aren't
// installed (install failed) are skipped — we can only check what we
// have.
//
// Manifest read errors and binding-store errors are swallowed: this
// is a best-effort surface. The CLI rendering treats `unbound` as
// "what we know to be unbound" — silence in this list does not mean
// "nothing's unbound", it means "nothing was reported". The bigger
// failure modes (missing installer, bad manifest) are already
// surfaced through `install_failures`.
//
// Output is sorted by (connector_fqn, kind) for deterministic order.
func (s *apiServer) computeUnboundCapabilities(ctx context.Context, required []api.ConnectorRef) []api.UnboundCapability {
	out := []api.UnboundCapability{}
	if s.installer == nil || s.installer.Store == nil {
		return out
	}
	seen := map[string]bool{}
	for _, ref := range required {
		// One UnboundCapability per (connector_fqn, kind) — even if
		// multiple actions reference the same connector, the
		// capability declared by the connector manifest is the same.
		canonFQN, manifest, err := s.lookupConnector(ref.Fqn)
		if err != nil {
			continue
		}
		cred := manifest.Capabilities.Credential
		if cred == nil || cred.Kind == "" {
			continue
		}
		key := canonFQN + "/" + cred.Kind
		if seen[key] {
			continue
		}
		seen[key] = true
		// If the binding exists, skip. ErrNotFound (or nil bindings
		// store) means unbound.
		if s.bindings != nil {
			if _, berr := s.bindings.Resolve(ctx, canonFQN, cred.Kind); berr == nil {
				continue
			} else if !errors.Is(berr, binding.ErrNotFound) {
				// AmbiguousError counts as bound: the user has
				// already created at least one binding.
				continue
			}
		}
		out = append(out, api.UnboundCapability{
			ConnectorFqn: canonFQN,
			Kind:         cred.Kind,
			Scope:        &cred.Scope,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectorFqn != out[j].ConnectorFqn {
			return out[i].ConnectorFqn < out[j].ConnectorFqn
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

