package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	var editedPayload map[string]any
	if req.EditedPayload != nil {
		editedPayload = *req.EditedPayload
	}
	err := s.actionApprovals.Decide(approvalID, req.Approved, reason, editedPayload)
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

// actionApprovalSSEHeartbeat is how often the SSE handler emits a
// keep-alive comment line on an idle stream. 30s clears most idle-
// proxy timeouts (NLB, nginx default is 60s; cloudfront is 60s) with
// margin to spare, while staying quiet enough that the dev-tools
// network panel doesn't get noisy.
const actionApprovalSSEHeartbeat = 30 * time.Second

// WatchActionApprovals is the SSE stream backing the webapp's
// `/approvals` page (issue #418). The handler:
//
//   - Subscribes to the queue BEFORE rendering the snapshot, so any
//     event between snapshot and subscription registration is
//     observed (the webapp de-dupes by id; a duplicate `pending`
//     event is preferred over a missed one).
//   - Emits a single `snapshot` event with the current pending list
//     (same shape as `GET /v1/action-approvals`), then one event per
//     state change.
//   - Sends `: heartbeat\n\n` every 30s so idle proxies don't close
//     the connection.
//   - Releases the subscription and returns when the client
//     disconnects (`r.Context().Done()`) or the per-subscriber buffer
//     overflows enough to drop events. Slow consumers resync from
//     `GET /v1/action-approvals` on reconnect.
//
// The handler does not impose its own timeout; it lives as long as
// the HTTP request does. That's deliberate — the page's open SSE
// connection is the entire point of the feature.
func (s *apiServer) WatchActionApprovals(w http.ResponseWriter, r *http.Request) {
	if s.actionApprovals == nil {
		writeError(w, http.StatusServiceUnavailable, "action_approvals_disabled",
			"action-approval queue is not configured")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported",
			"response writer does not support flushing")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// X-Accel-Buffering disables nginx response buffering for this
	// connection. Harmless without nginx; load-bearing when one is in
	// front (otherwise the heartbeat doesn't reach the browser until
	// nginx flushes its own buffer).
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe before rendering the snapshot. A registration that
	// happens between the snapshot read and the subscription would
	// otherwise be missed; with this ordering the worst case is a
	// duplicate `pending` event for an item already in the snapshot,
	// which the client de-dupes by id.
	events, cancel := s.actionApprovals.Subscribe()
	defer cancel()

	pending := s.actionApprovals.List()
	items := make([]api.PendingActionApproval, 0, len(pending))
	for _, a := range pending {
		items = append(items, toPendingActionApproval(a))
	}
	if err := writeSSEEvent(w, "snapshot", map[string]any{"items": items}); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(actionApprovalSSEHeartbeat)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-events:
			if !ok {
				// Subscriber channel was closed (queue shutdown or
				// double-cancel). End the stream cleanly so the
				// browser can reconnect.
				return
			}
			switch e.Type {
			case approval.ActionApprovalEventPending:
				if e.Pending == nil {
					continue
				}
				if err := writeSSEEvent(w, "pending", toPendingActionApproval(e.Pending)); err != nil {
					return
				}
			case approval.ActionApprovalEventResolved:
				if e.Resolved == nil {
					continue
				}
				if err := writeSSEEvent(w, "resolved", toResolvedActionApproval(e.Resolved)); err != nil {
					return
				}
			default:
				// Unknown event type — skip rather than crash. Lets
				// the queue add new event types without a coordinated
				// handler/webapp release.
				continue
			}
			flusher.Flush()
		}
	}
}

// resolvedActionApprovalDTO is the wire payload for the SSE
// `resolved` event. Mirrors the OpenAPI ResolvedActionApproval schema;
// kept here rather than in the codegen output because oapi-codegen
// only emits Go types for schemas referenced from a JSON-content
// operation, and SSE event payloads have no such anchor.
type resolvedActionApprovalDTO struct {
	ID        string                `json:"id"`
	Kind      approval.ApprovalKind `json:"kind"`
	Approved  bool                  `json:"approved"`
	Reason    string                `json:"reason,omitempty"`
	DecidedAt time.Time             `json:"decided_at"`
}

func toResolvedActionApproval(r *approval.ResolvedActionApproval) resolvedActionApprovalDTO {
	kind := r.Kind
	if kind == "" {
		kind = approval.ApprovalKindAction
	}
	return resolvedActionApprovalDTO{
		ID:        r.ID,
		Kind:      kind,
		Approved:  r.Approved,
		Reason:    r.Reason,
		DecidedAt: r.DecidedAt,
	}
}

// writeSSEEvent serializes a payload as a single SSE event frame.
// SSE format is `event: <type>\ndata: <single-line>\n\n`; the data
// line MUST NOT contain a literal newline, so we marshal compact JSON.
// Each event is one frame — no chunking across data lines, which
// keeps the client's reassembly trivial.
func writeSSEEvent(w io.Writer, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	return err
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
	kind := a.Kind
	if kind == "" {
		kind = approval.ApprovalKindAction
	}
	out := api.PendingActionApproval{
		Id:          a.ID,
		Kind:        api.PendingActionApprovalKind(kind),
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
