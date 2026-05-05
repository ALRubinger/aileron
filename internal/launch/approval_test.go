package launch_test

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
)

// TestRequestApproval_NoSocket asserts the fail-soft contract on the
// shell-shim approval client: when the AILERON_APPROVAL_SOCKET points
// at a non-existent path (the post-#419 launch path no longer starts
// an in-pty ApprovalServer, so nothing listens), the dial fails and
// we return "deny". aileron-sh's policy engine treats "deny" as
// "block this command", which is the safe default until the
// shell-shim → webapp wire-through lands as a follow-up to #419.
func TestRequestApproval_NoSocket(t *testing.T) {
	decision := launch.RequestApproval("/nonexistent/socket.sock", "echo test", "")
	if decision != "deny" {
		t.Errorf("decision = %q, want deny (fail-soft on dial error)", decision)
	}
}
