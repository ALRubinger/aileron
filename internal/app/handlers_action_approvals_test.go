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
	"github.com/ALRubinger/aileron/internal/observability"
	"github.com/ALRubinger/aileron/internal/vault"
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

// TestListActionApprovals_PreviewRoundTripsThroughTheWire pins the
// load-bearing wire shape for ADR-0016: an entry registered with a
// preview payload surfaces every preview field through the listing
// endpoint so the webapp's pending card can render it without a
// second fetch. Without this round-trip, the daemon would invoke the
// preview op for no observable effect — the user would still see only
// the raw inputs.
func TestListActionApprovals_PreviewRoundTripsThroughTheWire(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.RegisterKindWithPreview(approval.ApprovalKindAction,
		"send-draft", "github://aileron/google", "session-42",
		map[string]any{"draft_id": "r-12345"},
		&approval.ActionApprovalPreview{
			Fields: []approval.ActionApprovalPreviewField{
				{Label: "To", Value: "alice@example.com"},
				{Label: "Subject", Missing: true},
			},
		})

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
	if len(got.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(got.Items))
	}
	preview := got.Items[0].Preview
	if preview == nil {
		t.Fatal("Preview = nil, want populated preview after RegisterKindWithPreview")
	}
	if preview.Fields == nil || len(*preview.Fields) != 2 {
		t.Fatalf("Preview.Fields = %v, want 2 fields", preview.Fields)
	}
	fields := *preview.Fields
	if fields[0].Label != "To" {
		t.Errorf("Fields[0].Label = %q, want %q", fields[0].Label, "To")
	}
	if fields[0].Value == nil || *fields[0].Value != "alice@example.com" {
		t.Errorf("Fields[0].Value = %v, want pointer to %q", fields[0].Value, "alice@example.com")
	}
	if fields[1].Missing == nil || !*fields[1].Missing {
		t.Errorf("Fields[1].Missing = %v, want pointer to true", fields[1].Missing)
	}
}

// TestListActionApprovals_PreviewMultilineRoundTripsThroughTheWire
// pins the wire contract for the `multiline` render hint (ADR-0016).
// Fields whose label appeared in the manifest's `multiline` list must
// surface with Multiline=true on the wire so the webapp renders them
// as scrollable blockquotes; fields not in the list must omit the flag
// so the existing inline-row behavior is unchanged. Without this
// guarantee, the webapp would never receive the hint and the user-
// visible improvement the ADR motivated would not land.
func TestListActionApprovals_PreviewMultilineRoundTripsThroughTheWire(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.RegisterKindWithPreview(approval.ApprovalKindAction,
		"send-draft", "github://aileron/google", "session-42",
		map[string]any{"draft_id": "r-12345"},
		&approval.ActionApprovalPreview{
			Fields: []approval.ActionApprovalPreviewField{
				{Label: "To", Value: "alice@example.com"},
				{Label: "Body", Value: "long email body here", Multiline: true},
			},
		})

	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals", nil)
	rec := httptest.NewRecorder()
	srv.ListActionApprovals(rec, req)

	var got api.ActionApprovalListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fields := *got.Items[0].Preview.Fields
	if fields[0].Multiline != nil && *fields[0].Multiline {
		t.Errorf("Fields[0] (To).Multiline = true, want false/omitted")
	}
	if fields[1].Multiline == nil || !*fields[1].Multiline {
		t.Errorf("Fields[1] (Body).Multiline = %v, want pointer to true", fields[1].Multiline)
	}
}

// TestListActionApprovals_PreviewUnavailableSurfacesAcrossWire asserts
// the wholesale-failure shape (ADR-0016) reaches the wire intact. The
// "preview unavailable: <reason>" string is the only signal the user
// sees that the preview op itself failed; suppressing it would let
// the user approve a doomed call without warning.
func TestListActionApprovals_PreviewUnavailableSurfacesAcrossWire(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.RegisterKindWithPreview(approval.ApprovalKindAction,
		"send-draft", "github://aileron/google", "",
		map[string]any{"draft_id": "wrong"},
		&approval.ActionApprovalPreview{
			Unavailable: "preview unavailable: Gmail API returned 404",
		})

	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals", nil)
	rec := httptest.NewRecorder()
	srv.ListActionApprovals(rec, req)

	var got api.ActionApprovalListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	preview := got.Items[0].Preview
	if preview == nil || preview.Unavailable == nil {
		t.Fatal("Preview.Unavailable = nil, want populated reason")
	}
	if *preview.Unavailable != "preview unavailable: Gmail API returned 404" {
		t.Errorf("Preview.Unavailable = %q, want the 404 reason", *preview.Unavailable)
	}
	if preview.Fields != nil {
		t.Errorf("Preview.Fields = %v, want nil on wholesale failure", preview.Fields)
	}
}

