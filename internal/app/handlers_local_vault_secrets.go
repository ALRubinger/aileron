package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

// vaultCredentialSessionHeader is the optional request header the
// launcher may set to attribute a per-agent credential operation to a
// specific launch session. When absent the requester is resolved to the
// loopback service actor. This stays consistent with the
// `X-Aileron-Session-Id` convention used by the connector-operations and
// sandbox-proxy handlers, and requires no OpenAPI/wire-shape change.
const vaultCredentialSessionHeader = "X-Aileron-Session-Id"

// vaultCredentialLoopbackActorID is the actor ID used when no session
// header is present. ADR-0011 per-agent credential verbs are reachable
// only over the daemon loopback, so an unattributed request is recorded
// as a loopback service actor rather than minting a new ActorType.
const vaultCredentialLoopbackActorID = "loopback"

// maxSessionIDLen caps the length of an accepted X-Aileron-Session-Id
// after sanitization. The header is untrusted (the loopback endpoint is
// reachable by any local process, so the value can be forged), and it
// flows into both the audit actor ID and the `aileron.session.id`
// payload. A bound prevents an unbounded attacker-controlled string from
// bloating audit records.
const maxSessionIDLen = 128

// sanitizeSessionID treats the X-Aileron-Session-Id header as untrusted
// input. It trims surrounding whitespace, restricts the value to a safe
// allow-list (ASCII alphanumerics plus `-`, `_`, `.`), and truncates to
// maxSessionIDLen. Disallowed bytes — including CR/LF and other control
// characters that could forge extra slog fields or audit-record lines —
// are dropped. The result is empty when the input is absent or contains
// no allow-listed characters, which the caller treats as "no session"
// (falling back to the loopback service actor). Truncation happens after
// filtering so a long run of disallowed bytes cannot crowd out the
// allowed suffix.
func sanitizeSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, c := range raw {
		if b.Len() >= maxSessionIDLen {
			break
		}
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			b.WriteRune(c)
		default:
			// Drop anything else (whitespace, control bytes, punctuation,
			// non-ASCII) so no CR/LF or other injection reaches the slog
			// line or the audit payload.
		}
	}
	return b.String()
}

// vaultCredentialActor resolves the requester for a per-agent
// credential operation. When the optional session header is present it
// attributes the operation to that launch session; otherwise it falls
// back to the loopback service actor. The header is untrusted, so it is
// sanitized at this single read site (the only place it enters the audit
// surface); both the actor ID and the returned sessionID derive from the
// already-bounded value. The returned sessionID is empty unless the
// header supplied an allow-listed value, so callers can decide whether
// to stamp `aileron.session.id` into the payload.
func vaultCredentialActor(r *http.Request) (actor model.ActorRef, sessionID string) {
	if r != nil {
		sessionID = sanitizeSessionID(r.Header.Get(vaultCredentialSessionHeader))
	}
	if sessionID != "" {
		return model.ActorRef{Type: model.ActorTypeService, ID: sessionID}, sessionID
	}
	return model.ActorRef{Type: model.ActorTypeService, ID: vaultCredentialLoopbackActorID}, ""
}

// emitVaultCredentialEvent records an audit event and a session-log line
// for a per-agent credential verb (ADR-0010 + ADR-0011). errClass is
// empty on the success path and carries the handler's error code (e.g.
// `vault_locked`, `vault_not_found`) on failure. The credential bytes
// are never passed to this function, so they cannot reach the payload or
// the log line. The audit call is a no-op when no recorder is wired, but
// the slog line always fires so a nil-recorder daemon still leaves a
// session-log signal.
func (s *apiServer) emitVaultCredentialEvent(ctx context.Context, eventType model.EventType, name string, actor model.ActorRef, sessionID, errClass string) {
	logArgs := []any{
		"event", string(eventType),
		"agent", name,
		"actor", actor.ID,
	}
	if sessionID != "" {
		logArgs = append(logArgs, "session", sessionID)
	}
	if errClass != "" {
		logArgs = append(logArgs, "error_class", errClass)
	}
	if s.log != nil {
		if errClass != "" {
			s.log.Info("vault agent credential operation failed", logArgs...)
		} else {
			s.log.Info("vault agent credential operation", logArgs...)
		}
	}

	if s.auditRecorder == nil {
		return
	}
	payload := map[string]any{
		"aileron.vault.agent": name,
	}
	if sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	if errClass != "" {
		payload["aileron.failure.class"] = errClass
	}
	s.auditRecorder.RecordSuccess(ctx, eventType, actor, payload)
}

