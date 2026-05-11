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

// /v1/sessions/{id}/comms/* contract under the non-blocking flow:
//
//   - GET /comms/messages — 200 with the queue snapshot, marks messages
//     read on success. 503 when the daemon was not configured with a
//     notify queue. (Unchanged.)
//   - POST /comms/send / /comms/draft / /comms/http — 202 with an
//     ActionRunPendingResponse; the daemon dispatches in a background
//     goroutine once the user decides via the queue. The eventual
//     outcome (completed / failed / denied) lives on the queue's
//     [approval.Outcome] for the matching approval id, surfaced via
//     `GET /v1/action-approvals/{id}/result` and the `check_action_status`
//     MCP tool.
//
// Tests below exercise the request side (shape of 202), the queue-
// dispatch side (which Outcome is reached), and the side effects
// (listener.Send called / not called, upstream HTTP hit / not hit,
// audit lines written).

// newCommsServer builds an apiServer wired with a fresh notify queue,
// listener registry, and approval queue. Tests drive the queue
// directly via Decide() to simulate user verdicts; the listener
// registry stays empty unless the test populates it.
func newCommsServer(t *testing.T) *apiServer {
	t.Helper()
	return &apiServer{
		log:             slog.Default(),
		notifyQueue:     comms.NewNotifyQueue(100, nil),
		listeners:       comms.NewListenerRegistry(),
		actionApprovals: approval.NewActionApprovalQueue(nil, nil),
	}
}

// fakeListener captures Send calls so tests can assert on dispatch
// without round-tripping through the real Slack/Discord SDKs.
type fakeListener struct {
	service string
	sent    []comms.OutgoingMessage
	sendErr error
}

func (f *fakeListener) Service() string                { return f.service }
func (f *fakeListener) Connect(context.Context) error  { return nil }
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

// startPendingComms POSTs body to handler, asserts 202, decodes the
// ActionRunPendingResponse, and returns it for follow-up Decide /
// Outcome polling. Centralises the boilerplate so each test focuses
// on its specific scenario.
func startPendingComms[T any](
	t *testing.T,
	handler func(http.ResponseWriter, *http.Request, string),
	urlPath, sessionID string,
	reqBody T,
) api.ActionRunPendingResponse {
	t.Helper()
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, urlPath, bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, req, sessionID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var pending api.ActionRunPendingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil {
		t.Fatalf("decode pending: %v; body=%s", err, w.Body.String())
	}
	return pending
}

// waitForCommsOutcome polls the approval queue's Outcome for the
// given approval id until status == want or the test times out.
// Returns the final outcome for further assertions. Failing here
// means the background goroutine never reached the expected
// terminal state — that's a real bug, not flake; don't drop the
// timeout below ~1s.
func waitForCommsOutcome(t *testing.T, q *approval.ActionApprovalQueue, id string, want approval.OutcomeStatus) approval.Outcome {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got approval.Outcome
	for time.Now().Before(deadline) {
		var ok bool
		got, ok = q.Outcome(id)
		if ok && got.Status == want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("outcome for %q never reached %q; last seen = %+v", id, want, got)
	return got
}

// decodeCommsResult parses the JSON in [approval.Outcome.Result] back
// into [commsRunResult] so tests can assert on dispatch outcomes the
// same way they used to assert on the synchronous CommsToolResponse
// body.
func decodeCommsResult(t *testing.T, raw string) commsRunResult {
	t.Helper()
	if raw == "" {
		return commsRunResult{}
	}
	var out commsRunResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode comms result: %v; raw=%s", err, raw)
	}
	return out
}

// --- ReadCommsMessages ---

func TestReadCommsMessages_HappyPath(t *testing.T) {
	s := newCommsServer(t)
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
	if got := s.notifyQueue.UnreadCount(); got != 0 {
		t.Errorf("UnreadCount after read = %d, want 0", got)
	}
}

func TestReadCommsMessages_FilterByService(t *testing.T) {
	s := newCommsServer(t)
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
	s := newCommsServer(t)
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

func TestSendCommsMessage_Returns202WithPendingResponse(t *testing.T) {
	s := newCommsServer(t)
	s.listeners.Set("slack", &fakeListener{service: "slack"})
	s.webappURL = "http://127.0.0.1:54321"

	pending := startPendingComms(t, s.SendCommsMessage, "/v1/sessions/sess-1/comms/send", "sess-1",
		api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "ship it"})

	if pending.Status != api.ActionRunPendingResponseStatusPendingApproval {
		t.Errorf("status = %q, want pending_approval", pending.Status)
	}
	if pending.ApprovalId == "" {
		t.Error("approval_id missing from pending response")
	}
	if pending.ReviewUrl == nil || *pending.ReviewUrl == "" {
		t.Error("review_url not populated despite webappURL set on server")
	}
	if pending.Message == "" {
		t.Error("message is empty; the agent uses this to prompt the user")
	}
}

