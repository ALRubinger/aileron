package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/comms"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// CommsRequest is sent by aileron-mcp to read or send messages.
// Wire shape preserved across the pty-removal in #419 — aileron-mcp's
// tool definitions don't change. The behaviors of `send_message`,
// `draft_reply`, and `http_request` regressed from "in-pty user
// approval prompt" to "deny pending webapp wire-through" (tracked
// as a #419 follow-up); the protocol itself is unchanged.
type CommsRequest struct {
	Method  string `json:"method"` // "read_messages", "send_message", "draft_reply", or "http_request"
	Service string `json:"service,omitempty"`
	Channel string `json:"channel,omitempty"`
	Body    string `json:"body,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`
}

// CommsResponse is returned to aileron-mcp.
type CommsResponse struct {
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Messages []CommsMessageDTO `json:"messages,omitempty"`
}

// CommsMessageDTO is the wire format for a message.
type CommsMessageDTO struct {
	ID           string `json:"id"`
	Service      string `json:"service"`
	Channel      string `json:"channel"`
	Author       string `json:"author"`
	Body         string `json:"body"`
	Timestamp    string `json:"timestamp"`
	DraftRequest bool   `json:"draft_request,omitempty"`
}

// CommsServer handles IPC requests from aileron-mcp.
//
// `read_messages` is a pure queue read. `send_message`, `draft_reply`,
// and `http_request` register an entry on the shared
// [approval.ActionApprovalQueue] so the webapp's `/approvals` page
// surfaces the request and the user decides; on Approve the server
// dispatches the actual send / HTTP call, on Deny it returns an
// agent-facing error. The shape replaces the pre-#419 in-pty overlay
// with the (#418) webapp surface (#428).
//
// Approvals is nil only in legacy callers that haven't been updated
// to the queue-aware constructor; in that fallback the send-shaped
// methods fail-closed with a clear regression message so the agent
// learns to route around them.
type CommsServer struct {
	socketPath  string
	listener    net.Listener
	queue       *NotifyQueue
	senders     map[string]comms.Listener
	auditLog    string
	sessionID   string
	secrets     launchpolicy.SecretsConfig
	vault       vault.Vault
	httpClient  *http.Client
	approvals   *approval.ActionApprovalQueue
	approvalTTL time.Duration

	mu   sync.Mutex
	done bool
}

// NewCommsServer creates a comms IPC server. Run in a goroutine via Serve.
//
// approvals is the shared action-approval queue the daemon also
// exposes via `/v1/action-approvals`. Send-shaped tool calls
// (`send_message`, `draft_reply`, `http_request`) register kind-
// specific entries here and wait for the user to decide via the
// webapp. Pass nil for the legacy fail-closed behaviour preserved
// for callers that don't yet route through the queue.
func NewCommsServer(socketPath string, queue *NotifyQueue, senders []comms.Listener, auditLog, sessionID string, approvals *approval.ActionApprovalQueue) (*CommsServer, error) {
	os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("comms socket: %w", err)
	}

	senderMap := make(map[string]comms.Listener, len(senders))
	for _, s := range senders {
		senderMap[s.Service()] = s
	}

	return &CommsServer{
		socketPath:  socketPath,
		listener:    ln,
		queue:       queue,
		senders:     senderMap,
		auditLog:    auditLog,
		sessionID:   sessionID,
		approvals:   approvals,
		approvalTTL: defaultCommsApprovalTimeout,
	}, nil
}

// defaultCommsApprovalTimeout is how long a send-shaped CommsServer
// method holds the IPC response open waiting for the user to decide
// in the webapp. Matches the apiServer's RunAction timeout (5
// minutes) so all kinds of pending approvals share the same upper
// bound — predictable for the user and bounded for upstream MCP /
// HTTP timeouts.
const defaultCommsApprovalTimeout = 5 * time.Minute

// Serve accepts connections. Blocks until Close. Run in a goroutine.
func (cs *CommsServer) Serve() {
	for {
		conn, err := cs.listener.Accept()
		if err != nil {
			cs.mu.Lock()
			done := cs.done
			cs.mu.Unlock()
			if done {
				return
			}
			continue
		}
		go cs.handleConn(conn)
	}
}

