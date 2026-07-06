package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/model"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// --- Binding endpoints (ADR-0006) ---
//
// All five binding handlers follow the same pattern: short-circuit
// to a structured 503 when the binding store is not wired (dev-mode
// without a vault), translate binding-package sentinel errors into
// the right HTTP status, and emit an audit event for every mutation
// so the lifecycle is visible in the trace store.

func (s *apiServer) ListBindings(w http.ResponseWriter, r *http.Request, params api.ListBindingsParams) {
	if s.bindings == nil {
		writeError(w, http.StatusServiceUnavailable, "vault_unavailable", "binding store not configured")
		return
	}
	all, err := s.bindings.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	filtered := make([]api.Binding, 0, len(all))
	for _, b := range all {
		if params.ConnectorFqn != nil && b.ConnectorFQN != *params.ConnectorFqn {
			continue
		}
		if params.Kind != nil && b.Kind != *params.Kind {
			continue
		}
		filtered = append(filtered, toAPIBinding(b))
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })
	writeJSON(w, http.StatusOK, api.BindingListResponse{Items: filtered})
}

func (s *apiServer) GetBinding(w http.ResponseWriter, r *http.Request, name api.BindingName) {
	if s.bindings == nil {
		writeError(w, http.StatusServiceUnavailable, "vault_unavailable", "binding store not configured")
		return
	}
	b, err := s.bindings.Get(r.Context(), binding.Name(name))
	if errors.Is(err, binding.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "binding not found: "+name)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toAPIBinding(b))
}

