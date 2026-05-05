package launch_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/launch"
)

// CommsServer + ActionApprovalQueue contract (#428):
//
//   - send_message → register comms_send → on Approve, dispatch via the
//     matched [comms.Listener] and return OK.
//   - send_message → on Deny, return an agent-facing error; no dispatch.
//   - draft_reply → register comms_draft with the original message
//     context → on Approve (with optional edited body), dispatch the
//     edited bytes; on Discard, no dispatch.
//   - http_request → register http_request with the matched secret
//     name → on Approve, issue the HTTP call.
//   - The queue entry's Args carry exactly the kind-specific shape
//     the webapp relies on (service/channel/body for comms_send;
//     original_author/original_body/draft_body for comms_draft;
//     method/url/secret_name for http_request).

// stubListener is a minimal [comms.Listener] that records the last
// outgoing message. Tests use it to assert "the dispatcher actually
// called Send with these bytes after the user approved."
type stubListener struct {
	service string
	mu      sync.Mutex
	last    comms.OutgoingMessage
	sendErr error
	sent    bool
}

func newStubListener(service string) *stubListener {
	return &stubListener{service: service}
}

func (s *stubListener) Connect(context.Context) error { return nil }
func (s *stubListener) Listen(context.Context) (<-chan comms.IncomingMessage, error) {
	return nil, nil
}
func (s *stubListener) Send(_ context.Context, msg comms.OutgoingMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.last = msg
	s.sent = true
	return nil
}
func (s *stubListener) Close() error    { return nil }
func (s *stubListener) Service() string { return s.service }
func (s *stubListener) Last() comms.OutgoingMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}
func (s *stubListener) Sent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// newQueueWiredCommsServer stands up a CommsServer with a real
// approval queue. The returned q is the same instance the server
// holds; tests Decide on it to drive the approval/deny path.
func newQueueWiredCommsServer(t *testing.T, listeners ...comms.Listener) (*launch.CommsServer, string, *approval.ActionApprovalQueue) {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "aileron-comms-queue-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	sock := f.Name()
	f.Close()
	os.Remove(sock)
	t.Cleanup(func() { os.Remove(sock) })

	q := approval.NewActionApprovalQueue(nil, nil)
	queue := launch.NewNotifyQueue(100, nil)
	srv, err := launch.NewCommsServer(sock, queue, listeners, "", "test-session", q)
	if err != nil {
		t.Fatalf("NewCommsServer: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	time.Sleep(20 * time.Millisecond)
	return srv, sock, q
}

func TestCommsServer_SendMessage_ApproveDispatches(t *testing.T) {
	listener := newStubListener("slack")
	_, sock, q := newQueueWiredCommsServer(t, listener)

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "send_message", Service: "slack", Channel: "#general", Body: "hello",
	})

	// Wait for the entry to land in the queue, then approve.
	entry := pollFirstPending(t, q, 2*time.Second)
	if entry.Kind != approval.ApprovalKindCommsSend {
		t.Errorf("Kind = %q, want %q", entry.Kind, approval.ApprovalKindCommsSend)
	}
	if entry.Args["service"] != "slack" || entry.Args["channel"] != "#general" || entry.Args["body"] != "hello" {
		t.Errorf("Args = %+v, want service=slack channel=#general body=hello", entry.Args)
	}
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	resp := <-respCh
	if !resp.OK {
		t.Fatalf("OK = false, err = %q", resp.Error)
	}
	if !listener.Sent() {
		t.Fatal("listener.Send was never called after Approve")
	}
	if got := listener.Last(); got.Channel != "#general" || got.Body != "hello" {
		t.Errorf("Send received %+v, want #general / hello", got)
	}
}

func TestCommsServer_SendMessage_DenyReturnsError(t *testing.T) {
	listener := newStubListener("slack")
	_, sock, q := newQueueWiredCommsServer(t, listener)

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "send_message", Service: "slack", Channel: "#general", Body: "hello",
	})

	entry := pollFirstPending(t, q, 2*time.Second)
	if err := q.Decide(entry.ID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	resp := <-respCh
	if resp.OK {
		t.Errorf("OK = true; want error after Deny")
	}
	if !strings.Contains(resp.Error, "user denied") || !strings.Contains(resp.Error, "wrong recipient") {
		t.Errorf("Error = %q, want a deny+reason message", resp.Error)
	}
	if listener.Sent() {
		t.Error("listener.Send was called despite the Deny")
	}
}

func TestCommsServer_SendMessage_NoMatchingService(t *testing.T) {
	// No listener registered for "slack" — handler must reject before
	// the queue entry is registered, otherwise the user sees a card
	// for a request that can never be fulfilled.
	_, sock, q := newQueueWiredCommsServer(t)

	resp := dialComms(t, sock, launch.CommsRequest{
		Method: "send_message", Service: "slack", Channel: "#general", Body: "hi",
	})
	if resp.OK {
		t.Errorf("OK = true; want error when no listener for service")
	}
	if !strings.Contains(resp.Error, "no listener") {
		t.Errorf("Error = %q, want \"no listener for service\"", resp.Error)
	}
	if pending := q.List(); len(pending) != 0 {
		t.Errorf("queue.List len = %d, want 0 (no entry should land for an undispatchable request)", len(pending))
	}
}

