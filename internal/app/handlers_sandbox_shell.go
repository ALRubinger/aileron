package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

const sandboxShellDecisionReasonAllowOnly = "sandbox shell decision contract is allow-only in this implementation slice"
const sandboxShellDecisionAllow = "allow"
const sandboxShellDecisionStatusDecided = "decided"

// DecideSandboxShellCommand records the daemon-side shell mediation boundary.
// The current cut is intentionally allow-only; later #801 slices add policy,
// deny, approval-pending, and result-drain semantics before launch routes real
// shell execution through this endpoint.
func (s *apiServer) DecideSandboxShellCommand(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req api.SandboxShellDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}

	command := strings.TrimSpace(req.Command)
	if command == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "command field is required")
		return
	}

	latencyMs := time.Since(start).Milliseconds()
	auditID := s.recordSandboxShellDecision(r, req, command, sandboxShellDecisionAllow, sandboxShellDecisionReasonAllowOnly, latencyMs)
	writeJSON(w, http.StatusOK, api.SandboxShellDecisionResponse{
		Status:   sandboxShellDecisionStatusDecided,
		Decision: sandboxShellDecisionAllow,
		AuditId:  auditID,
		Reason:   ptrNonEmpty(sandboxShellDecisionReasonAllowOnly),
	})
}

func (s *apiServer) recordSandboxShellDecision(r *http.Request, req api.SandboxShellDecisionRequest, command, decision, reason string, latencyMs int64) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.shell.boundary":   "sandbox_shell",
		"aileron.shell.command":    command,
		"aileron.shell.decision":   decision,
		"aileron.shell.reason":     reason,
		"aileron.shell.latency_ms": latencyMs,
	}
	if cwd := strings.TrimSpace(valueOrEmpty(req.Cwd)); cwd != "" {
		payload["aileron.shell.cwd"] = cwd
	}
	if shell := strings.TrimSpace(valueOrEmpty(req.Shell)); shell != "" {
		payload["aileron.shell.path"] = shell
	}
	if req.Pid != nil {
		payload["aileron.shell.pid"] = *req.Pid
	}
	if req.Ppid != nil {
		payload["aileron.shell.ppid"] = *req.Ppid
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Aileron-Session-Id")); sessionID != "" {
		payload["aileron.session.id"] = sessionID
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxShellDecided,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-shell"},
		payload,
	)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
