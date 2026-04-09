package launch

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
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
	ID        string `json:"id"`
	Service   string `json:"service"`
	Channel   string `json:"channel"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp"`
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

	mu   sync.Mutex
	done bool
}

// NewCommsServer creates a comms IPC server.
func NewCommsServer(socketPath string, queue *NotifyQueue, senders []comms.Listener, bar *StatusBar, copier *OutputCopier, router *KeyRouter) (*CommsServer, error) {
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
		dtos = append(dtos, CommsMessageDTO{
			ID:        m.ID,
			Service:   m.Source,
			Channel:   m.Channel,
			Author:    m.Author,
			Body:      m.Body,
			Timestamp: m.Timestamp.Format(time.RFC3339),
		})
	}
	cs.queue.MarkAllRead()
	return CommsResponse{OK: true, Messages: dtos}
}

func (cs *CommsServer) sendMessage(req CommsRequest) CommsResponse {
	if req.Service == "" || req.Channel == "" || req.Body == "" {
		return CommsResponse{Error: "service, channel, and body are required"}
	}

	sender, ok := cs.senders[req.Service]
	if !ok {
		return CommsResponse{Error: "no listener for service: " + req.Service}
	}

	// Prompt the developer for approval before sending.
	decision := cs.promptSendApproval(req.Service, req.Channel, req.Body)
	if decision != "approve" {
		return CommsResponse{Error: "message denied by user"}
	}

	if err := sender.Send(nil, comms.OutgoingMessage{
		Channel: req.Channel,
		Body:    req.Body,
	}); err != nil {
		return CommsResponse{Error: "send failed: " + err.Error()}
	}

	return CommsResponse{OK: true}
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

	w := 74
	termRows, termCols := 24, 80
	if cs.bar != nil {
		termRows, termCols = cs.bar.Dims()
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("\033[33m┌─ ✈️ Aileron ─ Send Message %s┐\033[0m", strings.Repeat("─", w-27)))
	lines = append(lines, fmt.Sprintf("\033[33m│\033[0m%s\033[33m│\033[0m", strings.Repeat(" ", w)))
	lines = append(lines, fmt.Sprintf("\033[33m│\033[0m  To: \033[1m%s %s\033[0m%s\033[33m│\033[0m",
		service, channel, strings.Repeat(" ", max(0, w-6-len(service)-1-len(channel)))))

	lines = append(lines, fmt.Sprintf("\033[33m│\033[0m%s\033[33m│\033[0m", strings.Repeat(" ", w)))

	// Wrap the body text into lines that fit the box.
	bodyLines := wrapText(body, w-4)
	for _, bl := range bodyLines {
		lines = append(lines, fmt.Sprintf("\033[33m│\033[0m  %s%s\033[33m│\033[0m",
			bl, strings.Repeat(" ", max(0, w-2-len(bl)))))
	}

	lines = append(lines, fmt.Sprintf("\033[33m│\033[0m%s\033[33m│\033[0m", strings.Repeat(" ", w)))
	lines = append(lines, fmt.Sprintf("\033[33m│\033[0m  \033[1m[y]\033[0m  Send%s\033[33m│\033[0m", strings.Repeat(" ", w-11)))
	lines = append(lines, fmt.Sprintf("\033[33m│\033[0m  \033[1m[n]\033[0m  Discard%s\033[33m│\033[0m", strings.Repeat(" ", w-14)))
	lines = append(lines, fmt.Sprintf("\033[33m│\033[0m%s\033[33m│\033[0m", strings.Repeat(" ", w)))
	lines = append(lines, fmt.Sprintf("\033[33m└%s┘\033[0m", strings.Repeat("─", w)))

	boxWidth := w + 2
	leftPad := max(0, (termCols-boxWidth)/2)
	topPad := max(0, (termRows-len(lines)-2)/2)
	margin := strings.Repeat(" ", leftPad)

	var prompt strings.Builder
	prompt.WriteString("\033[?1049h\033[2J\033[1;1H")
	for i := 0; i < topPad; i++ {
		prompt.WriteString("\r\n")
	}
	for _, line := range lines {
		fmt.Fprintf(&prompt, "%s%s\r\n", margin, line)
	}
	prompt.WriteString("\r\n")
	fmt.Fprintf(&prompt, "%s  > ", margin)

	if cs.copier != nil {
		cs.copier.WriteExclusive([]byte(prompt.String()))
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

// wrapText splits text into lines of at most maxWidth characters.
func wrapText(text string, maxWidth int) []string {
	if len(text) <= maxWidth {
		return []string{text}
	}
	var lines []string
	for len(text) > maxWidth {
		// Find a space to break at.
		idx := strings.LastIndex(text[:maxWidth], " ")
		if idx < 0 {
			idx = maxWidth
		}
		lines = append(lines, text[:idx])
		text = strings.TrimLeft(text[idx:], " ")
	}
	if len(text) > 0 {
		lines = append(lines, text)
	}
	return lines
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
