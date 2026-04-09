package launch

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	ptyPkg "github.com/creack/pty/v2"
)

// ApprovalRequest is sent by aileron-sh when a command needs user approval.
type ApprovalRequest struct {
	Command string `json:"command"`
	Reason  string `json:"reason"`
}

// ApprovalResponse is the user's decision, sent back to aileron-sh.
type ApprovalResponse struct {
	Decision string `json:"decision"` // "allow_once", "deny", "allow_project", "allow_user"
}

// ApprovalServer listens on a Unix socket for approval requests from
// aileron-sh and prompts the developer on the real terminal.
type ApprovalServer struct {
	socketPath string
	listener   net.Listener
	bar        *StatusBar
	copier     *OutputCopier
	router     *KeyRouter
	ptmx       *os.File // pty master — for triggering resize after prompt

	mu   sync.Mutex
	done bool
}

// NewApprovalServer creates a server that listens for approval requests.
func NewApprovalServer(socketPath string, bar *StatusBar, copier *OutputCopier, router *KeyRouter, ptmx *os.File) (*ApprovalServer, error) {
	// Remove stale socket file.
	os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("approval socket: %w", err)
	}

	return &ApprovalServer{
		socketPath: socketPath,
		listener:   ln,
		bar:        bar,
		copier:     copier,
		router:     router,
		ptmx:       ptmx,
	}, nil
}

// Serve accepts connections and handles approval requests. Blocks until
// Close is called. Run in a goroutine.
func (s *ApprovalServer) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			done := s.done
			s.mu.Unlock()
			if done {
				return
			}
			continue
		}
		s.handleConn(conn)
	}
}

func (s *ApprovalServer) handleConn(conn net.Conn) {
	defer conn.Close()

	var req ApprovalRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	decision := s.promptOnTerminal(req.Command, req.Reason)
	json.NewEncoder(conn).Encode(ApprovalResponse{Decision: decision})
}

// promptOnTerminal pauses pty output, switches to the alternate screen
// buffer, prompts the developer, and restores everything.
func (s *ApprovalServer) promptOnTerminal(command, reason string) string {
	// Steal keyboard input from the key router so keypresses come to us.
	var inputCh <-chan byte
	if s.router != nil {
		inputCh = s.router.StealInput()
		defer s.router.ReleaseInput()
	}

	// Pause pty output so the prompt renders cleanly.
	if s.copier != nil {
		s.copier.SetPaused(true)
	}
	defer func() {
		if s.copier != nil {
			s.copier.SetPaused(false)
		}
		// Trigger a pty resize so Claude Code redraws its full TUI.
		s.triggerRedraw()
	}()

	// Switch to alternate screen buffer — preserves scrollback history.
	w := 74 // inner width of the box
	pad := func(content string) string {
		// Pad content to fixed width for the right border.
		display := len(content) // approximate — good enough for ASCII
		if display >= w {
			return content
		}
		return content + strings.Repeat(" ", w-display)
	}

	var prompt strings.Builder
	prompt.WriteString("\033[?1049h\033[2J\033[1;1H")
	fmt.Fprintf(&prompt, "  \033[33m┌─ ✈️ Aileron %s┐\033[0m\r\n", strings.Repeat("─", w-12))
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m%s\033[33m│\033[0m\r\n", strings.Repeat(" ", w))
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m  ⚠️  The agent wants to run:%s\033[33m│\033[0m\r\n", strings.Repeat(" ", w-29))
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m  \033[1;36m%s\033[0m%s\033[33m│\033[0m\r\n", command, strings.Repeat(" ", w-2-len(command)))
	if reason != "" {
		fmt.Fprintf(&prompt, "  \033[33m│\033[0m  \033[2m%s\033[0m%s\033[33m│\033[0m\r\n", reason, strings.Repeat(" ", w-2-len(reason)))
	}
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m%s\033[33m│\033[0m\r\n", strings.Repeat(" ", w))
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m  %s\033[33m│\033[0m\r\n", pad("\033[1m[y]\033[0m  Yes, this time           Run this command; ask again next time"))
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m  %s\033[33m│\033[0m\r\n", pad("\033[1m[n]\033[0m  No, not this time        Block this command; ask again next time"))
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m  %s\033[33m│\033[0m\r\n", pad("\033[1m[a]\033[0m  Allow for this project   Add a rule to the project policy"))
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m  %s\033[33m│\033[0m\r\n", pad("\033[1m[m]\033[0m  Allow for me             Add a rule in my personal policy"))
	fmt.Fprintf(&prompt, "  \033[33m│\033[0m%s\033[33m│\033[0m\r\n", strings.Repeat(" ", w))
	fmt.Fprintf(&prompt, "  \033[33m└%s┘\033[0m\r\n", strings.Repeat("─", w))
	prompt.WriteString("  > ")

	if s.copier != nil {
		s.copier.WriteExclusive([]byte(prompt.String()))
	}

	// Read keypresses until we get a valid response.
	if inputCh == nil {
		return "deny"
	}
	for {
		b := <-inputCh
		switch b {
		case 'y', 'Y':
			return "allow_once"
		case 'n', 'N':
			return "deny"
		case 'a', 'A':
			return "allow_project"
		case 'm', 'M':
			return "allow_user"
		default:
			continue
		}
	}
}

// triggerRedraw clears the screen and sends a SIGWINCH to the agent's
// pty so it redraws its entire TUI. The status bar re-renders via the
// idle timer.
func (s *ApprovalServer) triggerRedraw() {
	// Restore normal screen buffer — scrollback history is preserved.
	if s.copier != nil {
		s.copier.WriteExclusive([]byte("\033[?1049l"))
	}
	if s.ptmx != nil {
		// Re-set the pty to its current size — this triggers SIGWINCH
		// in the agent, causing it to redraw.
		if ws, err := ptyPkg.GetsizeFull(s.ptmx); err == nil {
			ptyPkg.Setsize(s.ptmx, ws)
		}
	}
}

// Close shuts down the approval server and removes the socket file.
func (s *ApprovalServer) Close() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	s.listener.Close()
	os.Remove(s.socketPath)
}

// SocketPath returns the path to the Unix socket.
func (s *ApprovalServer) SocketPath() string {
	return s.socketPath
}

// RequestApproval connects to the approval server and requests user
// approval for a command. Called from aileron-sh.
func RequestApproval(socketPath, command, reason string) string {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return "deny"
	}
	defer conn.Close()

	json.NewEncoder(conn).Encode(ApprovalRequest{
		Command: command,
		Reason:  reason,
	})

	var resp ApprovalResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return "deny"
	}
	return resp.Decision
}

// WriteDenyToTTY writes a deny message to the real terminal via the
// approval server's pause mechanism.
func WriteDenyToTTY(w io.Writer, command, reason string) {
	fmt.Fprintf(w, "\033[31m  ✗ aileron: denied\033[0m %s\n", command)
	if reason != "" {
		fmt.Fprintf(w, "    %s\n", reason)
	}
}

// WriteDenyByUserToTTY writes a user-denied message.
func WriteDenyByUserToTTY(w io.Writer, command string) {
	fmt.Fprintf(w, "\033[33m  ✗ aileron: denied by user\033[0m %s\n", command)
}