func TestCommsServer_DraftReply_ApproveWithEditDispatchesEdited(t *testing.T) {
	listener := newStubListener("slack")
	srv, sock, q := newQueueWiredCommsServer(t, listener)

	// Push an "incoming" message into the NotifyQueue so draftReply
	// has an original to look up. The CommsServer's queue is
	// inaccessible from outside the package; use the public
	// PushNotifyMessage helper... wait, there isn't one. Instead, use
	// the internal queue field via the [launch] package's
	// constructor: the test helper above keeps a handle but we don't
	// expose it. Workaround: send via DirectSend's path —
	// no, that's send-only. Cleanest path: piggyback on the server's
	// Notify queue handle via the PushIncoming helper exposed for
	// tests in #428.
	launch.PushIncomingForTest(srv, launch.IncomingForTest{
		ID:      "msg-orig-1",
		Service: "slack",
		Channel: "#general",
		Author:  "alice",
		Body:    "ping",
	})

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method:  "draft_reply",
		ReplyTo: "msg-orig-1",
		Body:    "ack",
	})

	entry := pollFirstPending(t, q, 2*time.Second)
	if entry.Kind != approval.ApprovalKindCommsDraft {
		t.Errorf("Kind = %q, want %q", entry.Kind, approval.ApprovalKindCommsDraft)
	}
	if entry.Args["original_body"] != "ping" || entry.Args["original_author"] != "alice" {
		t.Errorf("Args = %+v, want original_body=ping original_author=alice", entry.Args)
	}
	if entry.Args["draft_body"] != "ack" {
		t.Errorf("draft_body = %v, want \"ack\"", entry.Args["draft_body"])
	}

	if err := q.Decide(entry.ID, true, "", map[string]any{"body": "ack — got it"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	resp := <-respCh
	if !resp.OK {
		t.Fatalf("OK = false, err = %q", resp.Error)
	}
	got := listener.Last()
	if got.Body != "ack — got it" {
		t.Errorf("dispatched body = %q, want the edited \"ack — got it\"", got.Body)
	}
	if got.Channel != "#general" {
		t.Errorf("dispatched channel = %q, want \"#general\" (taken from the original message)", got.Channel)
	}
}

func TestCommsServer_DraftReply_DiscardReturnsError(t *testing.T) {
	listener := newStubListener("slack")
	srv, sock, q := newQueueWiredCommsServer(t, listener)
	launch.PushIncomingForTest(srv, launch.IncomingForTest{
		ID: "msg-orig-2", Service: "slack", Channel: "#general", Author: "bob", Body: "yo",
	})

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "draft_reply", ReplyTo: "msg-orig-2", Body: "hi",
	})

	entry := pollFirstPending(t, q, 2*time.Second)
	if err := q.Decide(entry.ID, false, "off-topic", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	resp := <-respCh
	if resp.OK {
		t.Errorf("OK = true; want error after discard")
	}
	if listener.Sent() {
		t.Error("listener.Send was called despite the discard")
	}
}

func TestCommsServer_HTTPRequest_ApproveDispatches(t *testing.T) {
	// Spin up a tiny upstream HTTP server. The CommsServer dispatches
	// the user-approved request to it; assert the body / method are
	// forwarded verbatim.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"got":"` + r.Method + `:` + string(body) + `"}`))
	}))
	t.Cleanup(upstream.Close)

	_, sock, q := newQueueWiredCommsServer(t)

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method:  "http_request",
		Service: "POST",
		Channel: upstream.URL,
		Body:    `{"hello":"world"}`,
	})

	entry := pollFirstPending(t, q, 2*time.Second)
	if entry.Kind != approval.ApprovalKindHTTPRequest {
		t.Errorf("Kind = %q, want %q", entry.Kind, approval.ApprovalKindHTTPRequest)
	}
	if entry.Args["method"] != "POST" || entry.Args["url"] != upstream.URL {
		t.Errorf("Args = %+v, want method=POST url=%s", entry.Args, upstream.URL)
	}
	if entry.Args["secret_name"] != "" {
		t.Errorf("secret_name = %v, want empty (no secrets configured in this test)", entry.Args["secret_name"])
	}
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	resp := <-respCh
	if !resp.OK {
		t.Fatalf("OK = false, err = %q", resp.Error)
	}
	if len(resp.Messages) == 0 {
		t.Fatal("Messages is empty; want one entry with the upstream response")
	}
	if !strings.Contains(resp.Messages[0].Body, `"got":"POST:{"hello":"world"}"`) {
		t.Errorf("upstream response body = %q, want the POST-passthrough payload", resp.Messages[0].Body)
	}
}

func TestCommsServer_HTTPRequest_DenyReturnsError(t *testing.T) {
	_, sock, q := newQueueWiredCommsServer(t)

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method:  "http_request",
		Service: "GET",
		Channel: "https://example.com/should-not-be-hit",
	})

	entry := pollFirstPending(t, q, 2*time.Second)
	if err := q.Decide(entry.ID, false, "no thanks", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	resp := <-respCh
	if resp.OK {
		t.Errorf("OK = true; want error on Deny")
	}
	if !strings.Contains(resp.Error, "user denied") {
		t.Errorf("Error = %q, want a deny message", resp.Error)
	}
}

// --- helpers ---

// dialCommsAsync dispatches a request on a goroutine so the test can
// drive the queue's Decide concurrently. The channel buffers one
// response; the caller reads it after Deciding.
func dialCommsAsync(t *testing.T, sock string, req launch.CommsRequest) chan launch.CommsResponse {
	t.Helper()
	ch := make(chan launch.CommsResponse, 1)
	go func() {
		ch <- dialComms(t, sock, req)
	}()
	return ch
}

// pollFirstPending waits up to timeout for the queue to have at
// least one pending entry, then returns the first one. Avoids
// race-on-register flakes between dialing and Deciding.
func pollFirstPending(t *testing.T, q *approval.ActionApprovalQueue, timeout time.Duration) *approval.ActionApproval {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pending := q.List(); len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queue had no pending entries after %s", timeout)
	return nil
}

// --- silence unused-import warnings on the json import in case the
// file later trims the asserts that need it. ---

var _ = json.Marshal
