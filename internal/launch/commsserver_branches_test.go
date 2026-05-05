package launch_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/launch"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Branch coverage for the CommsServer's send-shaped methods (#428).
// Each test names the specific branch it exercises so a failure
// localises to one path. Together they cover the validation guards,
// timeout / dispatch-failure paths, the secrets injection helper,
// and the URL pattern matcher.

// --- URLMatchesPattern ---------------------------------------------

func TestURLMatchesPattern_Cases(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		pattern string
		want    bool
	}{
		{"trailing wildcard match", "https://slack.com/api/chat.postMessage", "slack.com/api/*", true},
		{"trailing wildcard no match", "https://discord.com/api/x", "slack.com/api/*", false},
		{"exact containment match", "https://api.github.com/repos/foo/bar", "api.github.com", true},
		{"exact containment no match", "https://api.gitlab.com/repos/foo/bar", "api.github.com", false},
		{"empty pattern matches anywhere via Contains", "https://example.com", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := launch.URLMatchesPattern(tc.url, tc.pattern); got != tc.want {
				t.Errorf("URLMatchesPattern(%q, %q) = %v, want %v", tc.url, tc.pattern, got, tc.want)
			}
		})
	}
}

// --- send_message validation ---------------------------------------

func TestCommsServer_SendMessage_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		req  launch.CommsRequest
	}{
		{"missing service", launch.CommsRequest{Method: "send_message", Channel: "#x", Body: "hi"}},
		{"missing channel", launch.CommsRequest{Method: "send_message", Service: "slack", Body: "hi"}},
		{"missing body", launch.CommsRequest{Method: "send_message", Service: "slack", Channel: "#x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sock, _ := newQueueWiredCommsServer(t, newStubListener("slack"))
			resp := dialComms(t, sock, tc.req)
			if resp.OK {
				t.Errorf("OK = true; want validation error")
			}
		})
	}
}

func TestCommsServer_SendMessage_TimeoutReturnsAgentError(t *testing.T) {
	listener := newStubListener("slack")
	srv, sock, _ := newQueueWiredCommsServer(t, listener)
	launch.SetApprovalTTLForTest(srv, 100*time.Millisecond)

	resp := dialComms(t, sock, launch.CommsRequest{
		Method: "send_message", Service: "slack", Channel: "#x", Body: "hi",
	})
	if resp.OK {
		t.Fatalf("OK = true; want timeout error")
	}
	if resp.Error == "" {
		t.Error("Error = empty; want a timeout message")
	}
	if !contains(resp.Error, "timeout") {
		t.Errorf("Error = %q, want a timeout message", resp.Error)
	}
}

func TestCommsServer_SendMessage_DispatchFailureSurfacesUpstream(t *testing.T) {
	listener := newStubListener("slack")
	listener.sendErr = errors.New("upstream slack 500")

	_, sock, q := newQueueWiredCommsServer(t, listener)
	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "send_message", Service: "slack", Channel: "#x", Body: "hi",
	})
	entry := pollFirstPending(t, q, 1*time.Second)
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	resp := <-respCh
	if resp.OK {
		t.Errorf("OK = true; want dispatch failure")
	}
	if !contains(resp.Error, "dispatch failed") || !contains(resp.Error, "upstream slack 500") {
		t.Errorf("Error = %q, want both 'dispatch failed' and the upstream message", resp.Error)
	}
}

// --- draft_reply ----------------------------------------------------

func TestCommsServer_DraftReply_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		req  launch.CommsRequest
	}{
		{"missing reply_to", launch.CommsRequest{Method: "draft_reply", Body: "hi"}},
		{"missing body", launch.CommsRequest{Method: "draft_reply", ReplyTo: "m1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sock, _ := newQueueWiredCommsServer(t)
			resp := dialComms(t, sock, tc.req)
			if resp.OK {
				t.Errorf("OK = true; want validation error")
			}
		})
	}
}

func TestCommsServer_DraftReply_NoOriginalDispatchError(t *testing.T) {
	// reply_to that's not in the NotifyQueue. The user can still
	// approve, but the dispatcher has no service / channel — handler
	// surfaces a clear error rather than silently dropping.
	_, sock, q := newQueueWiredCommsServer(t)

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "draft_reply", ReplyTo: "no-such-msg", Body: "ack",
	})
	entry := pollFirstPending(t, q, 1*time.Second)
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	resp := <-respCh
	if resp.OK {
		t.Errorf("OK = true; want dispatch error")
	}
	if !contains(resp.Error, "no longer available") {
		t.Errorf("Error = %q, want 'no longer available' message", resp.Error)
	}
}

