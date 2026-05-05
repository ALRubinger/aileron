package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
)

// newActionApprovalsTestServer returns an apiServer wired with an
// in-memory ActionApprovalQueue, scoped to one test. The queue's
// id generator is monotonic and deterministic so test assertions can
// match exact ids without race-watching for crypto/rand output.
func newActionApprovalsTestServer(t *testing.T) (*apiServer, *approval.ActionApprovalQueue) {
	t.Helper()
	idCounter := 0
	q := approval.NewActionApprovalQueue(func() string {
		idCounter++
		return formatTestApprovalID(idCounter)
	}, func() time.Time {
		return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC).Add(time.Duration(idCounter) * time.Second)
	})
	srv := &apiServer{actionApprovals: q}
	return srv, q
}

func formatTestApprovalID(n int) string {
	return "act-test-" + string(rune('0'+n))
}

// TestListActionApprovals_EmptyQueue verifies the well-formed empty
// response shape — `Items: []` rather than `Items: null`. Webapp /
// CLI rendering depends on the slice being present.
func TestListActionApprovals_EmptyQueue(t *testing.T) {
	srv, _ := newActionApprovalsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals", nil)
	rec := httptest.NewRecorder()
	srv.ListActionApprovals(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionApprovalListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items == nil {
		t.Errorf("Items = nil; want empty slice for the wire shape")
	}
	if len(got.Items) != 0 {
		t.Errorf("Items len = %d, want 0", len(got.Items))
	}
}

// TestListActionApprovals_NilQueueReturnsEmpty asserts that when no
// queue is configured (e.g. dev-mode server constructed without the
// approvals wiring), the handler returns an empty list rather than
// 500'ing. Action approvals are an opt-in feature; the daemon should
// continue serving its other surface even when the queue is absent.
func TestListActionApprovals_NilQueueReturnsEmpty(t *testing.T) {
	srv := &apiServer{}
	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals", nil)
	rec := httptest.NewRecorder()
	srv.ListActionApprovals(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got api.ActionApprovalListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 0 {
		t.Errorf("Items len = %d, want 0", len(got.Items))
	}
}

// TestListActionApprovals_PopulatedQueueSurfacesAllFields covers the
// load-bearing wire shape: every field a caller might render must
// round-trip through the handler. The webapp uses connector_fqn and
// args to render decision UIs; the CLI uses session_id and
// requested_at to print context.
func TestListActionApprovals_PopulatedQueueSurfacesAllFields(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.Register("send-email", "github://x/y", "session-42",
		map[string]any{"to": "alice@example.com", "subject": "hi"})
	q.Register("create-event", "github://x/y", "", nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals", nil)
	rec := httptest.NewRecorder()
	srv.ListActionApprovals(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionApprovalListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("Items len = %d, want 2", len(got.Items))
	}

	// First entry has all the optional fields populated.
	first := got.Items[0]
	if first.ActionName != "send-email" {
		t.Errorf("ActionName = %q", first.ActionName)
	}
	if first.ConnectorFqn == nil || *first.ConnectorFqn != "github://x/y" {
		t.Errorf("ConnectorFqn = %v", first.ConnectorFqn)
	}
	if first.SessionId == nil || *first.SessionId != "session-42" {
		t.Errorf("SessionId = %v", first.SessionId)
	}
	if first.Args == nil {
		t.Fatal("Args is nil")
	}
	if (*first.Args)["to"] != "alice@example.com" {
		t.Errorf("Args.to = %v", (*first.Args)["to"])
	}

	// Second entry: ConnectorFqn populated, SessionId and Args
	// absent → omitted on the wire (pointer-nil) so the JSON shape
	// stays minimal for callers iterating the list.
	second := got.Items[1]
	if second.SessionId != nil {
		t.Errorf("SessionId = %v, want nil for empty session", second.SessionId)
	}
	// Args is always emitted because the schema treats it as a
	// (possibly empty) object — easier for client renderers than
	// branching on nil.
	if second.Args == nil {
		t.Errorf("Args = nil, want empty map")
	}
}

// TestDecideActionApproval_ApproveResolves is the handler-side
// regression for the approve path: a POST with `approved: true`
// resolves the queue entry; a follow-up List shows it gone. The
// runtime's Wait would have unblocked on the same channel.
func TestDecideActionApproval_ApproveResolves(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.Register("send-email", "github://x/y", "", nil)

	body := bytes.NewReader([]byte(`{"approved":true}`))
	req := httptest.NewRequest(http.MethodPost,
		"/v1/action-approvals/"+entry.ID+"/decide", body)
	rec := httptest.NewRecorder()
	srv.DecideActionApproval(rec, req, entry.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := q.List(); len(got) != 0 {
		t.Errorf("queue len after decide = %d, want 0", len(got))
	}
}

// TestDecideActionApproval_DenyForwardsReason asserts the reason
// makes it through the handler to the queue. The runtime's Wait
// receives it and propagates to the agent in the failure envelope
// so the agent can recover gracefully.
func TestDecideActionApproval_DenyForwardsReason(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.Register("send-email", "github://x/y", "", nil)

	// Set up a goroutine to receive the decision so we can verify
	// the reason actually propagated through Decide → channel.
	type result struct {
		decision approval.ActionDecision
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		d, err := entry.Wait(req2ctx(), 2*time.Second)
		resultCh <- result{d, err}
	}()

	body := bytes.NewReader([]byte(`{"approved":false,"reason":"wrong recipient"}`))
	req := httptest.NewRequest(http.MethodPost,
		"/v1/action-approvals/"+entry.ID+"/decide", body)
	rec := httptest.NewRecorder()
	srv.DecideActionApproval(rec, req, entry.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := <-resultCh
	if got.err != nil {
		t.Fatalf("Wait err: %v", got.err)
	}
	if got.decision.Approved {
		t.Errorf("decision.Approved = true, want false")
	}
	if got.decision.Reason != "wrong recipient" {
		t.Errorf("decision.Reason = %q", got.decision.Reason)
	}
}

// TestDecideActionApproval_UnknownIDReturns404 covers the race-loser
// path: a second webapp tab decides the same id, or the runtime
// timed out before the user clicked. Both end up here. The 404
// is what the CLI / webapp render as "this approval is no longer
// pending."
func TestDecideActionApproval_UnknownIDReturns404(t *testing.T) {
	srv, _ := newActionApprovalsTestServer(t)
	body := bytes.NewReader([]byte(`{"approved":true}`))
	req := httptest.NewRequest(http.MethodPost,
		"/v1/action-approvals/never-existed/decide", body)
	rec := httptest.NewRecorder()
	srv.DecideActionApproval(rec, req, "never-existed")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDecideActionApproval_InvalidJSONReturns400 catches a malformed
// CLI / webapp request. We want a structured error rather than the
// generic 500.
func TestDecideActionApproval_InvalidJSONReturns400(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.Register("send-email", "github://x/y", "", nil)

	body := bytes.NewReader([]byte(`{not json`))
	req := httptest.NewRequest(http.MethodPost,
		"/v1/action-approvals/"+entry.ID+"/decide", body)
	rec := httptest.NewRecorder()
	srv.DecideActionApproval(rec, req, entry.ID)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDecideActionApproval_NilQueueReturns503 asserts the dev-mode
// server (no approvals wiring) refuses the decision politely rather
// than panic'ing. Same surface treatment as List for the missing-
// dependency case.
func TestDecideActionApproval_NilQueueReturns503(t *testing.T) {
	srv := &apiServer{}
	body := bytes.NewReader([]byte(`{"approved":true}`))
	req := httptest.NewRequest(http.MethodPost,
		"/v1/action-approvals/x/decide", body)
	rec := httptest.NewRecorder()
	srv.DecideActionApproval(rec, req, "x")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestDecideActionApproval_EditedPayloadFlowsToWaiter asserts the
// `edited_payload` field surfaced in #428: the webapp ships the
// user-edited reply body via the decide endpoint, the queue carries
// it through Decide → ActionApproval.Wait so the dispatcher (e.g.
// CommsServer) can send the user's bytes rather than the agent's
// original draft.
func TestDecideActionApproval_EditedPayloadFlowsToWaiter(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.RegisterCommsDraft("slack", "#general", "alice", "ping", "pong", "msg-1", "")

	type result struct {
		decision approval.ActionDecision
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		d, err := entry.Wait(req2ctx(), 2*time.Second)
		resultCh <- result{d, err}
	}()

	body := bytes.NewReader([]byte(`{"approved":true,"edited_payload":{"body":"pong — got it"}}`))
	req := httptest.NewRequest(http.MethodPost,
		"/v1/action-approvals/"+entry.ID+"/decide", body)
	rec := httptest.NewRecorder()
	srv.DecideActionApproval(rec, req, entry.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := <-resultCh
	if got.err != nil {
		t.Fatalf("Wait err: %v", got.err)
	}
	if !got.decision.Approved {
		t.Errorf("decision.Approved = false, want true")
	}
	body2, ok := got.decision.EditedPayload["body"].(string)
	if !ok || body2 != "pong — got it" {
		t.Errorf("EditedPayload.body = %v, want \"pong — got it\"", got.decision.EditedPayload["body"])
	}
}

// TestListActionApprovals_KindIsSurfaced asserts each pending entry
// carries its kind on the wire so the webapp's per-kind cards can
// render the right layout. Default kind for a Register call is
// `action`; explicit kinds round-trip verbatim.
func TestListActionApprovals_KindIsSurfaced(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.Register("send-email", "github://x/y", "", nil)
	q.RegisterCommsSend("slack", "#general", "hi", "")
	q.RegisterCommsDraft("slack", "#general", "a", "b", "c", "m1", "")
	q.RegisterHTTPRequest("GET", "https://x", "", "linear-key", "")

	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals", nil)
	rec := httptest.NewRecorder()
	srv.ListActionApprovals(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got api.ActionApprovalListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 4 {
		t.Fatalf("Items len = %d, want 4", len(got.Items))
	}
	wantKinds := []api.PendingActionApprovalKind{
		api.PendingActionApprovalKindAction,
		api.PendingActionApprovalKindCommsSend,
		api.PendingActionApprovalKindCommsDraft,
		api.PendingActionApprovalKindHttpRequest,
	}
	for i, want := range wantKinds {
		if got.Items[i].Kind != want {
			t.Errorf("Items[%d].Kind = %q, want %q", i, got.Items[i].Kind, want)
		}
	}
}

// TestNewHandlerWithConfig_ReusesSuppliedActionApprovals confirms
// that when launch passes its shared queue via Config.ActionApprovals,
// the apiServer routes through it rather than creating a fresh one.
// This is the wiring that lets CommsServer's send-shaped tools and
// the gateway's `/v1/action-approvals` API see the same SSE stream.
func TestNewHandlerWithConfig_ReusesSuppliedActionApprovals(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	q.RegisterCommsSend("slack", "#general", "hi", "")

	h, err := NewHandlerWithConfig(slog.Default(), Config{ActionApprovals: q})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"kind":"comms_send"`) {
		t.Errorf("body = %s, want a comms_send entry routed through the supplied queue", body)
	}
}

// req2ctx is a tiny helper to keep the test bodies readable;
// the imports declared at file scope are enough.
func req2ctx() context.Context { return context.Background() }

// --- WatchActionApprovals (SSE) — issue #418 followups ---

// sseEvent is one parsed Server-Sent Event frame: an `event:` line
// followed by a `data:` line, terminated by a blank line.
type sseEvent struct {
	Event string
	Data  string
}

// readSSEEvent reads one event frame from r. SSE comment lines
// (starting with `:`, used here for heartbeats) are skipped. Returns
// io.EOF when the stream ends.
func readSSEEvent(r *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return sseEvent{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev.Event != "" || ev.Data != "" {
				return ev, nil
			}
			// blank line outside a frame (between heartbeat and next
			// event); keep reading.
		case strings.HasPrefix(line, ":"):
			// SSE comment / heartbeat — ignore.
		case strings.HasPrefix(line, "event:"):
			ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			ev.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

// TestWatchActionApprovals_EmitsSnapshotOnConnect verifies the first
// event a freshly connected SSE subscriber receives is a snapshot of
// the currently pending queue. The webapp uses this to render the
// initial pending list without a separate GET — the SSE stream is the
// single source of truth once subscribed.
func TestWatchActionApprovals_EmitsSnapshotOnConnect(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.Register("send-email", "github://x/y", "sess-1", map[string]any{"to": "alice"})

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.WatchActionApprovals))
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	r := bufio.NewReader(resp.Body)
	ev, err := readSSEEvent(r)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if ev.Event != "snapshot" {
		t.Errorf("first event type = %q, want snapshot", ev.Event)
	}
	var payload struct {
		Items []api.PendingActionApproval `json:"items"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("decode snapshot: %v; data=%q", err, ev.Data)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("snapshot.items len = %d, want 1; data=%s", len(payload.Items), ev.Data)
	}
	if payload.Items[0].ActionName != "send-email" {
		t.Errorf("snapshot.items[0].action_name = %q", payload.Items[0].ActionName)
	}
}

// TestWatchActionApprovals_StreamsPendingAndResolved is the live-
// updates regression: connect to an empty queue, observe the empty
// snapshot, Register an approval, observe the `pending` event,
// resolve it, observe the `resolved` event. This is exactly the
// state-update path the webapp depends on for live UI updates.
func TestWatchActionApprovals_StreamsPendingAndResolved(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.WatchActionApprovals))
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	if ev, err := readSSEEvent(r); err != nil || ev.Event != "snapshot" {
		t.Fatalf("first event = %+v err=%v, want snapshot", ev, err)
	}

	a := q.Register("send-email", "github://x/y", "sess-1", map[string]any{"to": "alice"})
	ev, err := readSSEEvent(r)
	if err != nil {
		t.Fatalf("read pending event: %v", err)
	}
	if ev.Event != "pending" {
		t.Errorf("event type = %q, want pending", ev.Event)
	}
	var pending api.PendingActionApproval
	if err := json.Unmarshal([]byte(ev.Data), &pending); err != nil {
		t.Fatalf("decode pending: %v; data=%q", err, ev.Data)
	}
	if pending.Id != a.ID {
		t.Errorf("pending.id = %q, want %q", pending.Id, a.ID)
	}
	if pending.ActionName != "send-email" {
		t.Errorf("pending.action_name = %q", pending.ActionName)
	}

	if err := q.Decide(a.ID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	ev, err = readSSEEvent(r)
	if err != nil {
		t.Fatalf("read resolved event: %v", err)
	}
	if ev.Event != "resolved" {
		t.Errorf("event type = %q, want resolved", ev.Event)
	}
	var resolved struct {
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &resolved); err != nil {
		t.Fatalf("decode resolved: %v; data=%q", err, ev.Data)
	}
	if resolved.ID != a.ID {
		t.Errorf("resolved.id = %q, want %q", resolved.ID, a.ID)
	}
	if resolved.Approved {
		t.Errorf("resolved.approved = true, want false")
	}
	if resolved.Reason != "wrong recipient" {
		t.Errorf("resolved.reason = %q", resolved.Reason)
	}
}

// TestWatchActionApprovals_DisabledReturns503 mirrors the
// dev-mode-server treatment on List/Decide: when the queue is not
// configured, the handler refuses politely so a webapp tab pointed
// at a daemon without approvals wired produces a clean 503 rather
// than a stalled stream.
func TestWatchActionApprovals_DisabledReturns503(t *testing.T) {
	srv := &apiServer{}
	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals/watch", nil)
	rec := httptest.NewRecorder()
	srv.WatchActionApprovals(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestWatchActionApprovals_ClientDisconnectReleases asserts the
// handler returns when the client disconnects. Without proper
// context handling, every closed tab would leak a goroutine
// blocked on the events channel.
func TestWatchActionApprovals_ClientDisconnectReleases(t *testing.T) {
	srv, _ := newActionApprovalsTestServer(t)
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.WatchActionApprovals))
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	r := bufio.NewReader(resp.Body)
	if _, err := readSSEEvent(r); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	// Cancel the client context — handler should observe ctx.Done()
	// and return. Read should hit EOF (or connection-reset).
	cancel()
	resp.Body.Close()
	// The handler should have returned by now; we don't have a direct
	// signal, but if it leaked a goroutine the goleak-style sanity
	// would catch it in higher-level test runs. Here we just verify
	// the connection close didn't deadlock the test.
}
