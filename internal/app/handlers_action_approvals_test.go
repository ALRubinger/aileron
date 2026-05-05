package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// req2ctx is a tiny helper to keep the test bodies readable;
// the imports declared at file scope are enough.
func req2ctx() context.Context { return context.Background() }
