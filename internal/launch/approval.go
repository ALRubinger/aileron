package launch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/approval"
)

// ApprovalRequest is sent by aileron-sh when a command needs user
// approval. The wire shape is preserved across the pty-removal
// (issue #419) and the webapp wire-through (issue #427) so
// aileron-sh keeps working unchanged.
type ApprovalRequest struct {
	Command string `json:"command"`
	Reason  string `json:"reason"`
}

// ApprovalResponse is the user's decision, sent back to aileron-sh.
// Decision is one of "allow_once", "deny", "allow_project", or
// "allow_user". aileron-sh treats unknown values as deny.
//
// The four-option vocabulary is the policy-enforced shell's UX
// promise to the user: "approve once, save for project, save for me,
// or deny." On allow_project / allow_user, aileron-sh writes the new
// allow rule to disk on its side via [launchpolicy.AppendAllowRule];
// the launcher only routes the decision verbatim.
type ApprovalResponse struct {
	Decision string `json:"decision"`
}

// Wire-level decision strings. Kept as exported constants so the
// socket server, the unit tests, and any future CLI consumer agree
// on the vocabulary without copy-pasting string literals.
const (
	DecisionAllowOnce    = "allow_once"
	DecisionDeny         = "deny"
	DecisionAllowProject = "allow_project"
	DecisionAllowUser    = "allow_user"
)

// SavePolicyKey is the [approval.ActionDecision.EditedPayload] key
// the webapp posts on a shell-kind approval to express "approve and
// save the rule." Values are `""` (or absent — approve once),
// `"project"`, or `"user"`. The launch-side socket reads this key
// and translates it into the wire string aileron-sh expects; any
// other key on the payload is ignored.
const SavePolicyKey = "save_policy"

// RequestApproval connects to the approval socket and requests user
// approval for a command. Called from aileron-sh.
//
// The launcher wires up an [ApprovalSocketServer] on the path passed
// via AILERON_APPROVAL_SOCKET; the server registers a shell-kind
// entry on the action-approval queue, the webapp prompts the user,
// and the decision flows back here. If no server is listening (e.g.
// running aileron-sh outside a launch session), the dial fails and we
// return "deny" — the shell behaves as if the user denied, so the
// agent never silently bypasses policy.
func RequestApproval(socketPath, command, reason string) string {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return DecisionDeny
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(ApprovalRequest{
		Command: command,
		Reason:  reason,
	}); err != nil {
		return DecisionDeny
	}

	var resp ApprovalResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return DecisionDeny
	}
	return resp.Decision
}

// ApprovalSocketServer accepts dials from aileron-sh on the launch
// session's approval socket and routes each request through the
// webapp's action-approval surface (issue #427).
//
// One connection per command:
//
//  1. aileron-sh dials, encodes [ApprovalRequest]{Command, Reason}.
//  2. The server registers a shell-kind entry on the supplied
//     [approval.ActionApprovalQueue] via [approval.ActionApprovalQueue.RegisterShell];
//     the webapp's SSE stream lights up and the user sees a four-
//     option card (Approve once / Deny / Approve + save for project
//     / Approve + save for user).
//  3. The server blocks on [approval.ActionApproval.Wait] until the
//     user decides via `POST /v1/action-approvals/{id}/decide` (or
//     the timeout fires — default 5 minutes).
//  4. The decision is encoded as [ApprovalResponse]{Decision} on the
//     same connection. aileron-sh writes the new allow rule itself
//     on allow_project / allow_user; the launcher only routes the
//     verdict.
//
// The server runs until [ApprovalSocketServer.Close] is called or
// its context cancels. Each in-flight handler is tracked so Close
// drains them rather than leaving a pending aileron-sh dial hanging
// during shutdown.
type ApprovalSocketServer struct {
	queue     *approval.ActionApprovalQueue
	listener  net.Listener
	sessionID string
	timeout   time.Duration
	cwd       string
	log       *slog.Logger

	// serveCtx / serveCancel form the server's lifecycle context.
	// Allocated in [NewApprovalSocketServer] before Serve and Close
	// can race so a `defer Close(); go Serve(ctx)` pattern doesn't
	// trip the race detector — Serve never writes serveCancel after
	// startup, so Close can read it safely from any goroutine.
	serveCtx    context.Context
	serveCancel context.CancelFunc

	wg sync.WaitGroup
}

// approvalSocketDefaultTimeout is the per-request user-decision
// budget. Mirrors `internal/app/handlers_action_approvals.go`'s
// 5-minute default — a value the user can plausibly meet without
// being interrupted, but short enough to prevent indefinite hangs
// during a session crash. The webapp doesn't need to know the
// timeout to function: the SSE stream simply drops the resolved
// event when timeout fires, and aileron-sh maps the deny verdict
// into a "user denied" message to the agent.
const approvalSocketDefaultTimeout = 5 * time.Minute