// agentCredentialVaultPath is the canonical vault path scheme for
// per-agent OAuth/credential envelopes. The HTTP path uses the
// stable URL word `credentials`; the vault stays on `oauth` so
// existing `aileron vault list`-style tooling continues to surface
// entries by their established key. Plan-pinned (ADR-0025 path
// scheme: `agents/<name>/<purpose>`).
func agentCredentialVaultPath(name string) string {
	return "agents/" + name + "/oauth"
}

// agentNameFromVaultPath is the inverse of agentCredentialVaultPath:
// it extracts the agent name from an `agents/<name>/oauth` vault path.
// It returns false for any path that does not match the scheme so the
// list filter ignores non-agent entries (secrets, bindings, etc.).
func agentNameFromVaultPath(path string) (string, bool) {
	const prefix = "agents/"
	const suffix = "/oauth"
	// Guard the length before slicing: "agents/oauth" passes both
	// HasPrefix and HasSuffix but the prefix and suffix overlap, so
	// path[len(prefix):len(path)-len(suffix)] would slice out of range.
	if len(path) < len(prefix)+len(suffix) ||
		!strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	name := path[len(prefix) : len(path)-len(suffix)]
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// GetAgentCredentials returns the per-agent credential envelope for
// the launcher's Render lifecycle. Per ADR-0011/0012 the daemon owns
// the unlocked vault; the launcher never opens the file itself.
//
// Error envelopes are named (`vault_not_found`, `vault_locked`) so
// the launcher discriminates by code rather than status alone — a
// future status-code change would not silently re-route the
// fallthrough-to-in-container-login path documented in ADR-0025.
func (s *apiServer) GetAgentCredentials(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	actor, sessionID := vaultCredentialActor(r)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "agent name is required")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialRead, name, actor, sessionID, "invalid_request")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialRead, name, actor, sessionID, "no_local_vault")
		return
	}

	secret, err := s.vault.Get(ctx, agentCredentialVaultPath(name))
	if vault.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "vault_not_found",
			"no credential entry for agent "+name)
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialRead, name, actor, sessionID, "vault_not_found")
		return
	}
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before reading agent credentials")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialRead, name, actor, sessionID, "vault_locked")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_get_failed", err.Error())
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialRead, name, actor, sessionID, "vault_get_failed")
		return
	}

	s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialRead, name, actor, sessionID, "")
	writeJSON(w, http.StatusOK, agentCredentialsResponse(secret))
}

// PutAgentCredentials accepts a credential envelope from the
// launcher's Capture pass (on clean container exit) or from a
// pre-launch refresh hook (Codex's PreLaunchRefresh rotates the
// token against the vendor's auth server and persists the new
// bundle before starting the container per AE6).
func (s *apiServer) PutAgentCredentials(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	actor, sessionID := vaultCredentialActor(r)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "agent name is required")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialWrite, name, actor, sessionID, "invalid_request")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialWrite, name, actor, sessionID, "no_local_vault")
		return
	}

	var req api.AgentCredentials
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialWrite, name, actor, sessionID, "invalid_request")
		return
	}
	if len(req.Value) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"value is required and must be non-empty")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialWrite, name, actor, sessionID, "invalid_request")
		return
	}

	meta := agentCredentialsMetadataFromRequest(req.Metadata)
	err := s.vault.Put(ctx, agentCredentialVaultPath(name), req.Value, meta)
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before writing agent credentials")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialWrite, name, actor, sessionID, "vault_locked")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_put_failed", err.Error())
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialWrite, name, actor, sessionID, "vault_put_failed")
		return
	}

	s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialWrite, name, actor, sessionID, "")
	w.WriteHeader(http.StatusNoContent)
}

