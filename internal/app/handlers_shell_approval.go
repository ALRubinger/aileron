package app

import (
	"net/http"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
)

// RequestShellApproval handles `POST /v1/sessions/{session_id}/approvals/shell`,
// the daemon-side replacement for the per-launch unix-socket approval
// server (`/tmp/ai-{sessionID}.sock`) that the launcher used to bind.
//
// `aileron-sh` POSTs here when a shell command matches an `ask:` policy
// rule. The handler:
//
//  1. Registers a shell-kind entry on the daemon's action-approval
//     queue (the same queue that powers `/v1/action-approvals` for the
//     webapp), stamping the session id and cwd.
//  2. Blocks on the user's decision via [approval.ActionApproval.Wait]
//     up to the action-approval TTL (default 5 min).
//  3. Maps the decision through [approval.WireDecision] into the four-
//     option vocabulary `aileron-sh` understands and returns it.
//
// Any error path (timeout, ctx cancelled, missing queue) collapses to
// `deny` — the policy-enforced shell fails closed, never silently
// approves.
func (s *apiServer) RequestShellApproval(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.actionApprovals == nil {
		writeError(w, http.StatusServiceUnavailable, "action_approvals_disabled",
			"action-approval queue is not configured")
		return
	}

	var req api.ShellApprovalRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "missing_command", "command is required")
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}
	cwd := ""
	if req.Cwd != nil {
		cwd = *req.Cwd
	}

	entry := s.actionApprovals.RegisterShell(req.Command, reason, sessionID, cwd)
	decision, waitErr := entry.Wait(r.Context(), s.actionApprovalTimeout())

	wire := approval.WireDecision(decision, waitErr)
	writeJSON(w, http.StatusOK, api.ShellApprovalResponse{
		Decision: api.ShellApprovalResponseDecision(wire),
	})
}
