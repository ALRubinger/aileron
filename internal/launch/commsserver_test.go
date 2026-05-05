package launch_test

import (
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/launch"
)

// TestCommsServer_ReadMessages_Empty asserts the queue-read happy
// path: a fresh server with an empty NotifyQueue returns OK + no
// messages. This is the load-bearing path for `aileron-mcp`'s
// `read_messages` tool — runs every time the agent polls for new
// Slack/Discord traffic.
func TestCommsServer_ReadMessages_Empty(t *testing.T) {
	srv, sock := newCommsServer(t)
	defer srv.Close()

	resp := dialComms(t, sock, launch.CommsRequest{Method: "read_messages"})
	if !resp.OK {
		t.Errorf("OK = false; err=%q", resp.Error)
	}
	if len(resp.Messages) != 0 {
		t.Errorf("Messages = %d, want 0", len(resp.Messages))
	}
}

// TestCommsServer_SendMessage_NoQueueFailsClosed asserts the legacy
// fallback: when a CommsServer is constructed without an
// [approval.ActionApprovalQueue], send-shaped requests fail-closed
// rather than block indefinitely. Production launch always wires
// the queue (#428); this branch protects callers that haven't
// updated yet (e.g. tests that don't care about the approval flow).
func TestCommsServer_SendMessage_NoQueueFailsClosed(t *testing.T) {
	srv, sock := newCommsServer(t)
	defer srv.Close()

	resp := dialComms(t, sock, launch.CommsRequest{
		Method: "send_message", Service: "slack", Channel: "#x", Body: "hi",
	})
	if resp.OK {
		t.Errorf("OK = true; want fail-closed when no queue wired")
	}
	if resp.Error == "" {
		t.Error("Error is empty; want a clear fallback message")
	}
}

// TestCommsServer_DraftReply_NoQueueFailsClosed: same fallback as
// sendMessage. Kept separate so a future change to either method's
// fallback shape doesn't have to retire two tests at once.
func TestCommsServer_DraftReply_NoQueueFailsClosed(t *testing.T) {
	srv, sock := newCommsServer(t)
	defer srv.Close()

	resp := dialComms(t, sock, launch.CommsRequest{
		Method: "draft_reply", ReplyTo: "m1", Body: "ok",
	})
	if resp.OK {
		t.Errorf("OK = true; want fail-closed when no queue wired")
	}
}

// TestCommsServer_HTTPRequest_NoQueueFailsClosed: the generic
// `http_request` MCP tool (api_key-binding-injecting fetch) had its
// own pty approval prompt pre-#419 and now routes through the same
// queue (#428). Same fallback as the other two when no queue is
// supplied.
func TestCommsServer_HTTPRequest_NoQueueFailsClosed(t *testing.T) {
	srv, sock := newCommsServer(t)
	defer srv.Close()

	resp := dialComms(t, sock, launch.CommsRequest{
		Method: "http_request", Service: "GET", Channel: "https://example.com",
	})
	if resp.OK {
		t.Errorf("OK = true; want fail-closed when no queue wired")
	}
}

// TestCommsServer_UnknownMethod asserts the dispatch guard. Catches
// a typo in aileron-mcp before it produces an opaque "send failed"
// at runtime.
func TestCommsServer_UnknownMethod(t *testing.T) {
	srv, sock := newCommsServer(t)
	defer srv.Close()

	resp := dialComms(t, sock, launch.CommsRequest{Method: "blah"})
	if resp.Error == "" {
		t.Error("Error is empty; want unknown-method message")
	}
}

// TestRequestComms_NoSocket covers the client-side fail-soft for
// callers (aileron-mcp) that dial a non-existent socket. Returns a
// CommsResponse with an Error rather than panicking.
func TestRequestComms_NoSocket(t *testing.T) {
	resp := launch.RequestComms("/nonexistent/socket.sock", launch.CommsRequest{Method: "read_messages"})
	if resp.OK {
		t.Errorf("OK = true; want fail-soft on dial error")
	}
	if resp.Error == "" {
		t.Error("Error is empty; want connection-failure message")
	}
}

// TestCommsServer_DirectSend_NoSender asserts the direct-send error
// path when no listener is registered for the requested service.
// DirectSend is preserved across #419 for future webapp-driven
// reply paths; this guards against accidental nil-deref refactors.
func TestCommsServer_DirectSend_NoSender(t *testing.T) {
	srv, _ := newCommsServer(t)
	defer srv.Close()

	err := srv.DirectSend("nonexistent-service", "#x", "hi")
	if err == nil {
		t.Error("err = nil; want \"no listener for service\"")
	}
}

// TestCommsServer_SocketPath asserts the trivial accessor still works
// — useful for callers that need to delete the socket file out-of-band.
func TestCommsServer_SocketPath(t *testing.T) {
	srv, sock := newCommsServer(t)
	defer srv.Close()
	if got := srv.SocketPath(); got != sock {
		t.Errorf("SocketPath = %q, want %q", got, sock)
	}
}

// --- helpers ---

// newCommsServer stands up a CommsServer on a fresh socket. Caller
// must close. No bar/copier/router params — those died with #419's
// pty removal.
//
// macOS caps unix-socket paths at 104 bytes; t.TempDir() nests the
// test name and exceeds that on long descriptive test identifiers,
// so we use os.CreateTemp under /tmp explicitly to keep the socket
// path short and stable.
func newCommsServer(t *testing.T) (*launch.CommsServer, string) {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "aileron-comms-test-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	sock := f.Name()
	f.Close()
	os.Remove(sock) // listener will recreate it
	t.Cleanup(func() { os.Remove(sock) })

	q := launch.NewNotifyQueue(100, nil)
	srv, err := launch.NewCommsServer(sock, q, []comms.Listener{}, "", "test-session", nil)
	if err != nil {
		t.Fatalf("NewCommsServer: %v", err)
	}
	go srv.Serve()
	// Tiny pause so the listener is ready before Dial.
	time.Sleep(20 * time.Millisecond)
	return srv, sock
}

// dialComms connects to the comms socket, sends one CommsRequest,
// returns the CommsResponse. Test-side mirror of [launch.RequestComms]
// without the fail-soft wrapping — we want test failures, not nils.
func dialComms(t *testing.T, sock string, req launch.CommsRequest) launch.CommsResponse {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp launch.CommsResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}
