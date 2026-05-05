package launch_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/launch"
)

// shortSocketPath returns a short-enough socket path for the unix
// listener — macOS caps at 104 bytes and `t.TempDir()` paths in this
// repo's test setup commonly exceed that. Mirrors the pattern in
// commsserver_test.go's newCommsServer.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "aileron-approval-test-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	sock := f.Name()
	f.Close()
	os.Remove(sock) // the listener will (re)create it
	t.Cleanup(func() { os.Remove(sock) })
	return sock
}

// TestRequestApproval_NoSocket asserts the fail-soft contract on the
// shell-shim approval client: when the AILERON_APPROVAL_SOCKET points
// at a non-existent path (e.g. running aileron-sh outside a launch
// session), the dial fails and we return "deny". aileron-sh's policy
// engine treats "deny" as "block this command", which is the safe
// default — agents never silently bypass policy when the launcher
// isn't present to render the prompt.
func TestRequestApproval_NoSocket(t *testing.T) {
	decision := launch.RequestApproval("/nonexistent/socket.sock", "echo test", "")
	if decision != launch.DecisionDeny {
		t.Errorf("decision = %q, want %q (fail-soft on dial error)", decision, launch.DecisionDeny)
	}
}

// TestApprovalSocketServer_AllowOnceRoundTrip is the canonical happy
// path: aileron-sh dials, the server registers a shell-kind entry on
// the queue, the webapp (simulated by a direct Decide call) approves
// without a save policy, the server replies "allow_once". This pins
// the contract the post-#427 launch path promises aileron-sh.
func TestApprovalSocketServer_AllowOnceRoundTrip(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	socketPath := shortSocketPath(t)
	srv, err := launch.NewApprovalSocketServer(socketPath, q, "sess-1", "/cwd", nil)
	if err != nil {
		t.Fatalf("NewApprovalSocketServer: %v", err)
	}
	defer srv.Close()
	go srv.Serve(context.Background())

	// Drive the approver side from a separate goroutine so the
	// RequestApproval dial and the queue's Decide can race the way
	// they do in production (the webapp clicks while aileron-sh is
	// blocked on the held-open dial).
	decisionDone := make(chan struct{})
	go func() {
		defer close(decisionDone)
		// Spin briefly until the entry shows up on the queue. The
		// dial side races us; busy-poll on List is the simplest way
		// to wait without leaking implementation details.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pending := q.List()
			if len(pending) == 1 {
				if err := q.Decide(pending[0].ID, true, "", nil); err != nil {
					t.Errorf("Decide: %v", err)
				}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("no pending approval registered after 2s")
	}()

	got := launch.RequestApproval(socketPath, "git status", "ask rule")
	<-decisionDone
	if got != launch.DecisionAllowOnce {
		t.Errorf("decision = %q, want %q", got, launch.DecisionAllowOnce)
	}
}

// TestApprovalSocketServer_SavePolicyMappingTable asserts the table
// of (Approved, edited_payload.save_policy) → wire decision string
// aileron-sh expects. The launch-side socket is the only place this
// translation happens; if it drifts, the shim writes the wrong rule
// (or no rule) on the user's "save" click.
func TestApprovalSocketServer_SavePolicyMappingTable(t *testing.T) {
	cases := []struct {
		name          string
		approved      bool
		editedPayload map[string]any
		want          string
	}{
		{"deny", false, nil, launch.DecisionDeny},
		{"deny ignores save policy", false, map[string]any{"save_policy": "project"}, launch.DecisionDeny},
		{"allow once (nil payload)", true, nil, launch.DecisionAllowOnce},
		{"allow once (empty save_policy)", true, map[string]any{"save_policy": ""}, launch.DecisionAllowOnce},
		{"allow once (no save_policy key)", true, map[string]any{"other": "thing"}, launch.DecisionAllowOnce},
		{"allow project", true, map[string]any{"save_policy": "project"}, launch.DecisionAllowProject},
		{"allow user", true, map[string]any{"save_policy": "user"}, launch.DecisionAllowUser},
		{"unknown save_policy degrades to allow_once", true, map[string]any{"save_policy": "garbage"}, launch.DecisionAllowOnce},
		{"non-string save_policy degrades to allow_once", true, map[string]any{"save_policy": 42}, launch.DecisionAllowOnce},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := approval.NewActionApprovalQueue(nil, nil)
			socketPath := shortSocketPath(t)
			srv, err := launch.NewApprovalSocketServer(socketPath, q, "sess-1", "", nil)
			if err != nil {
				t.Fatalf("NewApprovalSocketServer: %v", err)
			}
			defer srv.Close()
			go srv.Serve(context.Background())

			done := make(chan struct{})
			go func() {
				defer close(done)
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					pending := q.List()
					if len(pending) == 1 {
						_ = q.Decide(pending[0].ID, tc.approved, "", tc.editedPayload)
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()

			got := launch.RequestApproval(socketPath, "git push", "ask rule")
			<-done
			if got != tc.want {
				t.Errorf("decision = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApprovalSocketServer_RegisteredEntryShape asserts the
// registered approval carries Kind "shell" and the
// command/reason/cwd in Args — the shape the webapp's shell-card
// layout reads. If a refactor moves these into a dedicated struct
// field, the webapp's render breaks; this test pins the contract.
func TestApprovalSocketServer_RegisteredEntryShape(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	socketPath := shortSocketPath(t)
	srv, err := launch.NewApprovalSocketServer(socketPath, q, "sess-42", "/work/repo", nil)
	if err != nil {
		t.Fatalf("NewApprovalSocketServer: %v", err)
	}
	defer srv.Close()
	go srv.Serve(context.Background())

	checked := make(chan struct{})
	go func() {
		defer close(checked)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pending := q.List()
			if len(pending) == 1 {
				p := pending[0]
				if p.Kind != approval.ApprovalKindShell {
					t.Errorf("Kind = %q, want %q", p.Kind, approval.ApprovalKindShell)
				}
				if got, want := p.Args["command"], "git push --force"; got != want {
					t.Errorf("Args[command] = %v, want %q", got, want)
				}
				if got, want := p.Args["reason"], "matches ask rule"; got != want {
					t.Errorf("Args[reason] = %v, want %q", got, want)
				}
				if got, want := p.Args["cwd"], "/work/repo"; got != want {
					t.Errorf("Args[cwd] = %v, want %q", got, want)
				}
				if p.SessionID != "sess-42" {
					t.Errorf("SessionID = %q, want %q", p.SessionID, "sess-42")
				}
				_ = q.Decide(p.ID, true, "", nil)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("no pending approval registered")
	}()

	_ = launch.RequestApproval(socketPath, "git push --force", "matches ask rule")
	<-checked
}

// TestApprovalSocketServer_NilQueueRejected asserts the constructor
// fails fast on a nil queue rather than panicking later inside the
// accept loop. The launcher always supplies a queue, but the
// constructor's contract should still be defensive — the failure
// mode is silent acceptance of dials that go nowhere, which is
// harder to debug than a startup error.
func TestApprovalSocketServer_NilQueueRejected(t *testing.T) {
	socketPath := shortSocketPath(t)
	if _, err := launch.NewApprovalSocketServer(socketPath, nil, "", "", nil); err == nil {
		t.Errorf("expected error for nil queue, got nil")
	}
}

// TestApprovalSocketServer_StaleSocketRemoved asserts the
// constructor removes a leftover socket file from a prior crashed
// session and successfully binds. Without this, a non-clean shutdown
// would leave `aileron launch` unable to start until the user
// manually `rm`s the socket file.
func TestApprovalSocketServer_StaleSocketRemoved(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	socketPath := shortSocketPath(t)

	// Simulate a stale socket file from a prior crashed session.
	srv1, err := launch.NewApprovalSocketServer(socketPath, q, "", "", nil)
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	// Closing the listener leaves the socket file on disk — that's
	// the case we're trying to recover from.
	_ = srv1.Close()

	srv2, err := launch.NewApprovalSocketServer(socketPath, q, "", "", nil)
	if err != nil {
		t.Fatalf("second bind (stale socket present): %v", err)
	}
	_ = srv2.Close()
}