func (cs *CommsServer) handleConn(conn net.Conn) {
	defer conn.Close()

	var req CommsRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(CommsResponse{Error: "invalid request"})
		return
	}

	var resp CommsResponse
	switch req.Method {
	case "read_messages":
		resp = cs.readMessages(req)
	case "send_message":
		resp = cs.sendMessage(req)
	case "draft_reply":
		resp = cs.draftReply(req)
	case "http_request":
		resp = cs.httpRequest(req)
	default:
		resp = CommsResponse{Error: "unknown method: " + req.Method}
	}

	json.NewEncoder(conn).Encode(resp)
}

// readMessages returns messages currently in the notification queue.
// Filters by service and channel when provided. Marks messages read
// after surfacing them so the agent doesn't see the same message
// twice across consecutive calls.
func (cs *CommsServer) readMessages(req CommsRequest) CommsResponse {
	msgs := cs.queue.Messages()
	var dtos []CommsMessageDTO
	for _, m := range msgs {
		if req.Service != "" && m.Source != req.Service {
			continue
		}
		if req.Channel != "" && m.Channel != req.Channel {
			continue
		}
		dto := CommsMessageDTO{
			ID:        m.ID,
			Service:   m.Source,
			Channel:   m.Channel,
			Author:    m.Author,
			Body:      m.Body,
			Timestamp: m.Timestamp.Format(time.RFC3339),
		}
		if m.AutoDraft && m.Draft == "" {
			dto.DraftRequest = true
		}
		dtos = append(dtos, dto)
	}
	cs.queue.MarkAllRead()
	return CommsResponse{OK: true, Messages: dtos}
}

// sendMessage registers a [approval.ApprovalKindCommsSend] entry on
// the shared action-approval queue, blocks until the user decides
// via the webapp, and on Approve dispatches the message via the
// matched [comms.Listener]. The IPC response holds open for up to
// [CommsServer.approvalTTL]; on Deny / Timeout / context cancel the
// agent receives a structured error (#428).
func (cs *CommsServer) sendMessage(req CommsRequest) CommsResponse {
	if req.Service == "" || req.Channel == "" || req.Body == "" {
		return CommsResponse{Error: "service, channel, and body are required"}
	}
	sender, ok := cs.senders[req.Service]
	if !ok {
		return CommsResponse{Error: "no listener for service: " + req.Service}
	}
	if cs.approvals == nil {
		cs.logMessage("message_denied_no_surface", req.Service, req.Channel, "", req.Body, "")
		return CommsResponse{Error: errSendApprovalUnavailable}
	}

	entry := cs.approvals.RegisterCommsSend(req.Service, req.Channel, req.Body, cs.sessionID)
	decision, err := entry.Wait(context.Background(), cs.approvalTTL)
	if errors.Is(err, approval.ErrActionApprovalTimeout) {
		cs.logMessage("message_denied_timeout", req.Service, req.Channel, "", req.Body, "")
		return CommsResponse{Error: "send_message: user did not respond before timeout"}
	}
	if err != nil {
		cs.logMessage("message_denied_error", req.Service, req.Channel, "", req.Body, "")
		return CommsResponse{Error: "send_message: " + err.Error()}
	}
	if !decision.Approved {
		cs.logMessage("message_denied", req.Service, req.Channel, "", req.Body, "")
		msg := "send_message: user denied"
		if decision.Reason != "" {
			msg += ": " + decision.Reason
		}
		return CommsResponse{Error: msg}
	}

	if sendErr := sender.Send(context.Background(), comms.OutgoingMessage{
		Channel: req.Channel,
		Body:    req.Body,
	}); sendErr != nil {
		cs.logMessage("message_send_failed", req.Service, req.Channel, "", req.Body, "")
		return CommsResponse{Error: "send_message: dispatch failed: " + sendErr.Error()}
	}
	cs.logMessage("message_sent", req.Service, req.Channel, "", req.Body, "")
	return CommsResponse{OK: true}
}

