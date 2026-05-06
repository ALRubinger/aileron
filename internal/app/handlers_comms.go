package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// Mirrors the 9A shell-approval pattern: register an entry on the
// daemon's action-approval queue, long-poll for the user's verdict,
// dispatch on approve.
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

	sender, ok := s.listeners.Get(req.Service)
	if !ok {
		s.logCommsEvent("message_denied_no_listener", sessionID, req.Service, req.Channel, "", req.Body, "")
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("send_message: no listener for service: " + req.Service)})
		return
	}

	entry := s.actionApprovals.RegisterCommsSend(req.Service, req.Channel, req.Body, sessionID)
	decision, waitErr := entry.Wait(r.Context(), s.actionApprovalTimeout())
	if errors.Is(waitErr, approval.ErrActionApprovalTimeout) {
		s.logCommsEvent("message_denied_timeout", sessionID, req.Service, req.Channel, "", req.Body, "")
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("send_message: user did not respond before timeout")})
		return
	}
	if waitErr != nil {
		s.logCommsEvent("message_denied_error", sessionID, req.Service, req.Channel, "", req.Body, "")
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("send_message: " + waitErr.Error())})
		return
	}
	if !decision.Approved {
		s.logCommsEvent("message_denied", sessionID, req.Service, req.Channel, "", req.Body, "")
		msg := "send_message: user denied"
		if decision.Reason != "" {
			msg += ": " + decision.Reason
		}
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr(msg)})
		return
	}

	if err := sender.Send(context.Background(), comms.OutgoingMessage{
		Channel: req.Channel,
		Body:    req.Body,
	}); err != nil {
		s.logCommsEvent("message_send_failed", sessionID, req.Service, req.Channel, "", req.Body, "")
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("send_message: dispatch failed: " + err.Error())})
		return
	}
	s.logCommsEvent("message_sent", sessionID, req.Service, req.Channel, "", req.Body, "")
	writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: true})
}

// DraftCommsReply handles `POST /v1/sessions/{id}/comms/draft`.
// Looks up the original incoming message in the notify queue by
// `reply_to`, registers a [comms_draft] approval entry, blocks on the
// user's verdict, and on approve dispatches the (possibly user-edited)
// reply through the matching listener.
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
		service = ""
		channel = ""
		originalAuthor = ""
		originalBody = ""
	}

	entry := s.actionApprovals.RegisterCommsDraft(service, channel, originalAuthor, originalBody, req.Body, req.ReplyTo, sessionID)
	decision, waitErr := entry.Wait(r.Context(), s.actionApprovalTimeout())
	if errors.Is(waitErr, approval.ErrActionApprovalTimeout) {
		s.logCommsEvent("draft_denied_timeout", sessionID, service, channel, "", req.Body, req.ReplyTo)
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("draft_reply: user did not respond before timeout")})
		return
	}
	if waitErr != nil {
		s.logCommsEvent("draft_denied_error", sessionID, service, channel, "", req.Body, req.ReplyTo)
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("draft_reply: " + waitErr.Error())})
		return
	}
	if !decision.Approved {
		s.logCommsEvent("draft_discarded", sessionID, service, channel, "", req.Body, req.ReplyTo)
		msg := "draft_reply: user discarded"
		if decision.Reason != "" {
			msg += ": " + decision.Reason
		}
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr(msg)})
		return
	}

	body := req.Body
	if edited, ok := decision.EditedPayload["body"].(string); ok && edited != "" {
		body = edited
		s.logCommsEvent("draft_edited", sessionID, service, channel, "", body, req.ReplyTo)
	}
	if !found || service == "" || channel == "" {
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("draft_reply: original message is no longer available; cannot dispatch")})
		return
	}
	sender, ok := s.listeners.Get(service)
	if !ok {
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("draft_reply: no listener for service: " + service)})
		return
	}
	if err := sender.Send(context.Background(), comms.OutgoingMessage{
		Channel: channel,
		Body:    body,
	}); err != nil {
		s.logCommsEvent("draft_send_failed", sessionID, service, channel, "", body, req.ReplyTo)
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("draft_reply: dispatch failed: " + err.Error())})
		return
	}
	s.logCommsEvent("reply_sent", sessionID, service, channel, "", body, req.ReplyTo)
	writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: true})
}

// RequestCommsHTTP handles `POST /v1/sessions/{id}/comms/http`. Matches
// the URL against api_key vault entries (where `metadata.type=api_key`
// and `metadata.labels[url-pattern]` matches), registers a
// [http_request] approval entry, long-polls, and on approve issues the
// HTTP call with the matched secret injected as a Bearer token.
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
	decision, waitErr := entry.Wait(r.Context(), s.actionApprovalTimeout())
	if errors.Is(waitErr, approval.ErrActionApprovalTimeout) {
		s.logCommsEvent("http_request_denied_timeout", sessionID, req.Method, req.Url, "", body, "")
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("http_request: user did not respond before timeout")})
		return
	}
	if waitErr != nil {
		s.logCommsEvent("http_request_denied_error", sessionID, req.Method, req.Url, "", body, "")
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("http_request: " + waitErr.Error())})
		return
	}
	if !decision.Approved {
		s.logCommsEvent("http_request_denied", sessionID, req.Method, req.Url, "", body, "")
		msg := "http_request: user denied"
		if decision.Reason != "" {
			msg += ": " + decision.Reason
		}
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr(msg)})
		return
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), req.Method, req.Url, bodyReader)
	if err != nil {
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("http_request: invalid request: " + err.Error())})
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
		secret, err := s.vault.Get(r.Context(), secretName)
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
		writeJSON(w, http.StatusOK, api.CommsToolResponse{Ok: false, Error: ptr("http_request: dispatch failed: " + err.Error())})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, commsResponseLimit))
	s.logCommsEvent("http_request_sent", sessionID, req.Method, req.Url, "", string(respBody), "")
	out := api.CommsToolResponse{
		Ok: true,
		Messages: &[]api.CommsMessage{{
			Id:        fmt.Sprintf("%d", resp.StatusCode),
			Service:   "",
			Channel:   "",
			Author:    "",
			Body:      string(respBody),
			Timestamp: time.Now(),
		}},
	}
	writeJSON(w, http.StatusOK, out)
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