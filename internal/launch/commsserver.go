package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/comms"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// CommsRequest is sent by aileron-mcp to read or send messages.
type CommsRequest struct {
	Method  string `json:"method"` // "read_messages" or "send_message"
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
	DraftRequest bool   `json:"draft_request,omitempty"` // true if a reply draft is requested
}

// CommsServer handles IPC requests from aileron-mcp for reading and
// sending messages. It bridges MCP tools to the NotifyQueue and
// comms Listeners.
type CommsServer struct {
	socketPath string
	listener   net.Listener
	queue      *NotifyQueue
	senders    map[string]comms.Listener // keyed by service name
	bar        *StatusBar
	copier     *OutputCopier
	router     *KeyRouter
	auditLog   string
	sessionID  string
	secrets    launchpolicy.SecretsConfig
	vault      vault.Vault
	httpClient *http.Client

	mu   sync.Mutex
	done bool
}

// NewCommsServer creates a comms IPC server. The auditLog and sessionID
// parameters are used to log message events to the audit trail.
func NewCommsServer(socketPath string, queue *NotifyQueue, senders []comms.Listener, bar *StatusBar, copier *OutputCopier, router *KeyRouter, auditLog, sessionID string) (*CommsServer, error) {
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
		bar:        bar,
		copier:     copier,
		router:     router,
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

// draftReply stores a draft reply on a message without sending it.
// The user reviews the draft in the overlay and decides to send,
// edit, or discard. Aileron handles sending — not the agent.
func (cs *CommsServer) draftReply(req CommsRequest) CommsResponse {
	if req.ReplyTo == "" || req.Body == "" {
		return CommsResponse{Error: "reply_to and body are required"}
	}

	if !cs.queue.SetDraft(req.ReplyTo, req.Body) {
		return CommsResponse{Error: "message not found: " + req.ReplyTo}
	}

	cs.logMessage("draft_queued", "", "", "", req.Body, req.ReplyTo)
	return CommsResponse{OK: true}
}

func (cs *CommsServer) sendMessage(req CommsRequest) CommsResponse {
	if req.Service == "" || req.Channel == "" || req.Body == "" {
		return CommsResponse{Error: "service, channel, and body are required"}
	}

	sender, ok := cs.senders[req.Service]
	if !ok {
		return CommsResponse{Error: "no listener for service: " + req.Service}
	}

	// Check if this is a reply to an auto-drafted message. If so,
	// present the draft in the overlay for review instead of the
	// generic approval prompt.
	if orig, found := cs.queue.RecentByChannel(req.Service, req.Channel, 5*time.Minute); found {
		cs.queue.SetDraft(orig.ID, req.Body)
		cs.logMessage("draft_queued", req.Service, req.Channel, "", req.Body, orig.ID)

		// Wait for the user to approve or discard via the overlay.
		// The overlay sets the Draft field to "" on discard or sends
		// directly on approve. We poll for the draft to be resolved.
		decision := cs.waitForDraftDecision(orig.ID, req.Body)
		switch decision {
		case "approve":
			if err := sender.Send(nil, comms.OutgoingMessage{
				Channel: req.Channel,
				Body:    req.Body,
			}); err != nil {
				return CommsResponse{Error: "send failed: " + err.Error()}
			}
			cs.logMessage("message_sent", req.Service, req.Channel, "", req.Body, orig.ID)
			return CommsResponse{OK: true}
		case "edited":
			// The user edited and sent directly from the overlay.
			// The overlay already called DirectSend, so just return OK.
			cs.logMessage("draft_edited", req.Service, req.Channel, "", req.Body, orig.ID)
			return CommsResponse{OK: true}
		default:
			cs.logMessage("draft_discarded", req.Service, req.Channel, "", req.Body, orig.ID)
			return CommsResponse{Error: "draft discarded by user"}
		}
	}

	// Prompt the developer for approval before sending.
	decision := cs.promptSendApproval(req.Service, req.Channel, req.Body)
	if decision != "approve" {
		cs.logMessage("message_denied", req.Service, req.Channel, "", req.Body, "")
		return CommsResponse{Error: "message denied by user"}
	}

	if err := sender.Send(nil, comms.OutgoingMessage{
		Channel: req.Channel,
		Body:    req.Body,
	}); err != nil {
		return CommsResponse{Error: "send failed: " + err.Error()}
	}

	cs.logMessage("message_sent", req.Service, req.Channel, "", req.Body, "")
	return CommsResponse{OK: true}
}

// waitForDraftDecision polls the queue until the user resolves the draft
// in the overlay. Returns "approve", "edited", or "discard".
func (cs *CommsServer) waitForDraftDecision(msgID, originalDraft string) string {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(10 * time.Minute)

	for {
		select {
		case <-ticker.C:
			msg, found := cs.queue.FindByID(msgID)
			decision := ResolveDraft(msg, found, originalDraft)
			switch decision {
			case DraftApprove:
				cs.queue.ClearDraft(msgID)
				return "approve"
			case DraftEdited:
				cs.queue.ClearDraft(msgID)
				return "edited"
			case DraftDiscard:
				return "discard"
			default:
				// DraftPending — keep polling.
			}
		case <-timeout:
			cs.queue.ClearDraft(msgID)
			return "discard"
		}
	}
}

// draftApproved is a sentinel value set on the Draft field when the
// user approves the draft as-is from the overlay.
const draftApproved = "\x00approved"

// DraftDecision represents the outcome of draft resolution.
// Possible values: "pending", "approve", "edited", "discard".
type DraftDecision string

const (
	DraftPending  DraftDecision = "pending"
	DraftApprove  DraftDecision = "approve"
	DraftEdited   DraftDecision = "edited"
	DraftDiscard  DraftDecision = "discard"
)

// ResolveDraft examines a message's current draft state and returns the
// decision. If the message has not been found (pass found=false), the
// result is DraftDiscard. If the draft is still the original text, the
// result is DraftPending — the caller should keep polling.
func ResolveDraft(msg Message, found bool, originalDraft string) DraftDecision {
	if !found {
		return DraftDiscard
	}
	if msg.Draft == "" && msg.DraftFor == "" {
		return DraftDiscard
	}
	if msg.Draft == draftApproved {
		return DraftApprove
	}
	if msg.Draft != originalDraft && msg.Draft != "" && msg.Draft != draftApproved {
		return DraftEdited
	}
	return DraftPending
}

// promptSendApproval shows the developer a draft message for approval
// using the same alternate-screen pattern as the shell approval prompt.
func (cs *CommsServer) promptSendApproval(service, channel, body string) string {
	var inputCh <-chan byte
	if cs.router != nil {
		inputCh = cs.router.StealInput()
		defer cs.router.ReleaseInput()
	}

	if cs.copier != nil {
		cs.copier.SetPaused(true)
	}
	defer func() {
		if cs.copier != nil {
			cs.copier.SetPaused(false)
		}
	}()

	termRows, termCols := 24, 80
	if cs.bar != nil {
		termRows, termCols = cs.bar.Dims()
	}
	panel := NewPanel(PanelConfig{
		Title: "✈️ Aileron ─ Send Message",
	}, termRows, termCols)

	var content []string
	content = append(content, "")
	content = append(content, fmt.Sprintf("  To: \033[1m%s %s\033[0m", service, channel))
	content = append(content, "")
	for _, bl := range wrapText(body, panel.ContentWidth()-4) {
		content = append(content, "  "+bl)
	}
	content = append(content, "")
	content = append(content, "  \033[1m[y]\033[0m  Send")
	content = append(content, "  \033[1m[n]\033[0m  Discard")
	content = append(content, "")

	prompt := panel.RenderToAltScreen(content) + "\r\n  > "

	if cs.copier != nil {
		cs.copier.WriteExclusive([]byte(prompt))
	}

	if inputCh == nil {
		return "deny"
	}
	for {
		b := <-inputCh
		switch b {
		case 'y', 'Y':
			// Restore screen.
			if cs.copier != nil {
				cs.copier.WriteExclusive([]byte("\033[?1049l"))
			}
			return "approve"
		case 'n', 'N':
			if cs.copier != nil {
				cs.copier.WriteExclusive([]byte("\033[?1049l"))
			}
			return "deny"
		default:
			continue
		}
	}
}

// DirectSend sends a message without an approval prompt. Used for
// user-authored replies from the overlay (the user typed it, so no
// approval is needed).
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
// credential injection. Called after vault is opened at launch time.
func (cs *CommsServer) SetSecrets(secrets launchpolicy.SecretsConfig, v vault.Vault) {
	cs.secrets = secrets
	cs.vault = v
	cs.httpClient = &http.Client{}
}

func (cs *CommsServer) httpRequest(req CommsRequest) CommsResponse {
	// Fields are repurposed: Service=HTTP method, Channel=URL, Body=body, ReplyTo=headers JSON.
	method := req.Service
	url := req.Channel
	body := req.Body
	headersJSON := req.ReplyTo

	if method == "" || url == "" {
		return CommsResponse{Error: "method and url are required"}
	}

	// Match URL against secrets config to find credential.
	secretName, _ := cs.matchSecret(url)

	// Prompt user approval.
	detail := fmt.Sprintf("%s %s", method, url)
	if secretName != "" {
		detail += fmt.Sprintf(" (credential: %s)", secretName)
	}
	decision := cs.promptSendApproval("http", detail, body)
	if decision != "approve" {
		cs.logMessage("http_request_denied", method, url, "", body, "")
		return CommsResponse{Error: "request denied by user"}
	}

	// Build HTTP request.
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	httpReq, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return CommsResponse{Error: "invalid request: " + err.Error()}
	}

	// Parse and set headers.
	if headersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err == nil {
			for k, v := range headers {
				httpReq.Header.Set(k, v)
			}
		}
	}

	// Inject credential if matched.
	if secretName != "" && cs.vault != nil {
		secret, err := cs.vault.Get(nil, secretName)
		if err == nil {
			httpReq.Header.Set("Authorization", "Bearer "+string(secret.Value))
		}
	}

	// Execute request.
	client := cs.httpClient
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return CommsResponse{Error: "request failed: " + err.Error()}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	cs.logMessage("http_request_sent", method, url, "", string(respBody), "")

	return CommsResponse{
		OK: true,
		Messages: []CommsMessageDTO{{
			ID:   fmt.Sprintf("%d", resp.StatusCode),
			Body: string(respBody),
		}},
	}
}

// matchSecret finds the first secret whose target patterns match the URL.
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
// Patterns use simple prefix matching with optional trailing wildcard.
// Examples: "slack.com/api/*" matches "https://slack.com/api/chat.postMessage"
func URLMatchesPattern(url, pattern string) bool {
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.Contains(url, prefix)
	}
	return strings.Contains(url, pattern)
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

// SocketPath returns the socket path.
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