// draftReply registers a [approval.ApprovalKindCommsDraft] entry —
// the user reviews the original incoming message alongside the
// proposed reply, optionally edits the reply body, and approves or
// discards. On approve the (possibly edited) body is dispatched
// through the listener for the same service/channel as the original
// message; on discard the agent receives a structured error.
//
// The original message is looked up in the [NotifyQueue] by
// [CommsRequest.ReplyTo]. If the message is no longer in the queue
// (TTL'd / cleared) the user can still approve the draft body in
// isolation; the dispatch path then surfaces an error rather than
// guessing at routing (#428).
func (cs *CommsServer) draftReply(req CommsRequest) CommsResponse {
	if req.ReplyTo == "" || req.Body == "" {
		return CommsResponse{Error: "reply_to and body are required"}
	}
	if cs.approvals == nil {
		cs.logMessage("draft_denied_no_surface", "", "", "", req.Body, req.ReplyTo)
		return CommsResponse{Error: errSendApprovalUnavailable}
	}

	original, found := cs.queue.FindByID(req.ReplyTo)
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

	entry := cs.approvals.RegisterCommsDraft(service, channel, originalAuthor, originalBody, req.Body, req.ReplyTo, cs.sessionID)
	decision, err := entry.Wait(context.Background(), cs.approvalTTL)
	if errors.Is(err, approval.ErrActionApprovalTimeout) {
		cs.logMessage("draft_denied_timeout", service, channel, "", req.Body, req.ReplyTo)
		return CommsResponse{Error: "draft_reply: user did not respond before timeout"}
	}
	if err != nil {
		cs.logMessage("draft_denied_error", service, channel, "", req.Body, req.ReplyTo)
		return CommsResponse{Error: "draft_reply: " + err.Error()}
	}
	if !decision.Approved {
		cs.logMessage("draft_discarded", service, channel, "", req.Body, req.ReplyTo)
		msg := "draft_reply: user discarded"
		if decision.Reason != "" {
			msg += ": " + decision.Reason
		}
		return CommsResponse{Error: msg}
	}

	body := req.Body
	if edited, ok := decision.EditedPayload["body"].(string); ok && edited != "" {
		body = edited
		cs.logMessage("draft_edited", service, channel, "", body, req.ReplyTo)
	}
	if !found || service == "" || channel == "" {
		return CommsResponse{Error: "draft_reply: original message is no longer available; cannot dispatch"}
	}
	sender, ok := cs.senders[service]
	if !ok {
		return CommsResponse{Error: "draft_reply: no listener for service: " + service}
	}
	if sendErr := sender.Send(context.Background(), comms.OutgoingMessage{
		Channel: channel,
		Body:    body,
	}); sendErr != nil {
		cs.logMessage("draft_send_failed", service, channel, "", body, req.ReplyTo)
		return CommsResponse{Error: "draft_reply: dispatch failed: " + sendErr.Error()}
	}
	cs.logMessage("reply_sent", service, channel, "", body, req.ReplyTo)
	return CommsResponse{OK: true}
}

// httpRequest registers an [approval.ApprovalKindHTTPRequest] entry
// with the matched api_key binding name (never the value), waits for
// the user to decide via the webapp, and on Approve issues the HTTP
// call with the binding's bytes injected as a Bearer token.
//
// CommsRequest field reuse (preserved from pre-#419 for protocol
// continuity): Service = HTTP method, Channel = URL, Body = request
// body, ReplyTo = a JSON object of additional headers (#428).
func (cs *CommsServer) httpRequest(req CommsRequest) CommsResponse {
	method := req.Service
	url := req.Channel
	body := req.Body
	headersJSON := req.ReplyTo
	if method == "" || url == "" {
		return CommsResponse{Error: "method and url are required"}
	}
	if cs.approvals == nil {
		cs.logMessage("http_request_denied_no_surface", method, url, "", body, "")
		return CommsResponse{Error: errSendApprovalUnavailable}
	}

	secretName, _ := cs.matchSecret(url)

	entry := cs.approvals.RegisterHTTPRequest(method, url, body, secretName, cs.sessionID)
	decision, err := entry.Wait(context.Background(), cs.approvalTTL)
	if errors.Is(err, approval.ErrActionApprovalTimeout) {
		cs.logMessage("http_request_denied_timeout", method, url, "", body, "")
		return CommsResponse{Error: "http_request: user did not respond before timeout"}
	}
	if err != nil {
		cs.logMessage("http_request_denied_error", method, url, "", body, "")
		return CommsResponse{Error: "http_request: " + err.Error()}
	}
	if !decision.Approved {
		cs.logMessage("http_request_denied", method, url, "", body, "")
		msg := "http_request: user denied"
		if decision.Reason != "" {
			msg += ": " + decision.Reason
		}
		return CommsResponse{Error: msg}
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	httpReq, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return CommsResponse{Error: "http_request: invalid request: " + err.Error()}
	}
	if headersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err == nil {
			for k, v := range headers {
				httpReq.Header.Set(k, v)
			}
		}
	}
	if secretName != "" && cs.vault != nil {
		secret, err := cs.vault.Get(context.Background(), secretName)
		if err == nil {
			httpReq.Header.Set("Authorization", "Bearer "+string(secret.Value))
		}
	}

	client := cs.httpClient
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return CommsResponse{Error: "http_request: dispatch failed: " + err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	cs.logMessage("http_request_sent", method, url, "", string(respBody), "")
	return CommsResponse{
		OK: true,
		Messages: []CommsMessageDTO{{
			ID:   fmt.Sprintf("%d", resp.StatusCode),
			Body: string(respBody),
		}},
	}
}

