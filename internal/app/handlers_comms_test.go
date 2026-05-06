package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/vault"
)

// /v1/sessions/{id}/comms/* contract:
//
//   - GET /comms/messages — 200 with the queue snapshot, marks messages
//     read on success. 503 when the daemon was not configured with a
//     notify queue.
//   - POST /comms/send / /comms/draft / /comms/http — 200 with
//     `{ok:true}` on approve, `{ok:false,error:...}` on deny / timeout
//     / dispatch failure. 400 on missing fields. 503 when no queue or
//     no approval queue is configured.

// newCommsServer builds an apiServer wired with a fresh notify queue,
// listener registry, and approval queue. ttl scopes the per-entry
// approval wait so timeout tests run quickly. Tests drive the queue
// directly via Decide() to simulate user verdicts; the listener
// registry stays empty unless the test populates it.
func newCommsServer(t *testing.T, ttl time.Duration) *apiServer {
	t.Helper()
	return &apiServer{
		log:               slog.Default(),
		notifyQueue:       comms.NewNotifyQueue(100, nil),
		listeners:         comms.NewListenerRegistry(),
		actionApprovals:   approval.NewActionApprovalQueue(nil, nil),
		actionApprovalTTL: ttl,
	}
}

// approveNextOf decides the first matching pending approval as soon as
// it appears. Mirrors the helper used by the shell-approval tests.
func approveNextOf(s *apiServer, kind approval.ApprovalKind, approved bool, edited map[string]any) {
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pending := s.actionApprovals.List()
			for _, p := range pending {
				if p.Kind == kind {
					_ = s.actionApprovals.Decide(p.ID, approved, "", edited)
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

// fakeListener captures Send calls so tests can assert on dispatch
// without round-tripping through the real Slack/Discord SDKs.
type fakeListener struct {
	service string
	sent    []comms.OutgoingMessage
	sendErr error
}

func (f *fakeListener) Service() string                            { return f.service }
func (f *fakeListener) Connect(context.Context) error              { return nil }
func (f *fakeListener) Listen(context.Context) (<-chan comms.IncomingMessage, error) {
	return make(chan comms.IncomingMessage), nil
}
func (f *fakeListener) Send(_ context.Context, msg comms.OutgoingMessage) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, msg)
	return nil
}
func (f *fakeListener) Close() error { return nil }

// --- ReadCommsMessages ---

func TestReadCommsMessages_HappyPath(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	s.notifyQueue.Push(comms.Message{ID: "1", Source: "slack", Channel: "#dev", Author: "alice", Body: "hello", Timestamp: time.Now()})
	s.notifyQueue.Push(comms.Message{ID: "2", Source: "discord", Channel: "general", Author: "bob", Body: "hi", Timestamp: time.Now()})

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/x/comms/messages", nil)
	w := httptest.NewRecorder()
	s.ReadCommsMessages(w, req, "x", api.ReadCommsMessagesParams{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp api.ReadCommsMessagesResponse
	mustDecode(t, w.Body, &resp)
	if len(resp.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(resp.Messages))
	}
	// All surfaced messages must be marked read after the call.
	if got := s.notifyQueue.UnreadCount(); got != 0 {
		t.Errorf("UnreadCount after read = %d, want 0", got)
	}
}

func TestReadCommsMessages_FilterByService(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	s.notifyQueue.Push(comms.Message{ID: "1", Source: "slack", Channel: "#dev"})
	s.notifyQueue.Push(comms.Message{ID: "2", Source: "discord", Channel: "general"})

	svc := "slack"
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/x/comms/messages", nil)
	w := httptest.NewRecorder()
	s.ReadCommsMessages(w, req, "x", api.ReadCommsMessagesParams{Service: &svc})
	var resp api.ReadCommsMessagesResponse
	mustDecode(t, w.Body, &resp)
	if len(resp.Messages) != 1 || resp.Messages[0].Service != "slack" {
		t.Errorf("filter by service failed: %+v", resp.Messages)
	}
}

func TestReadCommsMessages_DraftRequestFlag(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	s.notifyQueue.Push(comms.Message{ID: "1", Source: "slack", AutoDraft: true})
	s.notifyQueue.Push(comms.Message{ID: "2", Source: "slack", AutoDraft: true, Draft: "already drafted"})

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/x/comms/messages", nil)
	w := httptest.NewRecorder()
	s.ReadCommsMessages(w, req, "x", api.ReadCommsMessagesParams{})
	var resp api.ReadCommsMessagesResponse
	mustDecode(t, w.Body, &resp)
	if resp.Messages[0].DraftRequest == nil || !*resp.Messages[0].DraftRequest {
		t.Error("expected draft_request=true on auto-draft without existing draft")
	}
	if resp.Messages[1].DraftRequest != nil && *resp.Messages[1].DraftRequest {
		t.Error("expected draft_request=false on already-drafted message")
	}
}

func TestReadCommsMessages_NoQueue503(t *testing.T) {
	s := &apiServer{log: slog.Default()} // no notifyQueue
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/x/comms/messages", nil)
	w := httptest.NewRecorder()
	s.ReadCommsMessages(w, req, "x", api.ReadCommsMessagesParams{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// --- SendCommsMessage ---

func TestSendCommsMessage_ApprovedDispatches(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)

	approveNextOf(s, approval.ApprovalKindCommsSend, true, nil)

	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "ship it"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "sess-1")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if !resp.Ok {
		t.Fatalf("ok=false, error=%v", deref(resp.Error))
	}
	if len(listener.sent) != 1 || listener.sent[0].Body != "ship it" {
		t.Errorf("listener sent = %+v, want one message body=ship it", listener.sent)
	}
}

func TestSendCommsMessage_DeniedReturnsError(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	s.listeners.Set("slack", &fakeListener{service: "slack"})

	approveNextOf(s, approval.ApprovalKindCommsSend, false, nil)

	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "ship it"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "sess-1")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok {
		t.Fatal("expected ok=false on deny")
	}
}

func TestSendCommsMessage_TimeoutCollapsesToError(t *testing.T) {
	// 50ms TTL, no decision — entry times out → ok=false.
	s := newCommsServer(t, 50*time.Millisecond)
	s.listeners.Set("slack", &fakeListener{service: "slack"})

	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "ship it"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "sess-1")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok {
		t.Fatal("expected ok=false on timeout")
	}
	if !strings.Contains(deref(resp.Error), "timeout") {
		t.Errorf("error = %q, want a timeout reference", deref(resp.Error))
	}
}

func TestSendCommsMessage_MissingFields400(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	s.listeners.Set("slack", &fakeListener{service: "slack"})
	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack"}) // missing channel, body
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSendCommsMessage_NoListenerForService(t *testing.T) {
	// Listener registry empty → /comms/send returns ok=false rather
	// than registering an approval that could never be dispatched.
	s := newCommsServer(t, 5*time.Second)
	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")
	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok {
		t.Fatal("expected ok=false when no listener registered")
	}
	if !strings.Contains(deref(resp.Error), "no listener for service") {
		t.Errorf("error = %q, want 'no listener for service' detail", deref(resp.Error))
	}
}

func TestSendCommsMessage_GarbageBody400(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	s.listeners.Set("slack", &fakeListener{service: "slack"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSendCommsMessage_NoApprovalQueue503(t *testing.T) {
	// Queue + listeners wired but action-approval queue nil → 503.
	s := &apiServer{
		log:         slog.Default(),
		notifyQueue: comms.NewNotifyQueue(10, nil),
		listeners:   comms.NewListenerRegistry(),
	}
	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#x", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestSendCommsMessage_NoQueue503(t *testing.T) {
	// Comms queue + listener registry both nil → 503.
	s := &apiServer{log: slog.Default(), actionApprovals: approval.NewActionApprovalQueue(nil, nil)}
	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// --- DraftCommsReply ---

func TestDraftCommsReply_ApprovedDispatches(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev", Author: "alice", Body: "is the deploy blocked?"})

	approveNextOf(s, approval.ApprovalKindCommsDraft, true, nil)

	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "no, all clear"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "sess-1")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if !resp.Ok {
		t.Fatalf("ok=false, error=%v", deref(resp.Error))
	}
	if len(listener.sent) != 1 || listener.sent[0].Body != "no, all clear" {
		t.Errorf("listener sent = %+v", listener.sent)
	}
}

func TestDraftCommsReply_EditedBodyWins(t *testing.T) {
	// User edits the draft via EditedPayload["body"] — the dispatcher
	// must send the edited bytes, not the agent's original draft.
	s := newCommsServer(t, 5*time.Second)
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev"})

	approveNextOf(s, approval.ApprovalKindCommsDraft, true, map[string]any{"body": "edited reply"})

	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "agent's draft"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "sess-1")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if !resp.Ok {
		t.Fatalf("ok=false, error=%v", deref(resp.Error))
	}
	if len(listener.sent) != 1 || listener.sent[0].Body != "edited reply" {
		t.Errorf("listener sent = %+v, want edited body", listener.sent)
	}
}

func TestDraftCommsReply_MissingFields400(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "x"}) // missing body
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestDraftCommsReply_OriginalEvictedFromQueue(t *testing.T) {
	// User approves a draft whose original message is no longer in the
	// queue — the daemon can't route the reply, so ok=false with a
	// descriptive error.
	s := newCommsServer(t, 5*time.Second)
	approveNextOf(s, approval.ApprovalKindCommsDraft, true, nil)

	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "evicted", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok {
		t.Fatal("expected ok=false when original message is no longer available")
	}
}

// --- RequestCommsHTTP ---

func TestRequestCommsHTTP_ApprovedDispatches(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			w.Header().Set("X-Saw-Auth", got)
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer upstream.Close()

	s := newCommsServer(t, 5*time.Second)
	approveNextOf(s, approval.ApprovalKindHTTPRequest, true, nil)

	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET", Url: upstream.URL})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if !resp.Ok {
		t.Fatalf("ok=false, error=%v", deref(resp.Error))
	}
	if resp.Messages == nil || len(*resp.Messages) != 1 {
		t.Fatalf("expected 1 response message, got %+v", resp.Messages)
	}
	if !strings.Contains((*resp.Messages)[0].Body, "ok") {
		t.Errorf("response body = %q", (*resp.Messages)[0].Body)
	}
}

func TestRequestCommsHTTP_BearerInjectedFromVault(t *testing.T) {
	// vault entry: type=api_key, label url-pattern matches the upstream
	// URL → handler injects "Bearer <value>" on the upstream call.
	gotAuth := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	v := vault.NewMemVault()
	_ = v.Put(context.Background(), "api_key/example/work", []byte("super-secret"), vault.Metadata{
		Type:   "api_key",
		Labels: map[string]string{"url-pattern": "127.0.0.1"},
	})

	s := newCommsServer(t, 5*time.Second)
	s.vault = v
	approveNextOf(s, approval.ApprovalKindHTTPRequest, true, nil)

	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET", Url: upstream.URL})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")

	auth := <-gotAuth
	if auth != "Bearer super-secret" {
		t.Errorf("Authorization header = %q, want 'Bearer super-secret'", auth)
	}
}

func TestRequestCommsHTTP_DeniedReturnsError(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	approveNextOf(s, approval.ApprovalKindHTTPRequest, false, nil)
	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET", Url: "https://example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok {
		t.Fatal("expected ok=false on deny")
	}
}

