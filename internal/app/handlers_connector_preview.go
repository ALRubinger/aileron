package app

import (
	"errors"
	"net/http"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// PreviewConnector implements POST /v1/connectors/preview — the
// consent-flow primitive per ADR-0007. Runs the install pipeline up
// through fetch + verify + parse but does NOT commit the tarball to
// the store. The CLI uses this to render the consent prompt before
// asking the operator to confirm.
//
// Same failure semantics as InstallConnector: every cstore.Error
// class maps to the same HTTP status. ADR-0007 makes signature
// failure a hard fail; that surfaces here as a 422 — `--yes` does
// not bypass it because the failure happens before the consent
// prompt fires.
func (s *apiServer) PreviewConnector(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeError(w, http.StatusServiceUnavailable, "installer_disabled",
			"connector install pipeline is not configured")
		return
	}
	var req api.InstallConnectorRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	fqn, err := cstore.ParseFQN(req.Fqn)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_fqn", err.Error())
		return
	}
	if req.Version == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "version is required")
		return
	}
	ref, err := cstore.ParseRef(fqn.String() + "@" + req.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	expected := ""
	if req.ExpectedHash != nil {
		expected = *req.ExpectedHash
	}
	preview, err := s.installer.Preview(r.Context(), cstore.InstallRequest{
		Ref:          ref,
		ExpectedHash: expected,
	})
	if err != nil {
		var aerr *cstore.Error
		if errors.As(err, &aerr) {
			writeError(w, installHTTPStatus(aerr.Class), string(aerr.Class), aerr.Message)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Publisher: prefer the manifest's human-readable Publisher
	// when set; fall back to the FQN authority. The FQN authority
	// is the load-bearing identity (signing key trust is keyed on
	// it), but operators recognize names like "Aileron Engineering"
	// faster than `aileron`.
	publisher := ref.FQN.Authority()
	if preview.Manifest != nil && preview.Manifest.Connector.Publisher != "" {
		publisher = preview.Manifest.Connector.Publisher
	}

	out := api.ConnectorPreview{
		Fqn:              ref.FQN.String(),
		Version:          ref.Version,
		Hash:             preview.Hash,
		Publisher:        publisher,
		SignatureStatus:  api.ConnectorPreviewSignatureStatusVerified,
		AlreadyInstalled: preview.AlreadyInstalled,
		Capabilities:     buildPreviewCapabilities(preview.Manifest),
	}
	writeJSON(w, http.StatusOK, out)
}

// buildPreviewCapabilities flattens the connector manifest's
// `[capabilities.*]` blocks into the wire shape the CLI consumes.
// Absent sub-tables stay absent so the CLI can render only what's
// declared.
func buildPreviewCapabilities(m *cstore.Manifest) api.PreviewCapabilities {
	out := api.PreviewCapabilities{}
	if m == nil {
		return out
	}
	if net := m.Capabilities.Network; net != nil && len(net.Hosts) > 0 {
		hosts := append([]string(nil), net.Hosts...)
		out.NetworkHosts = &hosts
	}
	if cred := m.Capabilities.Credential; cred != nil && cred.Kind != "" {
		c := struct {
			Kind  string  `json:"kind"`
			Scope *string `json:"scope,omitempty"`
		}{Kind: cred.Kind}
		if cred.Scope != "" {
			scope := cred.Scope
			c.Scope = &scope
		}
		out.Credential = &c
	}
	return out
}