func TestSendCommsMessage_ApprovedDispatches(t *testing.T) {
	s := newCommsServer(t)
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)

	pending := startPendingComms(t, s.SendCommsMessage, "/v1/sessions/sess-1/comms/send", "sess-1",
		api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "ship it"})

	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeCompleted)
	got := decodeCommsResult(t, outcome.Result)
	if !got.Ok {
		t.Errorf("result ok=false: %+v", got)
	}
	if len(listener.sent) != 1 || listener.sent[0].Body != "ship it" {
		t.Errorf("listener sent = %+v, want one message body=ship it", listener.sent)
	}
}

func TestSendCommsMessage_DeniedSkipsDispatch(t *testing.T) {
	s := newCommsServer(t)
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)

	pending := startPendingComms(t, s.SendCommsMessage, "/v1/sessions/sess-1/comms/send", "sess-1",
		api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "ship it"})

	if err := s.actionApprovals.Decide(pending.ApprovalId, false, "wrong channel", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeDenied)
	if outcome.DenyReason != "wrong channel" {
		t.Errorf("deny_reason = %q, want \"wrong channel\"", outcome.DenyReason)
	}
	if len(listener.sent) != 0 {
		t.Errorf("listener received %d sends on deny path; want 0", len(listener.sent))
	}
}

func TestSendCommsMessage_MissingFields400(t *testing.T) {
	s := newCommsServer(t)
	s.listeners.Set("slack", &fakeListener{service: "slack"})
	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack"}) // missing channel, body
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSendCommsMessage_NoListenerReturns400(t *testing.T) {
	// Listener registry empty → fail fast with 400. Pre-async this
	// returned 200/{ok:false}; the new contract is "don't ask the
	// user to approve a dispatch we can't make."
	s := newCommsServer(t)
	body, _ := json.Marshal(api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no_listener") {
		t.Errorf("expected no_listener error class, got %s", w.Body.String())
	}
}

func TestSendCommsMessage_GarbageBody400(t *testing.T) {
	s := newCommsServer(t)
	s.listeners.Set("slack", &fakeListener{service: "slack"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/send", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.SendCommsMessage(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSendCommsMessage_NoApprovalQueue503(t *testing.T) {
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

func TestSendCommsMessage_NoCommsSurface503(t *testing.T) {
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
	s := newCommsServer(t)
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev", Author: "alice", Body: "is the deploy blocked?"})

	pending := startPendingComms(t, s.DraftCommsReply, "/v1/sessions/sess-1/comms/draft", "sess-1",
		api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "no, all clear"})

	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeCompleted)
	got := decodeCommsResult(t, outcome.Result)
	if !got.Ok {
		t.Errorf("result ok=false: %+v", got)
	}
	if len(listener.sent) != 1 || listener.sent[0].Body != "no, all clear" {
		t.Errorf("listener sent = %+v", listener.sent)
	}
}

func TestDraftCommsReply_EditedBodyWins(t *testing.T) {
	// User edits the draft via EditedPayload["body"] — the dispatcher
	// must send the edited bytes, not the agent's original draft.
	s := newCommsServer(t)
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev"})

	pending := startPendingComms(t, s.DraftCommsReply, "/v1/sessions/sess-1/comms/draft", "sess-1",
		api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "agent's draft"})

	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", map[string]any{"body": "edited reply"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeCompleted)
	if len(listener.sent) != 1 || listener.sent[0].Body != "edited reply" {
		t.Errorf("listener sent = %+v, want edited body", listener.sent)
	}
}

func TestDraftCommsReply_MissingFields400(t *testing.T) {
	s := newCommsServer(t)
	body, _ := json.Marshal(api.DraftCommsReplyRequest{ReplyTo: "x"}) // missing body
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestDraftCommsReply_OriginalEvictedFailsOutcome(t *testing.T) {
	// Original message no longer in the queue — the background
	// goroutine surfaces this as a SetFailed with a descriptive
	// error after the user decides.
	s := newCommsServer(t)

	pending := startPendingComms(t, s.DraftCommsReply, "/v1/sessions/x/comms/draft", "x",
		api.DraftCommsReplyRequest{ReplyTo: "evicted", Body: "hi"})

	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeFailed)
	if !strings.Contains(outcome.ErrorMessage, "no longer available") {
		t.Errorf("error_message = %q, want 'no longer available' detail", outcome.ErrorMessage)
	}
}

func TestDraftCommsReply_NoListenerForService(t *testing.T) {
	// Original message was on a service that's no longer registered.
	// The background goroutine surfaces this as SetFailed.
	s := newCommsServer(t)
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev"})

	pending := startPendingComms(t, s.DraftCommsReply, "/v1/sessions/x/comms/draft", "x",
		api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "hi"})

	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeFailed)
	if !strings.Contains(outcome.ErrorMessage, "no listener for service") {
		t.Errorf("error_message = %q, want 'no listener for service'", outcome.ErrorMessage)
	}
}

func TestDraftCommsReply_DispatchErrorFailsOutcome(t *testing.T) {
	s := newCommsServer(t)
	s.listeners.Set("slack", &fakeListener{service: "slack", sendErr: errSendFailed{}})
	s.notifyQueue.Push(comms.Message{ID: "msg-1", Source: "slack", Channel: "#dev"})

	pending := startPendingComms(t, s.DraftCommsReply, "/v1/sessions/x/comms/draft", "x",
		api.DraftCommsReplyRequest{ReplyTo: "msg-1", Body: "hi"})
	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeFailed)
	if !strings.Contains(outcome.ErrorMessage, "dispatch failed") {
		t.Errorf("error_message = %q, want 'dispatch failed'", outcome.ErrorMessage)
	}
}