func TestRequestCommsHTTP_GarbageBody400(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRequestCommsHTTP_MissingFields400(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET"}) // missing url
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRequestCommsHTTP_NoApprovalQueue503(t *testing.T) {
	// Even without listeners, /comms/http needs the action-approval
	// queue. nil queue → 503 (matching the shell-approval pattern).
	s := &apiServer{log: slog.Default()}
	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET", Url: "https://x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// --- url-pattern matching ---

func TestMatchAPIKeyForURL_MatchesByLabel(t *testing.T) {
	v := vault.NewMemVault()
	_ = v.Put(context.Background(), "api_key/foo/work", []byte("k"), vault.Metadata{
		Type:   "api_key",
		Labels: map[string]string{"url-pattern": "api.example.com"},
	})
	_ = v.Put(context.Background(), "api_key/bar/work", []byte("k"), vault.Metadata{
		Type:   "api_key",
		Labels: map[string]string{"url-pattern": "other.example.com"},
	})
	s := &apiServer{vault: v}
	name, ok := s.matchAPIKeyForURL(context.Background(), "https://api.example.com/v1/x")
	if !ok || name != "api_key/foo/work" {
		t.Errorf("got (%q, %v), want api_key/foo/work, true", name, ok)
	}
}

func TestMatchAPIKeyForURL_SkipsNonAPIKey(t *testing.T) {
	v := vault.NewMemVault()
	_ = v.Put(context.Background(), "oauth/foo", []byte("k"), vault.Metadata{
		Type:   "oauth_refresh_token",
		Labels: map[string]string{"url-pattern": "api.example.com"},
	})
	s := &apiServer{vault: v}
	if _, ok := s.matchAPIKeyForURL(context.Background(), "https://api.example.com/v1"); ok {
		t.Error("expected no match for non-api_key entry")
	}
}

// --- Additional dispatch error coverage ---

func TestSendCommsMessage_DispatchErrorReturnsError(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	s.listeners.Set("slack", &fakeListener{service: "slack", sendErr: errSendFailed{}})
	approveNextOf(s, approval.ApprovalKindCommsSend, true, nil)

	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#x", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok || !strings.Contains(deref(resp.Error), "dispatch failed") {
		t.Errorf("expected ok=false with 'dispatch failed', got %+v", resp)
	}
}

func TestDraftCommsReply_DispatchErrorReturnsError(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	s.listeners.Set("slack", &fakeListener{service: "slack", sendErr: errSendFailed{}})
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev"})
	approveNextOf(s, approval.ApprovalKindCommsDraft, true, nil)

	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok || !strings.Contains(deref(resp.Error), "dispatch failed") {
		t.Errorf("expected ok=false with 'dispatch failed', got %+v", resp)
	}
}

func TestDraftCommsReply_NoListenerForService(t *testing.T) {
	// Original message was on a service that's no longer registered —
	// e.g. the listener died during the user's deliberation.
	s := newCommsServer(t, 5*time.Second)
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev"})
	approveNextOf(s, approval.ApprovalKindCommsDraft, true, nil)

	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok || !strings.Contains(deref(resp.Error), "no listener for service") {
		t.Errorf("expected 'no listener for service' error, got %+v", resp)
	}
}

func TestDraftCommsReply_NoQueue503(t *testing.T) {
	// Comms surface unconfigured → 503, matching SendCommsMessage.
	s := &apiServer{log: slog.Default(), actionApprovals: approval.NewActionApprovalQueue(nil, nil)}
	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "x", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestDraftCommsReply_NoApprovalQueue503(t *testing.T) {
	// Listener registry + queue wired but action-approval queue nil →
	// 503 (separate failure mode from no comms at all).
	s := &apiServer{
		log:         slog.Default(),
		notifyQueue: comms.NewNotifyQueue(10, nil),
		listeners:   comms.NewListenerRegistry(),
	}
	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "x", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestDraftCommsReply_GarbageBody400(t *testing.T) {
	s := newCommsServer(t, 5*time.Second)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestDraftCommsReply_ContextCancelledCollapsesToError(t *testing.T) {
	// ctx.Done before the user decides → waitErr is ctx.Err(), the
	// non-timeout error branch.
	s := newCommsServer(t, 5*time.Second)
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the request runs
	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")

	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok {
		t.Fatal("expected ok=false on ctx cancellation")
	}
}

func TestDraftCommsReply_TimeoutCollapsesToError(t *testing.T) {
	s := newCommsServer(t, 50*time.Millisecond)
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev"})
	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")
	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok || !strings.Contains(deref(resp.Error), "timeout") {
		t.Errorf("expected timeout error, got %+v", resp)
	}
}

func TestRequestCommsHTTP_TimeoutCollapsesToError(t *testing.T) {
	s := newCommsServer(t, 50*time.Millisecond)
	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET", Url: "https://example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok || !strings.Contains(deref(resp.Error), "timeout") {
		t.Errorf("expected timeout error, got %+v", resp)
	}
}

func TestRequestCommsHTTP_DispatchFailure(t *testing.T) {
	// Approve, then point at an unreachable URL — daemon's HTTP client
	// returns an error which surfaces as ok=false.
	s := newCommsServer(t, 5*time.Second)
	approveNextOf(s, approval.ApprovalKindHTTPRequest, true, nil)

	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET", Url: "http://127.0.0.1:1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	var resp api.CommsToolResponse
	mustDecode(t, w.Body, &resp)
	if resp.Ok || !strings.Contains(deref(resp.Error), "dispatch failed") {
		t.Errorf("expected dispatch failed error, got %+v", resp)
	}
}

func TestRequestCommsHTTP_HeadersInjected(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("X-Custom")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	s := newCommsServer(t, 5*time.Second)
	approveNextOf(s, approval.ApprovalKindHTTPRequest, true, nil)

	body, _ := json.Marshal(api.RequestCommsHTTPRequest{
		Method:  "GET",
		Url:     upstream.URL,
		Headers: ptr(`{"X-Custom":"hello"}`),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")

	if v := <-got; v != "hello" {
		t.Errorf("X-Custom = %q, want hello", v)
	}
}

func TestSendCommsMessage_AuditWriteHappens(t *testing.T) {
	dir := t.TempDir()
	s := newCommsServer(t, 5*time.Second)
	s.auditStateDir = dir
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)
	approveNextOf(s, approval.ApprovalKindCommsSend, true, nil)

	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "ship it"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "sess-1")

	// audit/audit-YYYY-MM-DD.jsonl should now have a `message_sent` entry.
	entries := readAuditMessages(t, dir)
	if len(entries) == 0 {
		t.Fatal("no audit entries written")
	}
	gotEvents := make(map[string]bool)
	for _, e := range entries {
		gotEvents[e["event"].(string)] = true
	}
	if !gotEvents["message_sent"] {
		t.Errorf("expected message_sent in audit; got %+v", gotEvents)
	}
}

func TestUrlMatchesPattern(t *testing.T) {
	cases := []struct {
		url, pattern string
		want         bool
	}{
		{"https://api.example.com/v1", "api.example.com", true},
		{"https://api.example.com/v1", "slack.com/api/*", false},
		{"https://slack.com/api/chat.postMessage", "slack.com/api/*", true},
		{"https://example.com/x", "missing", false},
	}
	for _, tc := range cases {
		if got := urlMatchesPattern(tc.url, tc.pattern); got != tc.want {
			t.Errorf("urlMatchesPattern(%q, %q) = %v, want %v", tc.url, tc.pattern, got, tc.want)
		}
	}
}

// errSendFailed is a sentinel used by the dispatch-error tests above.
type errSendFailed struct{}

func (errSendFailed) Error() string { return "slack returned 500" }

// readAuditMessages parses every `audit-*.jsonl` line under
// <dir>/audit/ as a generic map. Used by audit-shape assertions
// where pulling in the audit package's reader would couple the test
// to its struct field names — comms audit is loose enough that map
// access is friendlier.
func readAuditMessages(t *testing.T, dir string) []map[string]any {
	t.Helper()
	auditDir := dir + "/audit"
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("read audit dir: %v", err)
	}
	var out []map[string]any
	for _, e := range entries {
		data, err := os.ReadFile(auditDir + "/" + e.Name())
		if err != nil {
			t.Fatalf("read audit file: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("decode audit line: %v", err)
			}
			out = append(out, entry)
		}
	}
	return out
}

// --- helpers ---

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// (mustDecode is defined alongside other handler tests in this package.)