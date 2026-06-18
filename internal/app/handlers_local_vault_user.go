package app

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

// userServiceRe is the allow-list for the `{service}` path parameter of
// the user-level credential endpoint. The service name flows straight
// into the raw vault map key (`user/<service>`) with no wrapping suffix,
// so a value containing `/`, `..`, or other path metacharacters could
// escape the `user/` namespace and reach an unrelated vault entry (e.g.
// `agents/<name>/oauth`). The pattern requires a lowercase
// alphanumeric leading character followed by lowercase alphanumerics,
// `-`, or `_`. It is anchored at both ends so any disallowed byte
// rejects the whole value.
var userServiceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validateUserService reports whether service is a safe user-level
// service identifier. It is the single validation site for the
// `{service}` path parameter and must be called before any vault path
// is constructed in every verb handler.
func validateUserService(service string) bool {
	return userServiceRe.MatchString(service)
}

// userCredentialVaultPath is the canonical vault path scheme for
// user-level (agent-independent) credential envelopes. Unlike the
// per-agent scheme (`agents/<name>/oauth`) it carries no purpose
// suffix: the credential is keyed solely by the validated service
// name. The caller MUST have passed service through validateUserService
// before reaching here.
func userCredentialVaultPath(service string) string {
	return "user/" + service
}

// emitVaultUserCredentialEvent records an audit event and a session-log
// line for a user-level credential verb. It mirrors
// emitVaultCredentialEvent but is deliberately distinct: it logs the
// `service` field and stamps the payload key `aileron.vault.user.service`
// (never `aileron.vault.agent`), so a user operation on a service named
// e.g. "github" is never mislabeled as an agent of that name. errClass is
// empty on the success path and carries the handler's error code on
// failure. The credential bytes are never passed in, so they cannot reach
// the payload or the log line. The audit call is a no-op when no recorder
// is wired, but the slog line always fires.
func (s *apiServer) emitVaultUserCredentialEvent(ctx context.Context, eventType model.EventType, service string, actor model.ActorRef, sessionID, errClass string) {
	logArgs := []any{
		"event", string(eventType),
	}
	// service is the attacker-controlled `{service}` path parameter. The
	// validation-failure handlers pass "" so the raw, unvalidated value is
	// never stamped into the slog line or the audit payload: a rejected
	// `{service}` may carry CR/LF or other control bytes that forge extra
	// log fields, or be unbounded enough to bloat the audit record. On every
	// other path service has already passed validateUserService (an anchored
	// `^[a-z0-9][a-z0-9_-]*$` allow-list), so it is safe to record. Only the
	// `failure.class` is kept for a rejected request, mirroring how the
	// untrusted session header is dropped when it sanitizes to empty.
	if service != "" {
		logArgs = append(logArgs, "service", service)
	}
	logArgs = append(logArgs, "actor", actor.ID)
	if sessionID != "" {
		logArgs = append(logArgs, "session", sessionID)
	}
	if errClass != "" {
		logArgs = append(logArgs, "error_class", errClass)
	}
	if s.log != nil {
		if errClass != "" {
			s.log.Info("vault user credential operation failed", logArgs...)
		} else {
			s.log.Info("vault user credential operation", logArgs...)
		}
	}

	if s.auditRecorder == nil {
		return
	}
	payload := map[string]any{}
	if service != "" {
		payload["aileron.vault.user.service"] = service
	}
	if sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	if errClass != "" {
		payload["aileron.failure.class"] = errClass
	}
	s.auditRecorder.RecordSuccess(ctx, eventType, actor, payload)
}

// GetUserCredentials returns the user-level credential envelope stored at
// `user/<service>`. This is the agent-independent namespace backing
// user-scoped capture (#1148) and injection (#1149).
//
// The `{service}` parameter is validated against the allow-list before any
// vault path is constructed, so a path-traversal value can never reach the
// store. Error envelopes are named (`vault_not_found`, `vault_locked`) so
// callers discriminate by code rather than status alone.
func (s *apiServer) GetUserCredentials(w http.ResponseWriter, r *http.Request, service string) {
	ctx := r.Context()
	actor, sessionID := vaultCredentialActor(r)
	if !validateUserService(service) {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"service must match ^[a-z0-9][a-z0-9_-]*$")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialRead, "", actor, sessionID, "invalid_request")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialRead, service, actor, sessionID, "no_local_vault")
		return
	}

	secret, err := s.vault.Get(ctx, userCredentialVaultPath(service))
	if vault.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "vault_not_found",
			"no credential entry for user service "+service)
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialRead, service, actor, sessionID, "vault_not_found")
		return
	}
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before reading user credentials")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialRead, service, actor, sessionID, "vault_locked")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_get_failed", err.Error())
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialRead, service, actor, sessionID, "vault_get_failed")
		return
	}

	s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialRead, service, actor, sessionID, "")
	writeJSON(w, http.StatusOK, agentCredentialsResponse(secret))
}

