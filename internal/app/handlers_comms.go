package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/vault"
)

// commsResponseLimit caps how much of an upstream HTTP response body
// /comms/http surfaces back to the agent. Matches the pre-9B per-launch
// CommsServer's limit so behaviour is unchanged for callers.
const commsResponseLimit = 1 << 20

// commsApprovalWait is the duration the background-dispatch goroutine
// passes to [approval.ActionApproval.Wait]. Mirrors the action-run
// path's "effectively forever" — the user can approve or deny on
// their own schedule; daemon shutdown drops the goroutine without
// going through ctx (in-memory queue, persistence is the #648
// follow-up).
const commsApprovalWait = time.Duration(math.MaxInt64)

// commsRunResult is the wire shape the comms goroutines JSON-encode
// into the run-record's [approval.Outcome.Result] string. Mirrors the
// pre-async `CommsToolResponse` body so any agent-side parsers that
// were reading `ok` / `error` / `messages` keep working when they
// migrate from "decode the sync 200 body" to "decode result on the
// /v1/action-approvals/{id}/result body".
type commsRunResult struct {
	Ok       bool                  `json:"ok"`
	Error    string                `json:"error,omitempty"`
	Messages []commsRunResultEntry `json:"messages,omitempty"`
}

// commsRunResultEntry is a single message in [commsRunResult]. Used
// chiefly by /comms/http to carry the upstream response — status code
// in id, body in body — mirroring the legacy CommsMessage shape.
type commsRunResultEntry struct {
	Id        string    `json:"id"`
	Service   string    `json:"service"`
	Channel   string    `json:"channel"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	Timestamp time.Time `json:"timestamp"`
}

// ReadCommsMessages handles `GET /v1/sessions/{id}/comms/messages`.
// Replaces the per-launch unix-socket `read_messages` IPC method that
// the launch product's CommsServer used to field — under ADR-0012
// step 9B-2 the daemon owns the notify queue and surfaces it via HTTP.
//
// Filters by service / channel when provided, marks every surfaced
// message as read on success, and returns the snapshot.
func (s *apiServer) ReadCommsMessages(w http.ResponseWriter, r *http.Request, sessionID string, params api.ReadCommsMessagesParams) {
	if s.notifyQueue == nil {
		writeError(w, http.StatusServiceUnavailable, "comms_disabled",
			"daemon has no comms surface configured")
		return
	}

	service := ""
	if params.Service != nil {
		service = *params.Service
	}
	channel := ""
	if params.Channel != nil {
		channel = *params.Channel
	}

	out := make([]api.CommsMessage, 0, s.notifyQueue.Len())
	for _, m := range s.notifyQueue.Messages() {
		if service != "" && m.Source != service {
			continue
		}
		if channel != "" && m.Channel != channel {
			continue
		}
		dr := m.AutoDraft && m.Draft == ""
		dto := api.CommsMessage{
			Id:        m.ID,
			Service:   m.Source,
			Channel:   m.Channel,
			Author:    m.Author,
			Body:      m.Body,
			Timestamp: m.Timestamp,
		}
		if dr {
			t := true
			dto.DraftRequest = &t
		}
		out = append(out, dto)
	}
	s.notifyQueue.MarkAllRead()
	writeJSON(w, http.StatusOK, api.ReadCommsMessagesResponse{Messages: out})
}

// SendCommsMessage handles `POST /v1/sessions/{id}/comms/send`.
// Registers a [comms_send] entry on the action-approval queue and
// returns 202 with an ActionRunPendingResponse immediately. A
// background goroutine waits for the user's decision and, on approve,
// dispatches via the matching listener and records the outcome
// against the approval id. Mirrors the contract ratified by #648 for
// approval-gated [RunAction]; agents poll
// `GET /v1/action-approvals/{id}/result` to learn what happened.
func (s *apiServer) SendCommsMessage(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !s.commsConfigured() {
		writeError(w, http.StatusServiceUnavailable, "comms_disabled",
			"daemon has no comms surface configured")
		return
	}
	if s.actionApprovals == nil {
		writeError(w, http.StatusServiceUnavailable, "action_approvals_disabled",
			"action-approval queue is not configured")
		return
	}

	var req api.SendCommsMessageRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Service == "" || req.Channel == "" || req.Body == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"service, channel, and body are required")
		return
	}
	if _, ok := s.listeners.Get(req.Service); !ok {
		// Fail fast: no listener means there's no point asking the
		// user to approve a dispatch we already know can't happen.
		// Pre-async this came back as 200/{ok:false}; under the new
		// async contract a 400 is more honest — the request was
		// never queued at all, so the caller has no approval id to
		// poll.
		s.logCommsEvent("message_denied_no_listener", sessionID, req.Service, req.Channel, "", req.Body, "")
		writeError(w, http.StatusBadRequest, "no_listener",
			"send_message: no listener for service: "+req.Service)
		return
	}

	entry := s.actionApprovals.RegisterCommsSend(req.Service, req.Channel, req.Body, sessionID)
	go s.executeApprovedCommsSend(entry, req.Service, req.Channel, req.Body, sessionID)

	respBody := buildPendingApprovalResponse(entry.ID, "send_message", "",
		buildApprovalsReviewURL(s.webappURL, "", entry.ID))
	writeJSON(w, http.StatusAccepted, respBody)
}

// executeApprovedCommsSend is the background-dispatch goroutine for
// [SendCommsMessage]. Waits for the user's decision on the queue,
// dispatches via the matching listener on approve, and stores the
// outcome (completed / failed) against the approval id so
// `/v1/action-approvals/{id}/result` can serve it.
//
// Mirrors [executeApprovedAction]'s shape so future readers find one
// pattern across both surfaces.
func (s *apiServer) executeApprovedCommsSend(entry *approval.ActionApproval, service, channel, body, sessionID string) {
	ctx := context.Background()
	decision, waitErr := entry.Wait(ctx, commsApprovalWait)
	if waitErr != nil {
		// ctx.Err() — daemon shutting down; leave the outcome at its
		// current state. The queue is going away with us.
		return
	}
	if !decision.Approved {
		// Decide already flipped the outcome to denied; record the
		// audit-channel event for parity with the legacy synchronous
		// path so a "what did I deny today?" lookup still works.
		s.logCommsEvent("message_denied", sessionID, service, channel, "", body, "")
		return
	}

	sender, ok := s.listeners.Get(service)
	if !ok {
		// Race: listener was registered when the request landed but
		// removed before the user decided. Surface as a failure so
		// the agent sees actionable detail rather than a silent
		// terminal state.
		s.logCommsEvent("message_send_failed", sessionID, service, channel, "", body, "")
		_ = s.actionApprovals.SetFailed(entry.ID, nil,
			"send_message: no listener for service: "+service)
		return
	}
	if err := sender.Send(ctx, comms.OutgoingMessage{
		Channel: channel,
		Body:    body,
	}); err != nil {
		s.logCommsEvent("message_send_failed", sessionID, service, channel, "", body, "")
		_ = s.actionApprovals.SetFailed(entry.ID, nil,
			"send_message: dispatch failed: "+err.Error())
		return
	}
	s.logCommsEvent("message_sent", sessionID, service, channel, "", body, "")
	auditID := ""
	if s.newID != nil {
		auditID = s.newID()
	}
	_ = s.actionApprovals.SetCompleted(entry.ID, auditID,
		marshalCommsResult(commsRunResult{Ok: true}))
}

// DraftCommsReply handles `POST /v1/sessions/{id}/comms/draft`.
// Looks up the original incoming message in the notify queue by
// `reply_to`, registers a [comms_draft] entry, returns 202 with an
// ActionRunPendingResponse, and dispatches the (possibly user-edited)
// reply through the matching listener in a background goroutine once
// the user decides.
func (s *apiServer) DraftCommsReply(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !s.commsConfigured() {
		writeError(w, http.StatusServiceUnavailable, "comms_disabled",
			"daemon has no comms surface configured")
		return
	}
	if s.actionApprovals == nil {
		writeError(w, http.StatusServiceUnavailable, "action_approvals_disabled",
			"action-approval queue is not configured")
		return
	}

	var req api.DraftCommsReplyRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.ReplyTo == "" || req.Body == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"reply_to and body are required")
		return
	}

	original, found := s.notifyQueue.FindByID(req.ReplyTo)
	service := original.Source
	channel := original.Channel
	originalAuthor := original.Author
	originalBody := original.Body
	if !found {
		// Surface the empty values to the approval card so the user
		// can still see what the agent attempted; the goroutine
		// converts the missing-original into a SetFailed after the
		// user decides.
		service = ""
		channel = ""
		originalAuthor = ""
		originalBody = ""
	}

	entry := s.actionApprovals.RegisterCommsDraft(service, channel, originalAuthor, originalBody, req.Body, req.ReplyTo, sessionID)
	go s.executeApprovedCommsDraft(entry, service, channel, req.Body, req.ReplyTo, found, sessionID)

	respBody := buildPendingApprovalResponse(entry.ID, "draft_reply", "",
		buildApprovalsReviewURL(s.webappURL, "", entry.ID))
	writeJSON(w, http.StatusAccepted, respBody)
}

// executeApprovedCommsDraft is the background-dispatch goroutine for
// [DraftCommsReply]. Handles the user-edited-body case (the queue
// stamps the edited bytes onto [approval.ActionDecision.EditedPayload]
// when the user approved via the webapp's draft card) and the
// original-message-evicted case (SetFailed with a descriptive error).
func (s *apiServer) executeApprovedCommsDraft(entry *approval.ActionApproval, service, channel, originalBody, replyTo string, originalFound bool, sessionID string) {
	ctx := context.Background()
	decision, waitErr := entry.Wait(ctx, commsApprovalWait)
	if waitErr != nil {
		return
	}
	if !decision.Approved {
		s.logCommsEvent("draft_discarded", sessionID, service, channel, "", originalBody, replyTo)
		return
	}

	body := originalBody
	if edited, ok := decision.EditedPayload["body"].(string); ok && edited != "" {
		body = edited
		s.logCommsEvent("draft_edited", sessionID, service, channel, "", body, replyTo)
	}
	if !originalFound || service == "" || channel == "" {
		_ = s.actionApprovals.SetFailed(entry.ID, nil,
			"draft_reply: original message is no longer available; cannot dispatch")
		return
	}
	sender, ok := s.listeners.Get(service)
	if !ok {
		_ = s.actionApprovals.SetFailed(entry.ID, nil,
			"draft_reply: no listener for service: "+service)
		return
	}
	if err := sender.Send(ctx, comms.OutgoingMessage{
		Channel: channel,
		Body:    body,
	}); err != nil {
		s.logCommsEvent("draft_send_failed", sessionID, service, channel, "", body, replyTo)
		_ = s.actionApprovals.SetFailed(entry.ID, nil,
			"draft_reply: dispatch failed: "+err.Error())
		return
	}
	s.logCommsEvent("reply_sent", sessionID, service, channel, "", body, replyTo)
	auditID := ""
	if s.newID != nil {
		auditID = s.newID()
	}
	_ = s.actionApprovals.SetCompleted(entry.ID, auditID,
		marshalCommsResult(commsRunResult{Ok: true}))
}

// RequestCommsHTTP handles `POST /v1/sessions/{id}/comms/http`. Matches
// the URL against api_key vault entries (where `metadata.type=api_key`
// and `metadata.labels[url-pattern]` matches), registers a
// [http_request] entry, returns 202 with an ActionRunPendingResponse,
// and issues the HTTP call with the matched secret injected as a
// Bearer token in a background goroutine once the user decides.
//
// `commsConfigured()` is intentionally NOT checked here: `http_request`
// has no listener dependency. It only needs the vault for credential
// injection (and even that is optional). 503 here would surprise an
// agent that uses `http_request` in a launch where the user hasn't
// configured Slack/Discord.
func (s *apiServer) RequestCommsHTTP(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.actionApprovals == nil {
		writeError(w, http.StatusServiceUnavailable, "action_approvals_disabled",
			"action-approval queue is not configured")
		return
	}

	var req api.RequestCommsHTTPRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Method == "" || req.Url == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"method and url are required")
		return
	}

	body := ""
	if req.Body != nil {
		body = *req.Body
	}
	headersJSON := ""
	if req.Headers != nil {
		headersJSON = *req.Headers
	}

	secretName, _ := s.matchAPIKeyForURL(r.Context(), req.Url)

	entry := s.actionApprovals.RegisterHTTPRequest(req.Method, req.Url, body, secretName, sessionID)
	go s.executeApprovedCommsHTTP(entry, req.Method, req.Url, body, headersJSON, secretName, sessionID)

	respBody := buildPendingApprovalResponse(entry.ID, "http_request", "",
		buildApprovalsReviewURL(s.webappURL, "", entry.ID))
	writeJSON(w, http.StatusAccepted, respBody)
}

// executeApprovedCommsHTTP is the background-dispatch goroutine for
// [RequestCommsHTTP]. Issues the HTTP call after approval and stores
// the upstream response (status code + body, capped at
// [commsResponseLimit]) against the approval id.
func (s *apiServer) executeApprovedCommsHTTP(entry *approval.ActionApproval, method, urlStr, body, headersJSON, secretName, sessionID string) {
	ctx := context.Background()
	decision, waitErr := entry.Wait(ctx, commsApprovalWait)
	if waitErr != nil {
		return
	}
	if !decision.Approved {
		s.logCommsEvent("http_request_denied", sessionID, method, urlStr, "", body, "")
		return
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		_ = s.actionApprovals.SetFailed(entry.ID, nil,
			"http_request: invalid request: "+err.Error())
		return
	}
	if headersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err == nil {
			for k, v := range headers {
				httpReq.Header.Set(k, v)
			}
		}
	}
	if secretName != "" && s.vault != nil {
		secret, err := s.vault.Get(ctx, secretName)
		if err == nil {
			httpReq.Header.Set("Authorization", "Bearer "+string(secret.Value))
		}
	}

	client := s.commsHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		_ = s.actionApprovals.SetFailed(entry.ID, nil,
			"http_request: dispatch failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, commsResponseLimit))
	s.logCommsEvent("http_request_sent", sessionID, method, urlStr, "", string(respBody), "")
	auditID := ""
	if s.newID != nil {
		auditID = s.newID()
	}
	_ = s.actionApprovals.SetCompleted(entry.ID, auditID,
		marshalCommsResult(commsRunResult{
			Ok: true,
			Messages: []commsRunResultEntry{{
				Id:        fmt.Sprintf("%d", resp.StatusCode),
				Body:      string(respBody),
				Timestamp: time.Now(),
			}},
		}))
}

// marshalCommsResult JSON-encodes a [commsRunResult] for storage in
// the approval queue's [approval.Outcome.Result] string. Encoding
// failures fall back to a minimal error envelope so the result field
// is never empty when the goroutine reached this point.
func marshalCommsResult(r commsRunResult) string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"ok":false,"error":"comms result marshal failed"}`
	}
	return string(b)
}