func TestDraftCommsReply_NoQueue503(t *testing.T) {
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
	s := newCommsServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/draft", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.DraftCommsReply(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
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

	s := newCommsServer(t)
	pending := startPendingComms(t, s.RequestCommsHTTP, "/v1/sessions/x/comms/http", "x",
		api.RequestCommsHTTPRequest{Method: "GET", Url: upstream.URL})

	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeCompleted)
	got := decodeCommsResult(t, outcome.Result)
	if !got.Ok {
		t.Errorf("result ok=false: %+v", got)
	}
	if len(got.Messages) != 1 || !strings.Contains(got.Messages[0].Body, "ok") {
		t.Errorf("response messages = %+v", got.Messages)
	}
}

func TestRequestCommsHTTP_BearerInjectedFromVault(t *testing.T) {
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

	s := newCommsServer(t)
	s.vault = v

	pending := startPendingComms(t, s.RequestCommsHTTP, "/v1/sessions/x/comms/http", "x",
		api.RequestCommsHTTPRequest{Method: "GET", Url: upstream.URL})
	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	select {
	case auth := <-gotAuth:
		if auth != "Bearer super-secret" {
			t.Errorf("Authorization header = %q, want 'Bearer super-secret'", auth)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}
}

func TestRequestCommsHTTP_DeniedSkipsDispatch(t *testing.T) {
	s := newCommsServer(t)
	pending := startPendingComms(t, s.RequestCommsHTTP, "/v1/sessions/x/comms/http", "x",
		api.RequestCommsHTTPRequest{Method: "GET", Url: "https://example.com"})
	if err := s.actionApprovals.Decide(pending.ApprovalId, false, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeDenied)
}

func TestRequestCommsHTTP_GarbageBody400(t *testing.T) {
	s := newCommsServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRequestCommsHTTP_MissingFields400(t *testing.T) {
	s := newCommsServer(t)
	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET"}) // missing url
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRequestCommsHTTP_NoApprovalQueue503(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	body, _ := json.Marshal(api.RequestCommsHTTPRequest{Method: "GET", Url: "https://x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/comms/http", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestCommsHTTP(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestRequestCommsHTTP_DispatchFailureSurfacesOutcome(t *testing.T) {
	s := newCommsServer(t)
	pending := startPendingComms(t, s.RequestCommsHTTP, "/v1/sessions/x/comms/http", "x",
		api.RequestCommsHTTPRequest{Method: "GET", Url: "http://127.0.0.1:1"})
	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeFailed)
	if !strings.Contains(outcome.ErrorMessage, "dispatch failed") {
		t.Errorf("error_message = %q, want 'dispatch failed'", outcome.ErrorMessage)
	}
}

func TestRequestCommsHTTP_HeadersInjected(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("X-Custom")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	s := newCommsServer(t)
	pending := startPendingComms(t, s.RequestCommsHTTP, "/v1/sessions/x/comms/http", "x",
		api.RequestCommsHTTPRequest{
			Method:  "GET",
			Url:     upstream.URL,
			Headers: ptr(`{"X-Custom":"hello"}`),
		})
	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	select {
	case v := <-got:
		if v != "hello" {
			t.Errorf("X-Custom = %q, want hello", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}
}

// --- Audit ---

func TestSendCommsMessage_AuditWriteHappensOnDispatch(t *testing.T) {
	dir := t.TempDir()
	s := newCommsServer(t)
	s.auditStateDir = dir
	listener := &fakeListener{service: "slack"}
	s.listeners.Set("slack", listener)

	pending := startPendingComms(t, s.SendCommsMessage, "/v1/sessions/sess-1/comms/send", "sess-1",
		api.SendCommsMessageRequest{Service: "slack", Channel: "#dev", Body: "ship it"})
	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeCompleted)

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

// --- url-pattern matching (unchanged helpers) ---

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

// SendCommsMessage_DispatchErrorFailsOutcome rounds out the dispatch-
// error coverage; the listener returns an error from Send and the
// background goroutine records it as a failure outcome.
func TestSendCommsMessage_DispatchErrorFailsOutcome(t *testing.T) {
	s := newCommsServer(t)
	s.listeners.Set("slack", &fakeListener{service: "slack", sendErr: errSendFailed{}})

	pending := startPendingComms(t, s.SendCommsMessage, "/v1/sessions/x/comms/send", "x",
		api.SendCommsMessageRequest{Service: "slack", Channel: "#x", Body: "hi"})
	if err := s.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	outcome := waitForCommsOutcome(t, s.actionApprovals, pending.ApprovalId, approval.OutcomeFailed)
	if !strings.Contains(outcome.ErrorMessage, "dispatch failed") {
		t.Errorf("error_message = %q, want 'dispatch failed'", outcome.ErrorMessage)
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
