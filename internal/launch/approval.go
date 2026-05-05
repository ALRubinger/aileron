package launch

import (
	"encoding/json"
	"net"
)

// ApprovalRequest is sent by aileron-sh when a command needs user
// approval. The wire shape is preserved across the pty-removal
// (issue #419) so aileron-sh keeps working unchanged. The receiving
// side — the in-pty ApprovalServer — is gone; under the new launch
// path, no listener accepts connections on the AILERON_APPROVAL_SOCKET,
// so [RequestApproval] dials, fails, and returns "deny" for v0.x.
//
// Routing shell-shim approvals through the webapp's action-approval
// queue is tracked separately (see #419's followup notes); when that
// lands, the launch process will register a webapp-rendered
// approval card and reply on this socket with the user's decision.
type ApprovalRequest struct {
	Command string `json:"command"`
	Reason  string `json:"reason"`
}

// ApprovalResponse is the user's decision, sent back to aileron-sh.
// The pre-pty-removal flow returned one of "allow_once", "deny",
// "allow_project", "allow_user". The new flow only ever returns
// "deny" (because no server is listening) until the webapp wire-
// through ships; aileron-sh treats unknown values as deny so this
// is forward-compatible.
type ApprovalResponse struct {
	Decision string `json:"decision"`
}

// RequestApproval connects to the approval socket and requests user
// approval for a command. Called from aileron-sh.
//
// Under the new launch (no pty wrap), no ApprovalServer is started,
// so the dial fails and we return "deny" — the policy-enforced
// shell behaves as if the user denied. Claude Code's own shell-tool
// approval still runs, so the agent doesn't bypass user consent;
// aileron-sh's role here is policy enforcement + audit logging,
// not the second-prompt UX it used to render in the pty.
func RequestApproval(socketPath, command, reason string) string {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return "deny"
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(ApprovalRequest{
		Command: command,
		Reason:  reason,
	}); err != nil {
		return "deny"
	}

	var resp ApprovalResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return "deny"
	}
	return resp.Decision
}