func (s *apiServer) RevokeBinding(w http.ResponseWriter, r *http.Request, name api.BindingName) {
	if s.bindings == nil {
		writeError(w, http.StatusServiceUnavailable, "vault_unavailable", "binding store not configured")
		return
	}
	if s.vaultLocked {
		writeError(w, http.StatusLocked, "vault_locked", "unlock the vault before revoking bindings")
		return
	}
	if err := s.bindings.Delete(r.Context(), binding.Name(name)); errors.Is(err, binding.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "binding not found: "+name)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	s.recordBindingEvent(r.Context(), model.EventTypeBindingRevoked, name, "", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) SetupBindings(w http.ResponseWriter, r *http.Request) {
	var req api.BindingSetupRequest
	if err := decodeBodyStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if s.bindings == nil {
		writeError(w, http.StatusServiceUnavailable, "vault_unavailable", "binding store not configured")
		return
	}
	if s.vaultLocked {
		writeError(w, http.StatusLocked, "vault_locked", "unlock the vault before creating bindings")
		return
	}
	if s.installer == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "connector store not configured")
		return
	}
	connFQN, connManifest, err := s.lookupConnector(req.ConnectorFqn)
	if err != nil {
		writeError(w, http.StatusNotFound, "connector_not_installed", err.Error())
		return
	}
	if connManifest.Capabilities.Credential == nil {
		writeError(w, http.StatusBadRequest, "no_credential_capability",
			"connector "+connFQN+" does not declare [capabilities.credential]")
		return
	}
	declaredKind := connManifest.Capabilities.Credential.Kind
	declaredScope := connManifest.Capabilities.Credential.Scope
	defaultService := defaultServiceFromFQN(connFQN)

	skipExisting := true
	if req.SkipExisting != nil {
		skipExisting = *req.SkipExisting
	}

	created := []api.Binding{}
	skipped := []string{}

	for i, item := range req.Bindings {
		if string(item.Source.Kind) != declaredKind {
			writeError(w, http.StatusBadRequest, "kind_mismatch",
				"bindings["+itoa(i)+"].source.kind = "+string(item.Source.Kind)+
					"; connector declares "+declaredKind)
			return
		}
		if item.Source.Kind == "oauth2" {
			writeError(w, http.StatusBadRequest, "oauth_setup_not_yet_supported",
				"OAuth bindings are not yet supported via setup; tracking in #388")
			return
		}
		// Setup wires the credential bytes directly into the vault, which
		// works for the kinds whose value is a single opaque secret:
		// api_key (the key) and aws_sigv4 (the secret access key). oauth2
		// (handled above) needs the browser dance instead.
		if item.Source.Kind != "api_key" && item.Source.Kind != cstore.CredentialKindAWSSigV4 {
			writeError(w, http.StatusBadRequest, "unsupported_kind",
				"setup supports api_key and aws_sigv4 sources; got "+string(item.Source.Kind))
			return
		}
		if item.Source.Value == nil || *item.Source.Value == "" {
			writeError(w, http.StatusBadRequest, "missing_value",
				"bindings["+itoa(i)+"].source.value is required for "+string(item.Source.Kind))
			return
		}

		service := defaultService
		if item.Service != nil && *item.Service != "" {
			service = *item.Service
		}
		name, err := binding.MakeName(declaredKind, service, item.Identity)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_identity",
				"bindings["+itoa(i)+"]: "+err.Error())
			return
		}
		b := binding.Binding{
			Name:         name,
			Kind:         declaredKind,
			Service:      service,
			Identity:     item.Identity,
			Scope:        declaredScope,
			ConnectorFQN: connFQN,
			Status:       binding.StatusActive,
		}
		if item.Account != nil {
			b.Account = *item.Account
		}
		// aws_sigv4 carries the non-secret access key id on the binding. The
		// signing region and service are derived from the resolved upstream
		// host at egress (#1978), so the operator supplies no region here and
		// no second copy of the region can drift from the host being signed for.
		if item.Source.Kind == cstore.CredentialKindAWSSigV4 {
			if item.Source.AccessKeyId != nil {
				b.AccessKeyID = *item.Source.AccessKeyId
			}
		}
		err = s.bindings.Put(r.Context(), b, []byte(*item.Source.Value), binding.PutCreate)
		if errors.Is(err, binding.ErrAlreadyExists) {
			if skipExisting {
				skipped = append(skipped, string(b.Name))
				continue
			}
			writeError(w, http.StatusConflict, "binding_exists",
				"binding "+string(b.Name)+" already exists")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		// Re-read so the response reflects the persisted timestamps.
		stored, getErr := s.bindings.Get(r.Context(), b.Name)
		if getErr != nil {
			stored = b
		}
		created = append(created, toAPIBinding(stored))
		s.recordBindingEvent(r.Context(), model.EventTypeBindingCreated, string(b.Name), connFQN, declaredKind)
	}

	resp := api.BindingSetupResponse{Created: created}
	if len(skipped) > 0 {
		resp.Skipped = &skipped
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *apiServer) RebindBinding(w http.ResponseWriter, r *http.Request, name api.BindingName) {
	var req api.RebindRequest
	if err := decodeBodyStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if s.bindings == nil {
		writeError(w, http.StatusServiceUnavailable, "vault_unavailable", "binding store not configured")
		return
	}
	if s.vaultLocked {
		writeError(w, http.StatusLocked, "vault_locked", "unlock the vault before replacing bindings")
		return
	}
	existing, err := s.bindings.Get(r.Context(), binding.Name(name))
	if errors.Is(err, binding.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "binding not found: "+name)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if string(req.Source.Kind) != existing.Kind {
		writeError(w, http.StatusBadRequest, "kind_mismatch",
			"source.kind = "+string(req.Source.Kind)+"; binding's kind is "+existing.Kind)
		return
	}
	if req.Source.Kind == "oauth2" {
		writeError(w, http.StatusBadRequest, "oauth_setup_not_yet_supported",
			"OAuth rebind is not yet supported; tracking in #388")
		return
	}
	if req.Source.Kind != "api_key" && req.Source.Kind != cstore.CredentialKindAWSSigV4 {
		writeError(w, http.StatusBadRequest, "unsupported_kind",
			"rebind supports api_key and aws_sigv4 sources; got "+string(req.Source.Kind))
		return
	}
	if req.Source.Value == nil || *req.Source.Value == "" {
		writeError(w, http.StatusBadRequest, "missing_value",
			"source.value is required for "+string(req.Source.Kind))
		return
	}
	// aws_sigv4 rebind may also update the non-secret access key id alongside
	// rotating the secret access key. An omitted access key id keeps the
	// existing value so a pure secret rotation need not re-supply it. The
	// signing region and service are derived from the resolved upstream host
	// at egress (#1978), so there is no region to update here.
	if req.Source.Kind == cstore.CredentialKindAWSSigV4 {
		if req.Source.AccessKeyId != nil {
			existing.AccessKeyID = *req.Source.AccessKeyId
		}
	}
	if err := s.bindings.Put(r.Context(), existing, []byte(*req.Source.Value), binding.PutUpsert); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	stored, getErr := s.bindings.Get(r.Context(), existing.Name)
	if getErr != nil {
		stored = existing
	}
	s.recordBindingEvent(r.Context(), model.EventTypeBindingRebound, name, existing.ConnectorFQN, existing.Kind)
	writeJSON(w, http.StatusOK, toAPIBinding(stored))
}

