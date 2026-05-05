package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

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
// Pre-#419, this server rendered an in-pty approval panel before
// every send / draft / http call. The pty rendering is gone; the
// IPC interface stays so aileron-mcp's tools keep their schemas.
// Send / draft / http now fail-closed with a clear message until the
// shell-shim → webapp wire-through ships as a follow-up to #419.
// `read_messages` is unaffected — it's a pure queue read.
type CommsServer struct {
	socketPath string
	listener   net.Listener
	queue      *NotifyQueue
	senders    map[string]comms.Listener
	auditLog   string
	sessionID  string
	secrets    launchpolicy.SecretsConfig
	vault      vault.Vault
	httpClient *http.Client

	mu   sync.Mutex
	done bool
}

// NewCommsServer creates a comms IPC server. Run in a goroutine via Serve.
func NewCommsServer(socketPath string, queue *NotifyQueue, senders []comms.Listener, auditLog, sessionID string) (*CommsServer, error) {
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
		socketPath: socketPath,
		listener:   ln,
		queue:      queue,
		senders:    senderMap,
		auditLog:   auditLog,
		sessionID:  sessionID,
	}, nil
}

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

// sendMessage previously prompted the developer for approval in the
// pty before dispatching. With the pty gone (#419), the prompt
// surface is gone too. Fail-closed until the webapp wire-through
// lands — agents calling this tool see the error and can fall back
// to other mechanisms.
func (cs *CommsServer) sendMessage(req CommsRequest) CommsResponse {
	cs.logMessage("message_denied_no_surface", req.Service, req.Channel, "", req.Body, "")
	return CommsResponse{Error: errSendApprovalUnavailable}
}

// draftReply previously polled the in-pty overlay for an approve /
// edit / discard decision. Same pty-removal regression as sendMessage:
// fail-closed until the webapp wire-through ships.
func (cs *CommsServer) draftReply(req CommsRequest) CommsResponse {
	cs.logMessage("draft_denied_no_surface", "", "", "", req.Body, req.ReplyTo)
	return CommsResponse{Error: errSendApprovalUnavailable}
}

// httpRequest had a pty approval prompt before issuing the call.
// Same regression — fail-closed for now. Agents wanting to make
// HTTP calls should go through the connector + sandbox pipeline,
// which has its own approval surface (action manifests with
// `[approval] required = true`).
func (cs *CommsServer) httpRequest(req CommsRequest) CommsResponse {
	cs.logMessage("http_request_denied_no_surface", req.Service, req.Channel, "", req.Body, "")
	return CommsResponse{Error: errSendApprovalUnavailable}
}

// errSendApprovalUnavailable is the agent-facing message when a
// send-shaped CommsRequest hits the post-#419 launch path. Surfaces
// in `aileron-mcp`'s tool error so the LLM can route around it (e.g.
// suggest the user run the action via a connector instead).
const errSendApprovalUnavailable = "send / draft / http_request approval surface is currently unavailable in `aileron launch` (pty removed in #419; webapp wire-through tracked as a follow-up)"

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

// Touch the io import so it stays in the imports list. Used by the
// pre-#419 httpRequest path; preserved so the followup wire-through
// doesn't have to re-add the import.
var _ = io.Discard