// matchSecret finds the first secret whose target patterns match the
// URL. The match is best-effort and prefix-based; multiple matches
// resolve in iteration order. Returns the binding name (not the
// value) so the user surface can show "credential: <name>" without
// exposing bytes.
func (cs *CommsServer) matchSecret(url string) (string, bool) {
	for name, def := range cs.secrets {
		for _, pattern := range def.Targets {
			if URLMatchesPattern(url, pattern) {
				return name, true
			}
		}
	}
	return "", false
}

// URLMatchesPattern checks if a URL matches a target pattern.
// Patterns use simple containment with optional trailing wildcard.
// Examples:
//
//   - "slack.com/api/*" matches "https://slack.com/api/chat.postMessage".
//   - "api.github.com" matches "https://api.github.com/repos/foo/bar".
//
// Exact regex / glob expressiveness is out of scope; the pre-#419
// code shipped this exact matcher and its behaviour is what
// `aileron-mcp`'s `http_request` tool descriptions promise.
func URLMatchesPattern(url, pattern string) bool {
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.Contains(url, prefix)
	}
	return strings.Contains(url, pattern)
}

// errSendApprovalUnavailable is the agent-facing message when a
// send-shaped CommsRequest hits a CommsServer that wasn't given an
// [approval.ActionApprovalQueue] — the legacy fallback for callers
// that haven't been updated. Production wiring under `aileron launch`
// always supplies the queue, so this surface is reachable only by
// stale code paths or tests that don't construct the queue.
const errSendApprovalUnavailable = "send / draft / http_request approval surface is unavailable: comms server has no action-approval queue wired (production launch always supplies one — this is a legacy / test fallback)"

// DirectSend sends a message without an approval prompt. Used by
// downstream callers that have already received explicit user
// authorization via some other surface. Pre-#419, the in-pty overlay
// drove this; under the new launch path nothing currently calls
// DirectSend, but the method is preserved so any future webapp-driven
// reply path can dispatch through it without re-implementing send
// plumbing.
func (cs *CommsServer) DirectSend(service, channel, body string) error {
	sender, ok := cs.senders[service]
	if !ok {
		return fmt.Errorf("no listener for service: %s", service)
	}
	err := sender.Send(context.Background(), comms.OutgoingMessage{
		Channel: channel,
		Body:    body,
	})
	if err == nil {
		cs.logMessage("reply_sent", service, channel, "", body, "")
	}
	return err
}

// SetSecrets configures the secrets mapping and vault for http_request
// credential injection. Currently unused — httpRequest fail-closes
// before reaching credential lookup — but preserved for the eventual
// rewire so call sites don't have to re-add it.
func (cs *CommsServer) SetSecrets(secrets launchpolicy.SecretsConfig, v vault.Vault) {
	cs.secrets = secrets
	cs.vault = v
	cs.httpClient = &http.Client{}
}

// SocketPath returns the path to the Unix socket.
func (cs *CommsServer) SocketPath() string {
	return cs.socketPath
}

// Close shuts down the comms server.
func (cs *CommsServer) Close() {
	cs.mu.Lock()
	cs.done = true
	cs.mu.Unlock()
	cs.listener.Close()
	os.Remove(cs.socketPath)
}

// RequestComms connects to the comms server and makes a request.
// Called from aileron-mcp.
func RequestComms(socketPath string, req CommsRequest) CommsResponse {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return CommsResponse{Error: "connection failed: " + err.Error()}
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return CommsResponse{Error: "encode failed: " + err.Error()}
	}

	var resp CommsResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return CommsResponse{Error: "decode failed: " + err.Error()}
	}
	return resp
}

func (cs *CommsServer) logMessage(event, service, channel, author, body, inReplyTo string) {
	if cs.auditLog == "" {
		return
	}
	audit.AppendMessageEntry(cs.auditLog, audit.MessageEntry{
		Timestamp: time.Now(),
		SessionID: cs.sessionID,
		Event:     event,
		Service:   service,
		Channel:   channel,
		Author:    author,
		Body:      body,
		InReplyTo: inReplyTo,
	})
}