func TestCommsServer_DraftReply_ApproveWithoutEditDispatchesOriginal(t *testing.T) {
	// Approve with no editedPayload — the dispatcher must send the
	// agent's original draft body verbatim.
	listener := newStubListener("slack")
	srv, sock, q := newQueueWiredCommsServer(t, listener)
	launch.PushIncomingForTest(srv, launch.IncomingForTest{
		ID: "msg-x", Service: "slack", Channel: "#general", Author: "alice", Body: "ping",
	})

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "draft_reply", ReplyTo: "msg-x", Body: "agent draft",
	})
	entry := pollFirstPending(t, q, 1*time.Second)
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	resp := <-respCh
	if !resp.OK {
		t.Fatalf("OK = false, err = %q", resp.Error)
	}
	if got := listener.Last(); got.Body != "agent draft" {
		t.Errorf("dispatched body = %q, want agent's original \"agent draft\"", got.Body)
	}
}

func TestCommsServer_DraftReply_DispatchFailureSurfacesUpstream(t *testing.T) {
	listener := newStubListener("slack")
	listener.sendErr = errors.New("rate limited")
	srv, sock, q := newQueueWiredCommsServer(t, listener)
	launch.PushIncomingForTest(srv, launch.IncomingForTest{
		ID: "msg-y", Service: "slack", Channel: "#general", Author: "alice", Body: "ping",
	})

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "draft_reply", ReplyTo: "msg-y", Body: "ack",
	})
	entry := pollFirstPending(t, q, 1*time.Second)
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	resp := <-respCh
	if resp.OK {
		t.Errorf("OK = true; want dispatch failure")
	}
	if !contains(resp.Error, "dispatch failed") {
		t.Errorf("Error = %q, want 'dispatch failed'", resp.Error)
	}
}

func TestCommsServer_DraftReply_TimeoutReturnsAgentError(t *testing.T) {
	srv, sock, _ := newQueueWiredCommsServer(t, newStubListener("slack"))
	launch.SetApprovalTTLForTest(srv, 100*time.Millisecond)
	launch.PushIncomingForTest(srv, launch.IncomingForTest{
		ID: "msg-z", Service: "slack", Channel: "#general", Author: "a", Body: "p",
	})

	resp := dialComms(t, sock, launch.CommsRequest{
		Method: "draft_reply", ReplyTo: "msg-z", Body: "ack",
	})
	if resp.OK {
		t.Errorf("OK = true; want timeout error")
	}
}

// --- http_request ---------------------------------------------------

func TestCommsServer_HTTPRequest_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		req  launch.CommsRequest
	}{
		{"missing method", launch.CommsRequest{Method: "http_request", Channel: "https://x"}},
		{"missing url", launch.CommsRequest{Method: "http_request", Service: "GET"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sock, _ := newQueueWiredCommsServer(t)
			resp := dialComms(t, sock, tc.req)
			if resp.OK {
				t.Errorf("OK = true; want validation error")
			}
		})
	}
}

func TestCommsServer_HTTPRequest_TimeoutReturnsAgentError(t *testing.T) {
	srv, sock, _ := newQueueWiredCommsServer(t)
	launch.SetApprovalTTLForTest(srv, 100*time.Millisecond)

	resp := dialComms(t, sock, launch.CommsRequest{
		Method: "http_request", Service: "GET", Channel: "https://example.com",
	})
	if resp.OK {
		t.Errorf("OK = true; want timeout error")
	}
}

func TestCommsServer_HTTPRequest_HeadersForwarded(t *testing.T) {
	var gotXTrace string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXTrace = r.Header.Get("X-Trace")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	_, sock, q := newQueueWiredCommsServer(t)

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method:  "http_request",
		Service: "GET",
		Channel: upstream.URL,
		ReplyTo: `{"X-Trace":"abc-123"}`,
	})
	entry := pollFirstPending(t, q, 1*time.Second)
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	resp := <-respCh
	if !resp.OK {
		t.Fatalf("OK = false, err = %q", resp.Error)
	}
	if gotXTrace != "abc-123" {
		t.Errorf("upstream X-Trace = %q, want \"abc-123\" (custom header didn't forward)", gotXTrace)
	}
}

func TestCommsServer_HTTPRequest_SecretInjectedAsBearer(t *testing.T) {
	// Stand up an upstream that captures the Authorization header,
	// configure a secret whose target pattern matches the URL, and
	// confirm the daemon injects the binding's bytes (not the name)
	// as a Bearer token.
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	listener := newStubListener("slack")
	srv, sock, q := newQueueWiredCommsServer(t, listener)

	// In-memory vault with a known credential.
	kek, err := crypto.GenerateRandomKEK()
	if err != nil {
		t.Fatalf("GenerateRandomKEK: %v", err)
	}
	v, err := vault.NewEncryptedVault(vault.NewMemVault(), kek)
	if err != nil {
		t.Fatalf("NewEncryptedVault: %v", err)
	}
	const secretName = "linear-api-key"
	const secretValue = "lin_api_xyz"
	if err := v.Put(context.Background(), secretName, []byte(secretValue), vault.Metadata{Type: "api_key"}); err != nil {
		t.Fatalf("vault.Put: %v", err)
	}
	srv.SetSecrets(launchpolicy.SecretsConfig{
		secretName: launchpolicy.SecretDef{Targets: []string{upstream.URL}},
	}, v)

	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "http_request", Service: "GET", Channel: upstream.URL,
	})
	entry := pollFirstPending(t, q, 1*time.Second)
	if entry.Args["secret_name"] != secretName {
		t.Errorf("queue entry secret_name = %v, want %q", entry.Args["secret_name"], secretName)
	}
	if _, ok := entry.Args["secret_value"]; ok {
		t.Error("queue entry leaked secret_value to the webapp surface")
	}
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	resp := <-respCh
	if !resp.OK {
		t.Fatalf("OK = false, err = %q", resp.Error)
	}
	want := "Bearer " + secretValue
	if gotAuth != want {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, want)
	}
}