// PutUserCredentials writes the user-level credential envelope to
// `user/<service>`. This is the backing store for user-scoped capture
// (#1148). The `{service}` parameter is validated before any vault path
// is constructed.
func (s *apiServer) PutUserCredentials(w http.ResponseWriter, r *http.Request, service string) {
	ctx := r.Context()
	actor, sessionID := vaultCredentialActor(r)
	if !validateUserService(service) {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"service must match ^[a-z0-9][a-z0-9_-]*$")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialWrite, "", actor, sessionID, "invalid_request")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialWrite, service, actor, sessionID, "no_local_vault")
		return
	}

	var req api.AgentCredentials
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialWrite, service, actor, sessionID, "invalid_request")
		return
	}
	if len(req.Value) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"value is required and must be non-empty")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialWrite, service, actor, sessionID, "invalid_request")
		return
	}

	meta := agentCredentialsMetadataFromRequest(req.Metadata)
	err := s.vault.Put(ctx, userCredentialVaultPath(service), req.Value, meta)
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before writing user credentials")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialWrite, service, actor, sessionID, "vault_locked")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_put_failed", err.Error())
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialWrite, service, actor, sessionID, "vault_put_failed")
		return
	}

	s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialWrite, service, actor, sessionID, "")
	w.WriteHeader(http.StatusNoContent)
}

// DeleteUserCredentials removes the user-level credential envelope at
// `user/<service>`.
//
// The store-layer Delete is idempotent (it returns nil whether or not the
// path existed), so this handler performs an explicit existence check via
// Get before deleting to honor the 404-on-miss contract. A locked vault
// makes Get return ErrCredentialUnavailable, which yields 423 before any
// delete. The `{service}` parameter is validated before any vault path is
// constructed.
func (s *apiServer) DeleteUserCredentials(w http.ResponseWriter, r *http.Request, service string) {
	ctx := r.Context()
	actor, sessionID := vaultCredentialActor(r)
	if !validateUserService(service) {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"service must match ^[a-z0-9][a-z0-9_-]*$")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialDelete, "", actor, sessionID, "invalid_request")
		return
	}
	if s.vault == nil {
		writeError(w, http.StatusServiceUnavailable, "no_local_vault",
			"daemon is not configured with a vault")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialDelete, service, actor, sessionID, "no_local_vault")
		return
	}

	path := userCredentialVaultPath(service)
	// The existence check decrypts the stored credential, but the secret is
	// intentionally discarded with `_`: DELETE binds only the service name,
	// never the loaded value. emitVaultUserCredentialEvent takes only
	// `service`, so the decrypted bytes can never reach an audit payload or
	// session-log line (ADR-0011: no plaintext credential in any record). Do
	// not bind or log this Get result.
	_, err := s.vault.Get(ctx, path)
	if vault.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "vault_not_found",
			"no credential entry for user service "+service)
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialDelete, service, actor, sessionID, "vault_not_found")
		return
	}
	if errors.Is(err, vault.ErrCredentialUnavailable) {
		writeError(w, http.StatusLocked, "vault_locked",
			"unlock the vault before deleting user credentials")
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialDelete, service, actor, sessionID, "vault_locked")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "vault_get_failed", err.Error())
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialDelete, service, actor, sessionID, "vault_get_failed")
		return
	}

	if err := s.vault.Delete(ctx, path); err != nil {
		if errors.Is(err, vault.ErrCredentialUnavailable) {
			writeError(w, http.StatusLocked, "vault_locked",
				"unlock the vault before deleting user credentials")
			s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialDelete, service, actor, sessionID, "vault_locked")
			return
		}
		writeError(w, http.StatusInternalServerError, "vault_delete_failed", err.Error())
		s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialDelete, service, actor, sessionID, "vault_delete_failed")
		return
	}

	s.emitVaultUserCredentialEvent(ctx, model.EventTypeVaultUserCredentialDelete, service, actor, sessionID, "")
	w.WriteHeader(http.StatusNoContent)
}