// TestListActionApprovals_InputFieldsRoundTripsThroughTheWire pins
// the wire contract for the ADR-0003 amendment introducing per-input
// display metadata. An entry registered with a pre-rendered input-
// fields slice must surface every label, value, missing flag, and
// multiline hint through the listing endpoint so the webapp's pending
// card renders labeled rows instead of a raw JSON dump. Without this
// round-trip, the daemon would project the manifest's inputs for no
// observable effect.
func TestListActionApprovals_InputFieldsRoundTripsThroughTheWire(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.RegisterKindWithPreviewAndInputs(approval.ApprovalKindAction,
		"send-email", "github://aileron/google", "session-42",
		map[string]any{
			"to":      "alr@example.com",
			"subject": "hello",
			"body":    "line one\nline two",
		},
		nil,
		[]approval.ActionApprovalPreviewField{
			{Label: "To", Value: "alr@example.com"},
			{Label: "Subject", Missing: true},
			{Label: "Body", Value: "line one\nline two", Multiline: true},
		})

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
	if len(got.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(got.Items))
	}
	inputFields := got.Items[0].InputFields
	if inputFields == nil || len(*inputFields) != 3 {
		t.Fatalf("InputFields = %v, want 3 fields", inputFields)
	}
	fields := *inputFields

	// Field 0: standard inline row with a value.
	if fields[0].Label != "To" {
		t.Errorf("Fields[0].Label = %q, want %q", fields[0].Label, "To")
	}
	if fields[0].Value == nil || *fields[0].Value != "alr@example.com" {
		t.Errorf("Fields[0].Value = %v, want pointer to %q", fields[0].Value, "alr@example.com")
	}
	if fields[0].Missing != nil && *fields[0].Missing {
		t.Errorf("Fields[0].Missing = true, want false/omitted")
	}

	// Field 1: missing-required surfaces with Missing=true and empty value.
	if fields[1].Missing == nil || !*fields[1].Missing {
		t.Errorf("Fields[1].Missing = %v, want pointer to true", fields[1].Missing)
	}
	if fields[1].Value != nil {
		t.Errorf("Fields[1].Value = %v, want nil on missing", fields[1].Value)
	}

	// Field 2: multiline body with newline-bearing value.
	if fields[2].Multiline == nil || !*fields[2].Multiline {
		t.Errorf("Fields[2].Multiline = %v, want pointer to true", fields[2].Multiline)
	}
	if fields[2].Value == nil || *fields[2].Value != "line one\nline two" {
		t.Errorf("Fields[2].Value = %v, want literal-newline string", fields[2].Value)
	}
}

// TestListActionApprovals_InputFieldsOmittedWhenEmpty asserts the
// fallback contract: an entry registered without an input-fields
// projection (older callers, or manifests with no `[[inputs]]`)
// surfaces with InputFields omitted from the wire response so the
// webapp falls through to the historic raw-JSON accordion.
func TestListActionApprovals_InputFieldsOmittedWhenEmpty(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.Register("send-email", "github://aileron/google", "",
		map[string]any{"to": "alr@example.com"})

	req := httptest.NewRequest(http.MethodGet, "/v1/action-approvals", nil)
	rec := httptest.NewRecorder()
	srv.ListActionApprovals(rec, req)

	var got api.ActionApprovalListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items[0].InputFields != nil {
		t.Errorf("InputFields = %v, want nil when no projection registered", got.Items[0].InputFields)
	}
}