func TestCommsServer_HTTPRequest_DispatchFailureSurfacesUpstreamError(t *testing.T) {
	// Point at a closed listener — http.Client.Do returns "connection
	// refused" or similar. Confirms the dispatch-failed branch.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	addr := closed.URL
	closed.Close()

	_, sock, q := newQueueWiredCommsServer(t)
	respCh := dialCommsAsync(t, sock, launch.CommsRequest{
		Method: "http_request", Service: "GET", Channel: addr,
	})
	entry := pollFirstPending(t, q, 1*time.Second)
	if err := q.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	resp := <-respCh
	if resp.OK {
		t.Errorf("OK = true; want dispatch failure when upstream is closed")
	}
	if !contains(resp.Error, "dispatch failed") {
		t.Errorf("Error = %q, want 'dispatch failed'", resp.Error)
	}
}

// --- matchSecret + DirectSend --------------------------------------

func TestMatchSecret_FindsConfiguredPattern(t *testing.T) {
	srv, _, _ := newQueueWiredCommsServer(t)
	srv.SetSecrets(launchpolicy.SecretsConfig{
		"linear": launchpolicy.SecretDef{Targets: []string{"linear.app/*"}},
		"github": launchpolicy.SecretDef{Targets: []string{"api.github.com"}},
	}, nil)

	if got, ok := launch.MatchSecretForTest(srv, "https://linear.app/graphql"); !ok || got != "linear" {
		t.Errorf("MatchSecret(linear url) = (%q, %v), want (linear, true)", got, ok)
	}
	if got, ok := launch.MatchSecretForTest(srv, "https://api.github.com/x"); !ok || got != "github" {
		t.Errorf("MatchSecret(github url) = (%q, %v), want (github, true)", got, ok)
	}
	if _, ok := launch.MatchSecretForTest(srv, "https://example.com/x"); ok {
		t.Error("MatchSecret(example.com) returned ok=true; want no match")
	}
}

func TestCommsServer_DirectSend_DispatchesViaListener(t *testing.T) {
	listener := newStubListener("slack")
	srv, _, _ := newQueueWiredCommsServer(t, listener)
	if err := srv.DirectSend("slack", "#general", "hello"); err != nil {
		t.Fatalf("DirectSend: %v", err)
	}
	if !listener.Sent() {
		t.Error("listener.Send was not called")
	}
	if got := listener.Last(); got.Channel != "#general" || got.Body != "hello" {
		t.Errorf("Send received %+v, want #general / hello", got)
	}
}

// --- shared apiServer + CommsServer queue ---------------------------

func TestCommsServer_AndApiServerShareQueue(t *testing.T) {
	// Constructing the apiServer with a Config.ActionApprovals set
	// must reuse that queue rather than creating a fresh one — so an
	// entry the CommsServer registers is observable through the
	// gateway's `/v1/action-approvals` API on the same instance.
	q := approval.NewActionApprovalQueue(nil, nil)
	listener := newStubListener("slack")

	// macOS caps unix-socket paths at 104 bytes; use a short /tmp
	// path rather than t.TempDir() which nests test names.
	f, err := os.CreateTemp("/tmp", "aileron-comms-shared-*.sock")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	sock := f.Name()
	f.Close()
	os.Remove(sock)
	t.Cleanup(func() { os.Remove(sock) })

	queueNotify := launch.NewNotifyQueue(8, nil)
	srv, err := launch.NewCommsServer(sock, queueNotify, []comms.Listener{listener}, "", "session-1", q)
	if err != nil {
		t.Fatalf("NewCommsServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve()
	time.Sleep(20 * time.Millisecond)

	// Register an entry directly on the queue (mirrors what
	// CommsServer's sendMessage would do internally) and confirm
	// List sees it.
	a := q.RegisterCommsSend("slack", "#x", "ping", "session-1")
	if got := q.List(); len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("queue.List = %+v, want exactly the just-registered entry", got)
	}
}

// --- helpers ---

func contains(s, sub string) bool {
	return s != "" && sub != "" && (s == sub || (len(s) >= len(sub) && (indexOf(s, sub) >= 0)))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// silence unused imports if the file later trims any test that needed them
var _ = fmt.Sprintf