// NewApprovalSocketServer binds a Unix socket at socketPath and
// returns a server ready to be started via [ApprovalSocketServer.Serve].
// queue is the shared action-approval queue the gateway also serves;
// sessionID and cwd are stamped into each registered approval entry
// so the webapp can correlate. log may be nil — the package default
// is used when so.
//
// On failure the socket file may be left in place at the requested
// path; the caller should treat NewApprovalSocketServer as the
// transactional unit and not assume cleanup happened.
func NewApprovalSocketServer(socketPath string, queue *approval.ActionApprovalQueue, sessionID, cwd string, log *slog.Logger) (*ApprovalSocketServer, error) {
	if queue == nil {
		return nil, errors.New("approval socket: queue is required")
	}
	// Best-effort cleanup of a stale socket from a prior crashed
	// session. net.Listen on "unix" fails with EADDRINUSE if the path
	// exists, even when nothing is listening; removing first is the
	// standard pattern. Errors here are non-fatal — Listen will
	// surface a clearer error if the path is somehow protected.
	_ = os.Remove(socketPath)
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	return &ApprovalSocketServer{
		queue:       queue,
		listener:    l,
		sessionID:   sessionID,
		cwd:         cwd,
		timeout:     approvalSocketDefaultTimeout,
		log:         log,
		serveCtx:    serveCtx,
		serveCancel: serveCancel,
	}, nil
}

// Serve accepts connections and handles each in its own goroutine.
// Returns when the listener closes, [Close] is called, or ctx
// cancels. The caller's ctx is bridged to the server's lifecycle
// context via a watcher goroutine, so a SIGINT during Launch and an
// explicit Close both stop the accept loop.
func (s *ApprovalSocketServer) Serve(ctx context.Context) {
	// Bridge caller cancellation into the server's lifecycle context.
	// We can't replace serveCtx (Close depends on its cancel being
	// stable) so we just propagate Done events from one to the other.
	// The watcher exits on either side closing.
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				s.serveCancel()
			case <-s.serveCtx.Done():
			}
		}()
	}

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Accept errors after Close are expected (the listener
			// was unblocked deliberately). Anything before Close is
			// genuine — log so the operator can see it but don't
			// tear down the launcher: the agent is still running
			// and shell approvals just fall through to the dial-
			// failed deny path until Serve restarts (which today
			// means restarting launch).
			select {
			case <-s.serveCtx.Done():
				return
			default:
			}
			s.log.Debug("approval socket accept", "error", err)
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(s.serveCtx, conn)
		}()
	}
}

// Close stops accepting new connections and waits for in-flight
// handlers to drain. Pending Wait calls receive ctx.Done() and reply
// with "deny" so a session shutdown never leaves an aileron-sh dial
// hanging. Safe to call on a nil receiver and idempotent.
func (s *ApprovalSocketServer) Close() error {
	if s == nil {
		return nil
	}
	s.serveCancel()
	err := s.listener.Close()
	s.wg.Wait()
	return err
}

// handle services one aileron-sh connection: decode the request,
// register the approval, wait for the decision, encode the response.
// Errors at any stage become a deny verdict on the wire — the
// policy-enforced shell fails closed, never silently approves.
func (s *ApprovalSocketServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Bound the read so a slow/buggy client can't pin a goroutine
	// indefinitely. The request payload is two short JSON fields;
	// 5 seconds is generous and still well below the user-decision
	// timeout that bounds the actual approval wait.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var req ApprovalRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		s.log.Debug("approval socket decode", "error", err)
		_ = json.NewEncoder(conn).Encode(ApprovalResponse{Decision: DecisionDeny})
		return
	}
	// Clear the read deadline before the long Wait — the write
	// deadline is reset just before the encode below.
	_ = conn.SetDeadline(time.Time{})

	entry := s.queue.RegisterShell(req.Command, req.Reason, s.sessionID, s.cwd)
	decision, err := entry.Wait(ctx, s.timeout)

	resp := ApprovalResponse{Decision: decisionToWire(decision, err)}
	// Cap the response write so a misbehaving aileron-sh that read
	// nothing can't pin the handler. The wire payload is tiny;
	// 5 seconds is plenty.
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if encErr := json.NewEncoder(conn).Encode(resp); encErr != nil {
		s.log.Debug("approval socket encode", "error", encErr)
	}
}

// decisionToWire maps a queue verdict + Wait error into the four-
// option wire vocabulary aileron-sh expects. Any error path
// (timeout, context cancelled, missing decision) maps to "deny" —
// the shell fails closed when it cannot get an explicit approve.
//
// The "save and approve" verdicts ride through
// [approval.ActionDecision.EditedPayload] under [SavePolicyKey]; we
// pull the value here and translate. Unknown / malformed values
// degrade to "allow_once" rather than failing the whole approval —
// the user has already clicked Approve, and dropping back to
// approve-once is the least-surprising fallback.
func decisionToWire(d approval.ActionDecision, waitErr error) string {
	if waitErr != nil {
		return DecisionDeny
	}
	if !d.Approved {
		return DecisionDeny
	}
	raw, ok := d.EditedPayload[SavePolicyKey]
	if !ok {
		return DecisionAllowOnce
	}
	switch v, _ := raw.(string); v {
	case "project":
		return DecisionAllowProject
	case "user":
		return DecisionAllowUser
	default:
		return DecisionAllowOnce
	}
}
