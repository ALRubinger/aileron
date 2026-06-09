package app

import (
	"encoding/json"
	"net/http"
	"strings"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/model"
)

// RecordSandboxProxyDisabled handles POST /v1/sandbox-proxy/disabled.
// It records a sandbox.proxy.disabled audit event with the supplied
// reason so the audit log captures every launch where proxy bootstrap
// is not active for the session. Idempotent record-only: returns 204
// on success, never inspects credentials, and never reaches an
// upstream service.
//
// Mirrors recordSandboxProxyRejected's payload shape for fields the
// two events share (boundary, source, session, sandbox mode/image),
// and uses a distinct disabled_reason field instead of reject_reason
// so downstream audit consumers can filter on event type alone.
func (s *apiServer) RecordSandboxProxyDisabled(w http.ResponseWriter, r *http.Request) {
	var req api.SandboxProxyDisabledRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}

	sessionID := strings.TrimSpace(req.SessionId)
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "session_id field is required")
		return
	}
	reason := strings.TrimSpace(string(req.Reason))
	if reason == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "reason field is required")
		return
	}
	if !req.Reason.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_body", "reason must be one of user_opt_out, preflight_failed, unsupported_sandbox_mode")
		return
	}

	sandboxMode := ""
	if req.SandboxMode != nil {
		sandboxMode = strings.TrimSpace(*req.SandboxMode)
	}
	sandboxImage := ""
	if req.SandboxImage != nil {
		sandboxImage = strings.TrimSpace(*req.SandboxImage)
	}

	if s.auditRecorder == nil {
		// No recorder — accept and return 204. The endpoint is the
		// audit-emission boundary; with no audit subsystem there's
		// nothing to do. Treating this as a 5xx would leak the
		// daemon's configuration shape to the launcher.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	payload := map[string]any{
		"aileron.proxy.source":          "launcher",
		"aileron.proxy.boundary":        "https_proxy",
		"aileron.proxy.decision":        "disabled",
		"aileron.proxy.disabled_reason": reason,
		"aileron.session.id":            sessionID,
	}
	if sandboxMode != "" {
		payload["aileron.sandbox.mode"] = sandboxMode
	}
	if sandboxImage != "" {
		payload["aileron.sandbox.image"] = sandboxImage
	}

	s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxProxyDisabled,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "launcher"},
		payload,
	)
	w.WriteHeader(http.StatusNoContent)
}
