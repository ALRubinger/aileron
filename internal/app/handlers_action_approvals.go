package app

import (
	"errors"
	"net/http"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
)

// defaultActionApprovalTimeout is how long RunAction holds the
// response open waiting for a user decision when an action is gated
// on approval. 5 minutes balances "user actually has time to read
// and decide" against "MCP/HTTP timeouts somewhere upstream don't
// kill the request."
const defaultActionApprovalTimeout = 5 * time.Minute

// actionApprovalTimeout returns the wait window for a pending
// approval. Overridable via apiServer.actionApprovalTTL so tests can
// drive deterministic timeouts without sleeping for minutes.
func (s *apiServer) actionApprovalTimeout() time.Duration {
	if s.actionApprovalTTL > 0 {
		return s.actionApprovalTTL
	}
	return defaultActionApprovalTimeout
}

// ListActionApprovals returns the queue of pending action-level approvals.
// Surfaced to the webapp ([#418]) and to the `aileron approval` CLI so
// the user (or external automation) can act on requests the runtime is
// blocked on.
//
// [#418]: https://github.com/ALRubinger/aileron/issues/418
func (s *apiServer) ListActionApprovals(w http.ResponseWriter, _ *http.Request) {
	if s.actionApprovals == nil {
		writeJSON(w, http.StatusOK, api.ActionApprovalListResponse{Items: []api.PendingActionApproval{}})
		return
	}
	pending := s.actionApprovals.List()
	items := make([]api.PendingActionApproval, 0, len(pending))
	for _, a := range pending {
		items = append(items, toPendingActionApproval(a))
	}
	writeJSON(w, http.StatusOK, api.ActionApprovalListResponse{Items: items})
}

// DecideActionApproval resolves a pending approval. The runtime's
// blocked RunAction unblocks on the next tick; on `approved = true`
// the action proceeds and returns its normal result, on
// `approved = false` it returns an `approval_denied` failure envelope.
//
// Returns 404 when the id is unknown or already resolved (the second
// click from a different webapp tab, or a CLI deciding after the
// timeout fired). The held-open RunAction is the canonical race-loser
// — its Wait returns first, the second Decide finds nothing pending.
func (s *apiServer) DecideActionApproval(w http.ResponseWriter, r *http.Request, approvalID string) {
	if s.actionApprovals == nil {
		writeError(w, http.StatusServiceUnavailable, "action_approvals_disabled",
			"action-approval queue is not configured")
		return
	}
	var req api.ActionApprovalDecision
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}
	err := s.actionApprovals.Decide(approvalID, req.Approved, reason)
	if errors.Is(err, approval.ErrActionApprovalNotFound) {
		writeError(w, http.StatusNotFound, "not_found",
			"approval id is unknown or already resolved")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "decide_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// toPendingActionApproval marshals an internal queue entry to the
// API surface shape. Args is `additionalProperties: true` in the
// schema so we just forward the map verbatim; nil maps survive the
// JSON round-trip as `{}` to keep the wire format deterministic.
func toPendingActionApproval(a *approval.ActionApproval) api.PendingActionApproval {
	args := a.Args
	if args == nil {
		args = map[string]any{}
	}
	out := api.PendingActionApproval{
		Id:          a.ID,
		ActionName:  a.ActionName,
		RequestedAt: a.RequestedAt,
	}
	if a.ConnectorFQN != "" {
		fqn := a.ConnectorFQN
		out.ConnectorFqn = &fqn
	}
	if a.SessionID != "" {
		sid := a.SessionID
		out.SessionId = &sid
	}
	out.Args = &args
	return out
}
