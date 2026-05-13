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
	"github.com/ALRubinger/aileron/internal/failure"
)

// defaultActionApprovalTimeout is how long the still-synchronous
// approval-gated paths (the comms send_message / draft_reply /
// http_request handlers in handlers_comms.go) hold their response
// open waiting for a user decision. RunAction no longer uses this —
// gated /v1/actions/{name}/run calls return 202 immediately and the
// decision wait runs in a background goroutine without a timeout
// (the user can approve / deny whenever).
//
// The comms paths follow the same shape today as RunAction did
// before the async-return contract landed; converting them to the
// new contract is a follow-up to keep this PR scoped.
const defaultActionApprovalTimeout = 5 * time.Minute

// actionApprovalTimeout returns the wait window for the synchronous
// comms-approval paths. Overridable via apiServer.actionApprovalTTL so
// tests can drive deterministic timeouts without sleeping for minutes.
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
	// 204, not 200. The webapp's JSON client deserializes every 2xx
	// it doesn't special-case, and 200 with an empty body would crash
	// it on `res.json()`. 204 is the HTTP-canonical shape for "success,
	// no body" and the client already handles it.
	w.WriteHeader(http.StatusNoContent)
}

// GetActionApprovalResult serves the agent's poll for an approval's
// status and (when available) result. Returns 404 when the approval
// id is unknown — including ids minted in a previous daemon process,
// since the queue is in-memory.
//
// Status discriminates the body: terminal statuses (completed, denied,
// failed) carry their respective outcome fields; transient statuses
// (pending_approval, running) carry only `status`.
func (s *apiServer) GetActionApprovalResult(w http.ResponseWriter, _ *http.Request, approvalID string) {
	if s.actionApprovals == nil {
		writeError(w, http.StatusServiceUnavailable, "action_approvals_disabled",
			"action-approval queue is not configured")
		return
	}
	outcome, ok := s.actionApprovals.Outcome(approvalID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found",
			"approval id is unknown")
		return
	}
	writeJSON(w, http.StatusOK, toActionApprovalResult(outcome))
}

// toActionApprovalResult shapes an approval Outcome into the API's
// ActionApprovalResult wire type. Each status maps to its discriminator
// + the relevant payload fields; transient statuses carry only Status.
//
// OutcomeApprovedNotStarted is an internal-only state — production code
// only sees it for microseconds between Decide and SetRunning, and the
// agent's surface should treat it the same as "user approved, action
// will run shortly". Mapped to OutcomeRunning on the wire.
func toActionApprovalResult(o approval.Outcome) api.ActionApprovalResult {
	status := o.Status
	if status == approval.OutcomeApprovedNotStarted {
		status = approval.OutcomeRunning
	}
	out := api.ActionApprovalResult{
		Status: api.ActionApprovalResultStatus(status),
	}
	switch o.Status {
	case approval.OutcomeCompleted:
		out.AuditId = optStr(o.AuditID)
		out.Result = optStr(o.Result)
	case approval.OutcomeDenied:
		out.Reason = optStr(o.DenyReason)
	case approval.OutcomeFailed:
		if o.Failure != nil {
			env := failureToAPIEnvelope(o.Failure)
			out.Failure = &env
		}
		// For executor-level errors with no FailureEnvelope, surface
		// the plain-text reason via Reason so callers always have
		// some non-empty signal beyond the bare "failed" status.
		if o.Failure == nil && o.ErrorMessage != "" {
			out.Reason = optStr(o.ErrorMessage)
		}
	}
	return out
}

// failureToAPIEnvelope maps the failure package's internal Envelope
// shape onto the generated API's FailureEnvelope. The two have
// identical JSON wire shapes per ADR-0010 (failure_test.go pins this);
// the duplication exists because the API types are generated and can't
// depend on the failure package directly.
func failureToAPIEnvelope(f *failure.Failure) api.FailureEnvelope {
	env := failure.ToEnvelope(f)
	out := api.FailureEnvelope{}
	out.Error.Class = api.FailureClass(env.Error.Class)
	out.Error.Boundary = api.FailureBoundary(env.Error.Boundary)
	out.Error.Message = env.Error.Message
	out.Error.Retriable = env.Error.Retriable
	if env.Error.AuditID != "" {
		id := env.Error.AuditID
		out.Error.AuditId = &id
	}
	if len(env.Error.Details) > 0 {
		details := env.Error.Details
		anyMap := map[string]any(details)
		out.Error.Details = &anyMap
	}
	return out
}

// optStr returns nil for an empty string and a pointer otherwise, for
// the API's optional string fields.
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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
	if a.Preview != nil {
		out.Preview = previewToAPI(a.Preview)
	}
	if len(a.InputFields) > 0 {
		out.InputFields = inputFieldsToAPI(a.InputFields)
	}
	return out
}

// inputFieldsToAPI marshals the queue-side input-fields slice onto
// the API surface shape. The two types are structurally identical;
// the per-field per-pointer wrapping mirrors previewToAPI so the API
// surface omits empty strings and zero booleans rather than emitting
// them as wire fields the webapp would have to special-case.
func inputFieldsToAPI(fields []approval.ActionApprovalPreviewField) *[]api.ActionApprovalPreviewField {
	if len(fields) == 0 {
		return nil
	}
	out := make([]api.ActionApprovalPreviewField, len(fields))
	for i, f := range fields {
		af := api.ActionApprovalPreviewField{Label: f.Label}
		if f.Value != "" {
			v := f.Value
			af.Value = &v
		}
		if f.Missing {
			missing := true
			af.Missing = &missing
		}
		if f.Multiline {
			multiline := true
			af.Multiline = &multiline
		}
		out[i] = af
	}
	return &out
}

// previewToAPI marshals an internal preview record onto the API
// surface shape. Returns nil when the input is nil so callers can pass
// the result through to `PendingActionApproval.Preview` without a
// surrounding nil check.
func previewToAPI(p *approval.ActionApprovalPreview) *api.ActionApprovalPreview {
	if p == nil {
		return nil
	}
	out := api.ActionApprovalPreview{}
	if p.Unavailable != "" {
		u := p.Unavailable
		out.Unavailable = &u
	}
	if len(p.Fields) > 0 {
		fields := make([]api.ActionApprovalPreviewField, len(p.Fields))
		for i, f := range p.Fields {
			af := api.ActionApprovalPreviewField{Label: f.Label}
			if f.Value != "" {
				v := f.Value
				af.Value = &v
			}
			if f.Missing {
				missing := true
				af.Missing = &missing
			}
			if f.Multiline {
				multiline := true
				af.Multiline = &multiline
			}
			fields[i] = af
		}
		out.Fields = &fields
	}
	return &out
}