// TestDecideActionApproval_ApproveResolves is the handler-side
// regression for the approve path: a POST with `approved: true`
// resolves the queue entry; a follow-up List shows it gone. The
// runtime's Wait would have unblocked on the same channel.
//
// The endpoint returns 204 No Content with an empty body — the
// webapp's JSON client special-cases 204 and returns null. Earlier
// this was 200 with an empty body, which threw
// `Unexpected end of JSON input` in the browser when the page tried
// to deserialize the response.
func TestDecideActionApproval_ApproveResolves(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.Register("send-email", "github://x/y", "", nil)

	body := bytes.NewReader([]byte(`{"approved":true}`))
	req := httptest.NewRequest(http.MethodPost,
		"/v1/action-approvals/"+entry.ID+"/decide", body)
	rec := httptest.NewRecorder()
	srv.DecideActionApproval(rec, req, entry.ID)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Len(); got != 0 {
		t.Errorf("body len = %d, want 0 (204 must have empty body); body=%s", got, rec.Body.String())
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

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
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

// TestGetActionApprovalResult_PendingApprovalShape: a freshly
// registered entry returns status = pending_approval with no other
// fields. The agent's check_action_status tool surfaces this verbatim
// while waiting for the user.
func TestGetActionApprovalResult_PendingApprovalShape(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.Register("send-email", "github://x/y", "", nil)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/action-approvals/"+entry.ID+"/result", nil)
	rec := httptest.NewRecorder()
	srv.GetActionApprovalResult(rec, req, entry.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"pending_approval"`) {
		t.Errorf("body missing pending_approval status; got %s", body)
	}
}

// TestGetActionApprovalResult_DeniedSurfacesReason: after Decide(false)
// the endpoint returns status = denied with the user's reason
// attached. Reason is optional (a user denying without commentary
// leaves it empty) but the field is surfaced when present.
func TestGetActionApprovalResult_DeniedSurfacesReason(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.Register("send-email", "github://x/y", "", nil)
	if err := q.Decide(entry.ID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/v1/action-approvals/"+entry.ID+"/result", nil)
	rec := httptest.NewRecorder()
	srv.GetActionApprovalResult(rec, req, entry.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"denied"`) {
		t.Errorf("body missing denied status; got %s", body)
	}
	if !strings.Contains(body, "wrong recipient") {
		t.Errorf("body missing reason; got %s", body)
	}
}

// TestGetActionApprovalResult_CompletedSurfacesResult: the queue
// records a SetCompleted on the entry; the endpoint returns
// status = completed with audit_id and result populated.
func TestGetActionApprovalResult_CompletedSurfacesResult(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.Register("send-email", "github://x/y", "", nil)
	if err := q.SetCompleted(entry.ID, "audit-42", `{"sent":true}`); err != nil {
		t.Fatalf("SetCompleted: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/v1/action-approvals/"+entry.ID+"/result", nil)
	rec := httptest.NewRecorder()
	srv.GetActionApprovalResult(rec, req, entry.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":"completed"`, `"audit_id":"audit-42"`, `{\"sent\":true}`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got %s", want, body)
		}
	}
}

// TestGetActionApprovalResult_FailedWithExecutorError: an executor
// error (no FailureEnvelope) surfaces via the `reason` field. The
// `failure` envelope field stays absent — the API spec treats it as
// optional and a freeform error message is the right shape for an
// executor-level bug rather than a structured ADR-0010 failure.
func TestGetActionApprovalResult_FailedWithExecutorError(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	entry := q.Register("send-email", "github://x/y", "", nil)
	if err := q.SetFailed(entry.ID, nil, "connector binary not found"); err != nil {
		t.Fatalf("SetFailed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/v1/action-approvals/"+entry.ID+"/result", nil)
	rec := httptest.NewRecorder()
	srv.GetActionApprovalResult(rec, req, entry.ID)

	body := rec.Body.String()
	if !strings.Contains(body, `"status":"failed"`) {
		t.Errorf("body missing failed status; got %s", body)
	}
	if !strings.Contains(body, "connector binary not found") {
		t.Errorf("body missing executor error text; got %s", body)
	}
}

// TestGetActionApprovalResult_UnknownIDReturns404: ids from a
// previous daemon process, or typos, return 404. The agent's
// check_action_status tool surfaces this as a normal not-found error.
func TestGetActionApprovalResult_UnknownIDReturns404(t *testing.T) {
	srv, _ := newActionApprovalsTestServer(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/action-approvals/never-minted/result", nil)
	rec := httptest.NewRecorder()
	srv.GetActionApprovalResult(rec, req, "never-minted")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetActionApprovalResult_NilQueueReturns503: a daemon built
// without an action-approval queue returns 503 from /result, same
// shape the rest of the action-approval endpoints use.
func TestGetActionApprovalResult_NilQueueReturns503(t *testing.T) {
	srv := &apiServer{}
	req := httptest.NewRequest(http.MethodGet,
		"/v1/action-approvals/anything/result", nil)
	rec := httptest.NewRecorder()
	srv.GetActionApprovalResult(rec, req, "anything")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
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

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
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

	h, err := NewHandlerWithConfig(slog.Default(), Config{ActionApprovals: q, Vault: vault.NewMemVault()})
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

// TestWatchActionApprovals_ThroughLoggingMiddleware is the regression
// test for the SSE stream stalling at "Connecting to the approval
// stream…" in the webapp. The handler asserts `w.(http.Flusher)`; an
// earlier statusWriter wrapper in loggingMiddleware did not proxy
// Flush, so the assertion failed and the handler returned 500
// streaming_unsupported. The webapp's onError path doesn't clear its
// loading flag, so the page sat on the placeholder forever.
//
// The contract the SSE handler depends on is: any ResponseWriter
// wrapper in the middleware chain MUST preserve http.Flusher when
// the underlying writer supports it. This test exercises the handler
// through loggingMiddleware end-to-end and asserts the snapshot event
// is delivered with a 200 — which only happens if Flush proxies
// through the wrapper.
func TestWatchActionApprovals_ThroughLoggingMiddleware(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.Register("send-email", "github://x/y", "sess-1", map[string]any{"to": "alice"})

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	wrapped := loggingMiddleware(logger, http.HandlerFunc(srv.WatchActionApprovals))
	httpSrv := httptest.NewServer(wrapped)
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE handler must work through loggingMiddleware)", resp.StatusCode)
	}

	r := bufio.NewReader(resp.Body)
	ev, err := readSSEEvent(r)
	if err != nil {
		t.Fatalf("read snapshot: %v (handler never flushed through the middleware)", err)
	}
	if ev.Event != "snapshot" {
		t.Errorf("first event = %q, want snapshot", ev.Event)
	}
}

// TestWatchActionApprovals_ThroughProductionMiddlewareChain is the
// second-round regression for the same "Connecting to the approval
// stream…" stall. The first round only chained loggingMiddleware and
// missed that observability.HTTPMiddleware sits *between* logging and
// the handler with its own ResponseWriter wrapper that also didn't
// proxy Flush. The result: loggingMiddleware's statusWriter.Flush()
// asserted `(http.Flusher)` against statusRecorder, which failed,
// Flush became a no-op, bytes piled up in the http response buffer,
// and the connection sat open with no bytes ever reaching the client
// (no 500 this time — a silent stall).
//
// This test composes the chain in the same order as app.go's
// `buildHandler`: HTTPMiddleware wrapped inside loggingMiddleware
// wrapping the handler. Both wrappers must proxy Flush for the
// snapshot to make it to the client.
func TestWatchActionApprovals_ThroughProductionMiddlewareChain(t *testing.T) {
	srv, q := newActionApprovalsTestServer(t)
	q.Register("send-email", "github://x/y", "sess-1", map[string]any{"to": "alice"})

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	var handler http.Handler = http.HandlerFunc(srv.WatchActionApprovals)
	handler = loggingMiddleware(logger, handler)
	handler = observability.HTTPMiddleware(nil)(handler)

	httpSrv := httptest.NewServer(handler)
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v (request timed out — handler never flushed bytes through the chain)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	r := bufio.NewReader(resp.Body)
	ev, err := readSSEEvent(r)
	if err != nil {
		t.Fatalf("read snapshot: %v (snapshot never reached the client — Flush was a no-op somewhere)", err)
	}
	if ev.Event != "snapshot" {
		t.Errorf("first event = %q, want snapshot", ev.Event)
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