// DeleteAgentCredentials removes the per-agent credential envelope at
// `agents/<name>/oauth`. This is the operator-facing recovery path
// behind `aileron vault delete`: dropping the entry makes the next
// launch fall through to the in-container login bootstrap per ADR-0025.
//
// The store-layer Delete is idempotent across every implementation
// (it returns nil whether or not the path existed), so this handler
// performs an explicit existence check via Get before deleting. That
// is the only way to honor the 404-on-miss contract while reusing the
// existing store primitive. A locked vault makes Get return
// ErrCredentialUnavailable, which yields 423 before any delete.
func (s *apiServer) DeleteAgentCredentials(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	actor, sessionID := vaultCredentialActor(r)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "agent name is required")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialDelete, name, actor, sessionID, "invalid_request")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialDelete, name, actor, sessionID, "no_local_vault")
		return
	}

	path := agentCredentialVaultPath(name)
	// The existence check decrypts the stored credential, but the secret
	// is intentionally discarded with `_`: DELETE binds only the agent
	// name, never the loaded value. emitVaultCredentialEvent takes only
	// `name`, so the decrypted bytes can never reach an audit payload or
	// session-log line (ADR-0011: no plaintext credential in any record).
	// Do not bind or log this Get result.
	_, err := s.vault.Get(ctx, path)
	if vault.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "vault_not_found",
			"no credential entry for agent "+name)
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialDelete, name, actor, sessionID, "vault_not_found")
		return
	}
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before deleting agent credentials")
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialDelete, name, actor, sessionID, "vault_locked")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_get_failed", err.Error())
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialDelete, name, actor, sessionID, "vault_get_failed")
		return
	}

	if err := s.vault.Delete(ctx, path); err != nil {
		if errors.Is(err, vault.ErrCredentialUnavailable) {
			writeError(w, http.StatusLocked, "vault_locked",
				"unlock the vault before deleting agent credentials")
			s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialDelete, name, actor, sessionID, "vault_locked")
			return
		}
		writeError(w, http.StatusInternalServerError, "vault_delete_failed", err.Error())
		s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialDelete, name, actor, sessionID, "vault_delete_failed")
		return
	}

	s.emitVaultCredentialEvent(ctx, model.EventTypeVaultCredentialDelete, name, actor, sessionID, "")
	w.WriteHeader(http.StatusNoContent)
}

// ListAgentCredentials returns the agent names that have a credential
// envelope stored, with plaintext metadata only — never the credential
// value (ADR-0011). It works on a locked vault because List never
// decrypts a stored value (see vault.Vault.List).
func (s *apiServer) ListAgentCredentials(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		return
	}

	entries, err := s.vault.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_list_failed", err.Error())
		return
	}

	list := api.AgentCredentialsList{Agents: []api.AgentCredentialSummary{}}
	for _, e := range entries {
		name, ok := agentNameFromVaultPath(e.Path)
		if !ok {
			continue
		}
		list.Agents = append(list.Agents, api.AgentCredentialSummary{
			Name:     name,
			Metadata: agentMetadataToWire(e.Metadata),
		})
	}

	writeJSON(w, http.StatusOK, list)
}

// agentCredentialsResponse builds the JSON envelope returned by GET.
// oapi-codegen's []byte field is base64-encoded by encoding/json
// (the standard library default for byte slices), so the wire
// shape matches the spec's `format: byte` declaration.
func agentCredentialsResponse(s vault.Secret) api.AgentCredentials {
	return api.AgentCredentials{
		Value:    s.Value,
		Metadata: agentMetadataToWire(s.Metadata),
	}
}

// agentMetadataToWire maps a vault.Metadata into the wire-shaped
// optional-pointer struct shared by the GET envelope and the list
// summary. Returns nil when every field is empty so a credential with
// no metadata serializes without an empty `metadata` object.
func agentMetadataToWire(meta vault.Metadata) *api.AgentCredentialsMetadata {
	if meta.Type == "" && meta.Environment == "" && len(meta.Labels) == 0 {
		return nil
	}
	respMeta := api.AgentCredentialsMetadata{}
	if meta.Type != "" {
		t := meta.Type
		respMeta.Type = &t
	}
	if meta.Environment != "" {
		e := meta.Environment
		respMeta.Environment = &e
	}
	if len(meta.Labels) > 0 {
		labels := make(map[string]string, len(meta.Labels))
		for k, v := range meta.Labels {
			labels[k] = v
		}
		respMeta.Labels = &labels
	}
	return &respMeta
}

// agentCredentialsMetadataFromRequest is the inverse of
// agentCredentialsResponse: maps the wire-shaped optional pointers
// into the vault's plain Metadata struct. A nil request metadata
// stores a zero-value Metadata; explicit empty fields stay empty.
func agentCredentialsMetadataFromRequest(req *api.AgentCredentialsMetadata) vault.Metadata {
	if req == nil {
		return vault.Metadata{}
	}
	meta := vault.Metadata{}
	if req.Type != nil {
		meta.Type = *req.Type
	}
	if req.Environment != nil {
		meta.Environment = *req.Environment
	}
	if req.Labels != nil {
		meta.Labels = *req.Labels
	}
	return meta
}
