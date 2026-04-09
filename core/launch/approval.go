package launch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
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
	tty        *os.File
	bar        *StatusBar
	copier     *OutputCopier

	mu   sync.Mutex
	done bool
}

// NewApprovalServer creates a server that listens for approval requests.
// The tty should be the real terminal (os.Stdin or opened /dev/tty from
// the launcher's context).
func NewApprovalServer(socketPath string, bar *StatusBar, copier *OutputCopier) (*ApprovalServer, error) {
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
	// Pause pty output so the prompt renders cleanly.
	if s.copier != nil {
		s.copier.SetPaused(true)
	}
	defer func() {
		if s.copier != nil {
			s.copier.SetPaused(false)
		}
	}()

	// Write prompt to os.Stdout (same fd the copier uses) so writes
	// are ordered. Only open /dev/tty for reading keyboard input.
	w := os.Stdout

	// Clear the screen for a clean prompt.
	fmt.Fprint(w, "\033[2J\033[1;1H")

	fmt.Fprintf(w, "\033[33m  ⏸ aileron: agent wants to run\033[0m\n\n")
	fmt.Fprintf(w, "    %s\n", command)
	if reason != "" {
		fmt.Fprintf(w, "\n    \033[2m%s\033[0m\n", reason)
	}
	fmt.Fprintf(w, "\n    \033[1m[y]\033[0m allow once  \033[1m[n]\033[0m deny  \033[1m[p]\033[0m always (project)  \033[1m[u]\033[0m always (user)  ")

	// Read input from /dev/tty (the real terminal's keyboard).
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return "deny"
	}
	defer tty.Close()

	response := readSingleKey(tty)

	// Clear screen before resuming so the agent gets a clean redraw.
	fmt.Fprint(w, "\033[2J\033[1;1H")

	switch response {
	case 'y', 'Y':
		return "allow_once"
	case 'p', 'P':
		return "allow_project"
	case 'u', 'U':
		return "allow_user"
	default:
		return "deny"
	}
}

// readSingleKey reads a single keypress from the terminal in raw mode.
func readSingleKey(tty *os.File) byte {
	// Put the tty fd into raw mode for single-keypress reading.
	buf := make([]byte, 1)
	// Use a buffered reader in case raw mode isn't set on this fd.
	reader := bufio.NewReader(tty)
	b, err := reader.ReadByte()
	if err != nil {
		return 'n'
	}
	_ = buf
	return b
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