// --- helpers ---

// lookupConnector finds the connector manifest for the given FQN. It
// trusts the cstore index plus the on-disk manifest. Returns the
// canonicalised FQN string (so the audit/binding metadata always uses
// the canonical form), the parsed manifest, and any error.
func (s *apiServer) lookupConnector(fqnStr string) (string, *cstore.Manifest, error) {
	fqn, err := cstore.ParseFQN(fqnStr)
	if err != nil {
		return "", nil, err
	}
	_, hash, ok := s.installer.Store.LookupAnyVersion(fqn)
	if !ok {
		return "", nil, errors.New("connector " + fqn.String() + " is not installed")
	}
	dir, err := s.installer.Store.EntryDir(hash)
	if err != nil {
		return "", nil, err
	}
	bytes, err := os.ReadFile(filepath.Join(dir, "manifest.toml"))
	if err != nil {
		return "", nil, err
	}
	cmf, err := cstore.ParseManifest(filepath.Join(dir, "manifest.toml"), bytes)
	if err != nil {
		return "", nil, err
	}
	return fqn.String(), cmf, nil
}

// recordBindingEvent persists an audit event for a binding lifecycle
// transition. Payload deliberately excludes credential bytes. Field
// names follow the OTel-namespaced audit schema (issue #390 Phase
// 6.5) so Phase 7 emission can map them straight to span attributes.
func (s *apiServer) recordBindingEvent(ctx context.Context, evt model.EventType, name, connectorFQN, kind string) {
	if s.auditRecorder == nil {
		return
	}
	payload := map[string]any{"aileron.binding.name": name}
	if connectorFQN != "" {
		payload["aileron.connector.fqn"] = connectorFQN
	}
	if kind != "" {
		payload["aileron.capability.kind"] = kind
	}
	s.auditRecorder.RecordSuccess(ctx, evt, model.ActorRef{Type: model.ActorTypeHuman, ID: "user"}, payload)
}

// toAPIBinding renders an internal binding into the API response shape.
func toAPIBinding(b binding.Binding) api.Binding {
	out := api.Binding{
		Name:         string(b.Name),
		Kind:         b.Kind,
		Service:      b.Service,
		Identity:     b.Identity,
		ConnectorFqn: b.ConnectorFQN,
		CreatedAt:    b.CreatedAt,
	}
	if b.Scope != "" {
		s := b.Scope
		out.Scope = &s
	}
	if b.Account != "" {
		a := b.Account
		out.Account = &a
	}
	if b.AccessKeyID != "" {
		akid := b.AccessKeyID
		out.AccessKeyId = &akid
	}
	if !b.LastUsedAt.IsZero() {
		t := b.LastUsedAt
		out.LastUsedAt = &t
	}
	if !b.LastRefreshedAt.IsZero() {
		t := b.LastRefreshedAt
		out.LastRefreshedAt = &t
	}
	if b.RefreshTokenPresent {
		v := true
		out.RefreshTokenPresent = &v
	}
	if b.Status != "" {
		s := api.BindingStatus(b.Status)
		out.Status = &s
	}
	if len(b.GrantedScopes) > 0 {
		gs := append([]string(nil), b.GrantedScopes...)
		out.GrantedScopes = &gs
	}
	if b.StaleReason != "" {
		sr := api.BindingStaleReason(b.StaleReason)
		out.StaleReason = &sr
	}
	if len(b.MissingScopes) > 0 {
		ms := append([]string(nil), b.MissingScopes...)
		out.MissingScopes = &ms
	}
	return out
}

// defaultServiceFromFQN returns the service segment Aileron infers
// from a connector FQN's repo segment. For `github://aileron/slack`
// it returns `slack`. Used by setup when the caller does not
// explicitly override `service`.
func defaultServiceFromFQN(fqnStr string) string {
	fqn, err := cstore.ParseFQN(fqnStr)
	if err != nil {
		return "unknown"
	}
	if fqn.Repo == "" {
		return fqn.Owner
	}
	return fqn.Repo
}

// itoa formats a non-negative int as a decimal string. Avoids
// pulling in strconv for a one-liner used in a couple of error
// messages.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