// commsConfigured reports whether the daemon has a usable comms
// surface — both the queue and the listener registry must be wired.
// /comms/http stands apart since it doesn't need listeners.
func (s *apiServer) commsConfigured() bool {
	return s.notifyQueue != nil && s.listeners != nil
}

// matchAPIKeyForURL scans the vault for api_key entries whose
// `url-pattern` label matches url, returning the first match's name.
// The match is intentionally simple — substring or trailing-wildcard,
// matching the pre-9B per-launch CommsServer's URLMatchesPattern so
// the agent surface stays consistent.
func (s *apiServer) matchAPIKeyForURL(ctx context.Context, url string) (string, bool) {
	if s.vault == nil {
		return "", false
	}
	entries, err := s.vault.List(ctx)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.Metadata.Type != "api_key" {
			continue
		}
		pattern := e.Metadata.Labels["url-pattern"]
		if pattern == "" {
			continue
		}
		if urlMatchesPattern(url, pattern) {
			return e.Path, true
		}
	}
	return "", false
}

// urlMatchesPattern reports whether url contains pattern, with an
// optional trailing wildcard. Examples:
//
//	"slack.com/api/*"  matches "https://slack.com/api/chat.postMessage".
//	"api.github.com"   matches "https://api.github.com/repos/foo/bar".
//
// Exact regex / glob expressiveness is deliberately out of scope; the
// pre-9B per-launch CommsServer shipped this exact matcher.
func urlMatchesPattern(url, pattern string) bool {
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.Contains(url, prefix)
	}
	return strings.Contains(url, pattern)
}

// logCommsEvent writes a single message audit entry. Best-effort: a
// failed write is logged but does not abort the request.
func (s *apiServer) logCommsEvent(event, sessionID, service, channel, author, body, inReplyTo string) {
	if s.auditStateDir == "" {
		return
	}
	if err := audit.AppendMessageEntry(audit.DailyPath(s.auditStateDir), audit.MessageEntry{
		Timestamp: time.Now(),
		SessionID: sessionID,
		Event:     event,
		Service:   service,
		Channel:   channel,
		Author:    author,
		Body:      body,
		InReplyTo: inReplyTo,
	}); err != nil {
		s.log.Warn("comms audit write failed", "event", event, "error", err)
	}
}

// Compile-time assertion that the vault.Vault interface still has
// List — kept here so a refactor of the SPI surfaces an error in the
// comms layer rather than a runtime panic.
var _ = vault.Vault.List

// ptr returns a pointer to v. Local helper to keep handler bodies
// concise when constructing pointer-typed schema fields.
func ptr[T any](v T) *T { return &v }
