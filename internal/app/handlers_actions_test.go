package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/model"
)

const actionsTestManifest = `+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123"
capabilities = ["chat:write", "channels:read"]

[match]
intent = "tell team I shipped"

[[execute]]
id = "post"
connector = "github://aileron/slack"
op = "post_message"
+++

# Ship Update

Posts a "shipped" announcement to a Slack channel.
`

const actionsTestBadManifest = `+++
name = "broken"
version = "x.y"
+++
`

func newActionsTestServer(t *testing.T, files map[string]string) *apiServer {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	store := action.NewStore(dir)
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	return &apiServer{
		log:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		actions:  store,
		executor: action.StubExecutor{},
		newID:    func() string { return "audit_test_id" },
	}
}

func TestListActions_ReturnsInstalledManifests(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/actions", nil)
	rec := httptest.NewRecorder()
	srv.ListActions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items == nil || len(*got.Items) != 1 {
		t.Fatalf("Items = %v, want 1", got.Items)
	}
	a := (*got.Items)[0]
	if a.Name != "ship-update" {
		t.Errorf("Name = %q", a.Name)
	}
	if a.Match.Intent != "tell team I shipped" {
		t.Errorf("Match.Intent = %q", a.Match.Intent)
	}
	if a.Requires.Connectors == nil || len(*a.Requires.Connectors) != 1 {
		t.Fatalf("Requires.Connectors = %v", a.Requires.Connectors)
	}
	if a.Body == nil || (*a.Body == "") {
		t.Errorf("Body should be populated")
	}
	if got.LoadErrors != nil && len(*got.LoadErrors) != 0 {
		t.Errorf("LoadErrors = %v, want empty", got.LoadErrors)
	}
}

func TestListActions_SurfacesLoadErrors(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"good.md": actionsTestManifest,
		"bad.md":  actionsTestBadManifest,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/actions", nil)
	rec := httptest.NewRecorder()
	srv.ListActions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got api.ActionListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items == nil || len(*got.Items) != 1 {
		t.Errorf("Items count = %v, want 1 (the valid manifest)", got.Items)
	}
	if got.LoadErrors == nil || len(*got.LoadErrors) != 1 {
		t.Fatalf("LoadErrors = %v, want one entry", got.LoadErrors)
	}
	le := (*got.LoadErrors)[0]
	if le.File == "" {
		t.Errorf("LoadError.File is empty")
	}
	if le.Class == "" {
		t.Errorf("LoadError.Class is empty")
	}
}

func TestListActions_NilStoreReturnsEmpty(t *testing.T) {
	srv := &apiServer{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/v1/actions", nil)
	rec := httptest.NewRecorder()
	srv.ListActions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got api.ActionListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items != nil && len(*got.Items) != 0 {
		t.Errorf("Items = %v, want nil/empty", got.Items)
	}
}

func TestGetAction_ReturnsManifest(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/actions/ship-update", nil)
	rec := httptest.NewRecorder()
	srv.GetAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.Action
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "ship-update" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Body == nil {
		t.Fatal("Body is nil")
	}
	if got.Path == nil || *got.Path == "" {
		t.Errorf("Path is empty")
	}
	if len(got.Execute) != 1 {
		t.Errorf("Execute = %v, want 1 step", got.Execute)
	}
}

func TestGetAction_NotFound(t *testing.T) {
	srv := newActionsTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/actions/missing", nil)
	rec := httptest.NewRecorder()
	srv.GetAction(rec, req, "missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetAction_NilStoreReturns404(t *testing.T) {
	srv := &apiServer{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/v1/actions/x", nil)
	rec := httptest.NewRecorder()
	srv.GetAction(rec, req, "x")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestLoadErrorToAPI_PopulatesBoundaryAndLine(t *testing.T) {
	got := loadErrorToAPI(&action.Error{
		Class:    action.ClassParseError,
		Message:  "boom",
		File:     "x.md",
		Line:     42,
		Boundary: action.BoundaryAction,
	})
	if got.Line == nil || *got.Line != 42 {
		t.Errorf("Line = %v, want *42", got.Line)
	}
	if got.Boundary == nil || *got.Boundary != string(action.BoundaryAction) {
		t.Errorf("Boundary = %v, want *action", got.Boundary)
	}
}

func TestLoadErrorToAPI_OmitsBlankBoundaryAndZeroLine(t *testing.T) {
	got := loadErrorToAPI(&action.Error{
		Class:   action.ClassParseError,
		Message: "boom",
		File:    "x.md",
		// Boundary intentionally empty; Line zero.
	})
	if got.Boundary != nil {
		t.Errorf("Boundary = %v, want nil when blank", got.Boundary)
	}
	if got.Line != nil {
		t.Errorf("Line = %v, want nil when zero", got.Line)
	}
	if got.Class != string(action.ClassParseError) {
		t.Errorf("Class = %q", got.Class)
	}
}

// TestManifestToAPI_SurfacesApprovalPreviewPolicy verifies the
// listing endpoint's wire shape carries the action manifest's
// [approval.preview] directive (ADR-0016) verbatim. Without this, the
// /v1/actions catalog would not reflect that an action declares a
// preview op, so clients (the webapp's action detail page) could not
// surface the preview-aware behavior to users browsing installed
// actions.
func TestManifestToAPI_SurfacesApprovalPreviewPolicy(t *testing.T) {
	la := action.LoadedAction{
		Manifest: &action.Manifest{
			Name:    "send-draft",
			Version: "1.0.0",
			Source:  "hub://aileron/send-draft@1.0.0",
			Match:   action.Match{Intent: "send a draft"},
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "github://aileron/google", Version: "1.0.0", Hash: "sha256:a", Capabilities: []string{"send_draft"}},
			}},
			Inputs:  []action.Input{{Name: "draft_id", Type: "string", Description: "Draft id"}},
			Execute: []action.ExecuteStep{{ID: "s", Connector: "github://aileron/google", Op: "send_draft"}},
			Approval: &action.ApprovalPolicy{
				Required: true,
				Preview: &action.PreviewPolicy{
					Op:   "get_draft",
					Args: map[string]any{"id": "${args.draft_id}"},
					Render: map[string]string{
						"To":      "message.payload.headers.To",
						"Subject": "message.payload.headers.Subject",
					},
				},
			},
		},
	}
	got := manifestToAPI(la, true)
	if got.Approval == nil || got.Approval.Required == nil || !*got.Approval.Required {
		t.Fatalf("Approval.Required = %+v, want true", got.Approval)
	}
	if got.Approval.Preview == nil {
		t.Fatal("Approval.Preview = nil, want populated for manifest declaring [approval.preview]")
	}
	if got.Approval.Preview.Op != "get_draft" {
		t.Errorf("Approval.Preview.Op = %q, want %q", got.Approval.Preview.Op, "get_draft")
	}
	if got.Approval.Preview.Args == nil {
		t.Fatal("Approval.Preview.Args = nil, want populated")
	}
	if (*got.Approval.Preview.Args)["id"] != "${args.draft_id}" {
		t.Errorf("Args[id] = %v, want ${args.draft_id}", (*got.Approval.Preview.Args)["id"])
	}
	if got.Approval.Preview.Render["To"] != "message.payload.headers.To" {
		t.Errorf("Render[To] = %q, want path", got.Approval.Preview.Render["To"])
	}
}

func TestManifestToAPI_OmitsBlankBodyAndPath(t *testing.T) {
	la := action.LoadedAction{
		Manifest: &action.Manifest{
			Name:    "x",
			Version: "1.0.0",
			Source:  "hub://aileron/x@1.0.0",
			Match:   action.Match{Intent: "hi"},
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "github://aileron/slack", Version: "1.0.0", Hash: "sha256:a", Capabilities: []string{"chat:write"}},
			}},
			Execute: []action.ExecuteStep{{ID: "s", Connector: "github://aileron/slack", Op: "post"}},
			// Body intentionally blank.
		},
		// Path intentionally blank.
	}
	got := manifestToAPI(la, true)
	if got.Body != nil {
		t.Errorf("Body = %v, want nil when blank", got.Body)
	}
	if got.Path != nil {
		t.Errorf("Path = %v, want nil when blank", got.Path)
	}
}

func TestRunAction_HappyPath(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	body := bytes.NewReader([]byte(`{"args":{"channel":"#engineering"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionRunResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AuditId == "" {
		t.Error("expected non-empty audit_id")
	}
	if got.Result == nil || *got.Result == "" {
		t.Error("expected non-empty result content from stub executor")
	}
}

func TestRunAction_NotFound(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/missing/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunAction_VaultLocked(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.vaultLocked = true

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunAction_InvalidJSON(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	body := bytes.NewReader([]byte(`{not-json`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// failingExecutor is a test stub that returns a structured ADR-0010
// failure for the action-side error path (not a Go error).
type failingExecutor struct{ failure *failure.Failure }

func (f failingExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	return action.Result{Failure: f.failure}, nil
}

// erroringExecutor is a test stub that returns a Go error to exercise
// the gateway-fatal branch in RunAction.
type erroringExecutor struct{}

func (erroringExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	return action.Result{}, errors.New("executor blew up")
}

func TestRunAction_ActionSideFailure(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = failingExecutor{failure: failure.BindingRequiredFailure(
		"slack binding missing",
	)}

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	// ADR-0010: binding_required maps to 412 Precondition Failed.
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"binding_required"`)) {
		t.Errorf("expected envelope class in body; got %s", rec.Body.String())
	}
}

func TestRunAction_ExecutorError(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = erroringExecutor{}

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// actionsTestManifestApproval is `ship-update` annotated with
// `[approval] required = true`, used to exercise the runtime's
// approval gate.
const actionsTestManifestApproval = `+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123"
capabilities = ["chat:write"]

[match]
intent = "tell team I shipped"

[[execute]]
id = "post"
connector = "github://aileron/slack"
op = "post_message"

[approval]
required = true
+++

# Ship Update
`

// TestRunAction_ApprovalRequired_ApprovedReturnsResult is the load-bearing
// TestRunAction_ApprovalRequired_Returns202Immediately pins the new
// async contract for the approval-gated path: RunAction returns 202
// the moment the queue registers the entry, with a pending-approval
// body carrying the approval id, review URL, and instruction the
// agent should surface to the user. The action does not execute
// inline; the background goroutine handles it after the user decides.
func TestRunAction_ApprovalRequired_Returns202Immediately(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifestApproval,
	})
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	srv.webappURL = "http://127.0.0.1:54321"

	body := bytes.NewReader([]byte(`{"args":{"channel":"#engineering"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()

	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	for _, want := range []string{`"status":"pending_approval"`, `"approval_id":`, `"review_url":`, `"message":`} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q; got %s", want, got)
		}
	}
	if !strings.Contains(got, "http://127.0.0.1:54321/approvals?focus=") {
		t.Errorf("review_url should embed per-approval focus link; got %s", got)
	}
	if !strings.Contains(got, "aileron open approval ") {
		t.Errorf("message should include the `aileron open approval <id>` hint; got %s", got)
	}

	// One entry registered; it stays pending until decided.
	pending := srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(pending))
	}
}

// TestRunAction_ApprovalRequired_BackgroundExecutorRunsOnApprove
// verifies the load-bearing async path: after RunAction returns 202,
// a Decide(approved=true) drives the background goroutine to execute
// the action; the eventual outcome lands on the queue with status =
// completed and the action's result attached, polled via Outcome().
func TestRunAction_ApprovalRequired_BackgroundExecutorRunsOnApprove(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifestApproval,
	})
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	tracker := &countingExecutor{}
	srv.executor = tracker

	body := bytes.NewReader([]byte(`{"args":{"channel":"#engineering"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	pending := srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(pending))
	}
	approvalID := pending[0].ID

	if err := srv.actionApprovals.Decide(approvalID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Wait for the background goroutine to record completion.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if o, ok := srv.actionApprovals.Outcome(approvalID); ok && o.Status == approval.OutcomeCompleted {
			if tracker.calls != 1 {
				t.Errorf("executor calls = %d, want 1", tracker.calls)
			}
			if o.Result == "" {
				t.Errorf("Outcome.Result should carry the executor's content; got empty")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background executor never recorded completion; executor calls=%d", tracker.calls)
}

// TestRunAction_ApprovalRequired_DenyLeavesOutcomeDeniedAndSkipsExecutor:
// on Decide(approved=false), the background goroutine returns without
// invoking the executor; the queue's outcome is denied with the user's
// reason attached. The agent learns the outcome via Outcome().
func TestRunAction_ApprovalRequired_DenyLeavesOutcomeDeniedAndSkipsExecutor(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifestApproval,
	})
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	tracker := &countingExecutor{}
	srv.executor = tracker

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	pending := srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(pending))
	}
	approvalID := pending[0].ID
	if err := srv.actionApprovals.Decide(approvalID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Give the background goroutine a tick to observe the denial.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if o, ok := srv.actionApprovals.Outcome(approvalID); ok && o.Status == approval.OutcomeDenied {
			if o.DenyReason != "wrong recipient" {
				t.Errorf("Outcome.DenyReason = %q, want %q", o.DenyReason, "wrong recipient")
			}
			if tracker.calls != 0 {
				t.Errorf("executor was invoked %d times on deny; want 0", tracker.calls)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("outcome never reached denied")
}

// countingExecutor counts how many times Execute was called. Used to
// assert the deny path never reaches the executor.
type countingExecutor struct {
	calls int
}

func (c *countingExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	c.calls++
	return action.Result{Content: `{"action":"ship-update"}`}, nil
}

// previewingExecutor is a counting executor that also implements
// action.PreviewInvoker so tests can drive the [approval.preview]
// (ADR-0016) plumbing through RunAction without spinning up a real
// SandboxExecutor. The script field returns whatever PreviewResult
// the test wants; the call count exposes whether the handler invoked
// the preview hook at all.
type previewingExecutor struct {
	countingExecutor
	previewCalls   int
	previewArgs    map[string]any
	previewResult  action.PreviewResult
	previewSkipped bool // when true, InvokePreview is not implemented (tested by NOT embedding this method)
}

func (p *previewingExecutor) InvokePreview(_ context.Context, _ *action.Manifest, args map[string]any) action.PreviewResult {
	p.previewCalls++
	p.previewArgs = args
	return p.previewResult
}

// actionsTestManifestApprovalPreview is `send-draft` annotated with
// `[approval] required = true` AND an `[approval.preview]` directive
// (ADR-0016). Exercises the runtime's preview plumbing through
// RunAction: the handler must call InvokePreview before registering
// the approval entry and attach the rendered output to the queue.
const actionsTestManifestApprovalPreview = `+++
name = "send-draft"
version = "1.0.0"
source = "hub://aileron/send-draft@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc123"
capabilities = ["send_draft", "get_draft"]

[match]
intent = "send a draft"

[[inputs]]
name = "draft_id"
type = "string"
description = "Draft id"

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_draft"

[approval]
required = true

[approval.preview]
op = "get_draft"
args = { id = "${args.draft_id}" }

[approval.preview.render]
To = "message.payload.headers.To"
Subject = "message.payload.headers.Subject"
Preview = "message.snippet"
+++

# Send Draft
`

// TestRunAction_ApprovalPreview_AttachesRenderedFields verifies the
// load-bearing ADR-0016 wire: when the action manifest declares
// [approval.preview] and the executor implements PreviewInvoker, the
// handler invokes the preview ahead of Register and the queue entry
// surfaces the rendered fields. The 202 response is unchanged; the
// preview is consumed by the webapp's pending-approval payload.
func TestRunAction_ApprovalPreview_AttachesRenderedFields(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"send-draft.md": actionsTestManifestApprovalPreview,
	})
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	tracker := &previewingExecutor{
		previewResult: action.PreviewResult{
			Fields: []action.PreviewField{
				{Label: "To", Value: "alice@example.com"},
				{Label: "Subject", Value: "Weekly recap"},
				{Label: "Preview", Value: "Here's the recap..."},
			},
		},
	}
	srv.executor = tracker

	body := bytes.NewReader([]byte(`{"args":{"draft_id":"r-12345"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/send-draft/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "send-draft")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if tracker.previewCalls != 1 {
		t.Fatalf("InvokePreview calls = %d, want 1", tracker.previewCalls)
	}
	if tracker.previewArgs["draft_id"] != "r-12345" {
		t.Errorf("InvokePreview args[draft_id] = %v, want r-12345", tracker.previewArgs["draft_id"])
	}

	pending := srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(pending))
	}
	entry := pending[0]
	if entry.Preview == nil {
		t.Fatal("entry.Preview = nil, want populated preview")
	}
	if entry.Preview.Unavailable != "" {
		t.Errorf("Preview.Unavailable = %q, want empty", entry.Preview.Unavailable)
	}
	if len(entry.Preview.Fields) != 3 {
		t.Fatalf("Preview.Fields length = %d, want 3", len(entry.Preview.Fields))
	}
	if entry.Preview.Fields[0].Label != "To" || entry.Preview.Fields[0].Value != "alice@example.com" {
		t.Errorf("Preview.Fields[0] = %+v, want {To, alice@example.com}", entry.Preview.Fields[0])
	}
}

// TestRunAction_ApprovalPreview_UnavailableSurfacedAsFallback covers
// ADR-0016's failure-mode contract: when the preview op fails
// wholesale, the runtime attaches an Unavailable reason to the queue
// entry so the approval prompt renders "Preview unavailable: <reason>"
// alongside the raw inputs. The approval still proceeds; the user
// can decline.
func TestRunAction_ApprovalPreview_UnavailableSurfacedAsFallback(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"send-draft.md": actionsTestManifestApprovalPreview,
	})
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	tracker := &previewingExecutor{
		previewResult: action.PreviewResult{Unavailable: "preview unavailable: Gmail API returned 404"},
	}
	srv.executor = tracker

	body := bytes.NewReader([]byte(`{"args":{"draft_id":"wrong"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/send-draft/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "send-draft")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (approval still proceeds on preview failure)", rec.Code)
	}
	pending := srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(pending))
	}
	entry := pending[0]
	if entry.Preview == nil {
		t.Fatal("entry.Preview = nil, want populated with Unavailable reason")
	}
	if entry.Preview.Unavailable != "preview unavailable: Gmail API returned 404" {
		t.Errorf("Preview.Unavailable = %q, want the 404 reason", entry.Preview.Unavailable)
	}
	if len(entry.Preview.Fields) != 0 {
		t.Errorf("Preview.Fields = %+v, want empty on wholesale failure", entry.Preview.Fields)
	}
}

// TestRunAction_ApprovalPreview_NotInvokedWhenManifestOmitsBlock
// verifies the runtime does not invoke InvokePreview when the action
// manifest carries no [approval.preview] directive. Without this guard,
// every approval-required action would pay the cost of a preview call
// even when the author did not declare one.
func TestRunAction_ApprovalPreview_NotInvokedWhenManifestOmitsBlock(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifestApproval,
	})
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	tracker := &previewingExecutor{}
	srv.executor = tracker

	body := bytes.NewReader([]byte(`{"args":{"channel":"#eng"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if tracker.previewCalls != 0 {
		t.Errorf("InvokePreview called %d times for manifest without [approval.preview]; want 0", tracker.previewCalls)
	}
	pending := srv.actionApprovals.List()
	if len(pending) != 1 || pending[0].Preview != nil {
		t.Errorf("entry.Preview should be nil when manifest omits the block; got %+v", pending[0].Preview)
	}
}

// --- Audit emission on action-via-MCP path (U8 / #953) ---

// audit-store helpers shared across U8 tests.

// listEventsByType returns events of the given type in chronological
// (insertion) order. The MemStore's ListEvents is newest-first; we
// reverse so test assertions read top-down as the event lifecycle ran.
func listEventsByType(t *testing.T, store *audit.MemStore, eventType model.EventType) []audit.Event {
	t.Helper()
	all, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var out []audit.Event
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].EventType == eventType {
			out = append(out, all[i])
		}
	}
	return out
}

// eventTypesForSession returns the chronological list of event types
// stamped with the given session id.
func eventTypesForSession(t *testing.T, store *audit.MemStore, sessionID string) []model.EventType {
	t.Helper()
	all, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var out []model.EventType
	for i := len(all) - 1; i >= 0; i-- {
		if got, _ := all[i].Payload["aileron.session.id"].(string); got == sessionID {
			out = append(out, all[i].EventType)
		}
	}
	return out
}

// assertApprovedExecutionChain verifies the partial order that the system
// actually guarantees for an approved action's audit chain.
//
// The chain is written by two goroutines with no happens-before on their
// audit-store writes: ActionApprovalQueue.Decide emits approval.approved,
// and a broadcast subscriber launches executeApprovedAction, which emits
// execution.started then execution.succeeded. Approval causally gates
// execution, but the two goroutines' audit-store writes have no ordering
// guarantee relative to each other, so approval.approved may appear anywhere
// after approval.requested, including after execution.succeeded. Asserting a
// fixed [requested, approved, started, succeeded] sequence is therefore flaky.
//
// The contract this enforces:
//   - the four event types present are exactly {approval.requested,
//     approval.approved, execution.started, execution.succeeded};
//   - approval.requested is first;
//   - execution.started appears before execution.succeeded (by index);
//   - approval.approved is present and appears after approval.requested.
//
// It deliberately makes no assertion about approval.approved's position
// relative to the execution events. The optional label prefixes failure
// messages (e.g. "approval \"id\" ") when several chains are checked in a loop.
func assertApprovedExecutionChain(t *testing.T, label string, chain []model.EventType) {
	t.Helper()

	want := map[model.EventType]bool{
		model.EventTypeApprovalRequested:  true,
		model.EventTypeApprovalApproved:   true,
		model.EventTypeExecutionStarted:   true,
		model.EventTypeExecutionSucceeded: true,
	}
	if len(chain) != len(want) {
		t.Fatalf("%schain = %v; want 4 events {approval.requested, approval.approved, execution.started, execution.succeeded}", label, chain)
	}

	idx := make(map[model.EventType]int, len(chain))
	for i, ev := range chain {
		if !want[ev] {
			t.Errorf("%schain has unexpected event %s; chain = %v", label, ev, chain)
		}
		if _, dup := idx[ev]; dup {
			t.Errorf("%schain has duplicate event %s; chain = %v", label, ev, chain)
		}
		idx[ev] = i
	}
	for ev := range want {
		if _, ok := idx[ev]; !ok {
			t.Errorf("%schain is missing event %s; chain = %v", label, ev, chain)
		}
	}
	if t.Failed() {
		return
	}

	if chain[0] != model.EventTypeApprovalRequested {
		t.Errorf("%schain[0] = %s; want %s first", label, chain[0], model.EventTypeApprovalRequested)
	}
	if idx[model.EventTypeExecutionStarted] >= idx[model.EventTypeExecutionSucceeded] {
		t.Errorf("%sexecution.started (idx %d) must precede execution.succeeded (idx %d); chain = %v",
			label, idx[model.EventTypeExecutionStarted], idx[model.EventTypeExecutionSucceeded], chain)
	}
	if idx[model.EventTypeApprovalApproved] <= idx[model.EventTypeApprovalRequested] {
		t.Errorf("%sapproval.approved (idx %d) must follow approval.requested (idx %d); chain = %v",
			label, idx[model.EventTypeApprovalApproved], idx[model.EventTypeApprovalRequested], chain)
	}
}

// TestAssertApprovedExecutionChain pins the contract of the partial-order
// assertion shared by the approval round-trip tests. It is the regression
// guard for feedback #1168: the helper must accept the observed flaky order
// (approval.approved persisted after execution.succeeded) while still
// rejecting chains that violate the order the system genuinely guarantees.
func TestAssertApprovedExecutionChain(t *testing.T) {
	cases := []struct {
		name    string
		chain   []model.EventType
		wantErr bool
	}{
		{
			name: "canonical order",
			chain: []model.EventType{
				model.EventTypeApprovalRequested,
				model.EventTypeApprovalApproved,
				model.EventTypeExecutionStarted,
				model.EventTypeExecutionSucceeded,
			},
		},
		{
			// The exact interleaving observed flaking on the CI gate: the
			// background executor's events land before the Decide path's
			// approval.approved write. This MUST pass.
			name: "approved persisted after succeeded",
			chain: []model.EventType{
				model.EventTypeApprovalRequested,
				model.EventTypeExecutionStarted,
				model.EventTypeExecutionSucceeded,
				model.EventTypeApprovalApproved,
			},
		},
		{
			name: "approved persisted between execution events",
			chain: []model.EventType{
				model.EventTypeApprovalRequested,
				model.EventTypeExecutionStarted,
				model.EventTypeApprovalApproved,
				model.EventTypeExecutionSucceeded,
			},
		},
		{
			name: "missing approval.approved",
			chain: []model.EventType{
				model.EventTypeApprovalRequested,
				model.EventTypeExecutionStarted,
				model.EventTypeExecutionSucceeded,
			},
			wantErr: true,
		},
		{
			name: "missing execution.started",
			chain: []model.EventType{
				model.EventTypeApprovalRequested,
				model.EventTypeApprovalApproved,
				model.EventTypeExecutionSucceeded,
			},
			wantErr: true,
		},
		{
			name: "succeeded before started",
			chain: []model.EventType{
				model.EventTypeApprovalRequested,
				model.EventTypeApprovalApproved,
				model.EventTypeExecutionSucceeded,
				model.EventTypeExecutionStarted,
			},
			wantErr: true,
		},
		{
			name: "approval.requested not first",
			chain: []model.EventType{
				model.EventTypeApprovalApproved,
				model.EventTypeApprovalRequested,
				model.EventTypeExecutionStarted,
				model.EventTypeExecutionSucceeded,
			},
			wantErr: true,
		},
		{
			name: "unexpected event present",
			chain: []model.EventType{
				model.EventTypeApprovalRequested,
				model.EventTypeApprovalApproved,
				model.EventTypeExecutionStarted,
				model.EventTypeApprovalDenied,
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Run the assertion against a throwaway *testing.T in its own
			// goroutine: the helper's t.Fatalf path calls runtime.Goexit,
			// which would otherwise abort this subtest before we can read
			// the result. The closure returns only after Goexit (via the
			// deferred close), so fake.Failed() is safe to read afterward.
			fake := &testing.T{}
			done := make(chan struct{})
			go func() {
				defer close(done)
				assertApprovedExecutionChain(fake, "", tc.chain)
			}()
			<-done
			if got := fake.Failed(); got != tc.wantErr {
				t.Errorf("assertApprovedExecutionChain(%v) failed=%v; want failed=%v", tc.chain, got, tc.wantErr)
			}
		})
	}
}

// TestRunAction_NoApproval_EmitsExecutionStartedAndSucceeded covers
// R6 / R7 for the synchronous path: a non-approval action invoked via
// /v1/actions/{name}/run lands an execution.started → execution.succeeded
// pair stamped with the X-Aileron-Session-Id from the request.
//
// Regression for U8: before the fix this code path had `_ = sessionID`
// and emitted zero events. The U5 E2E test's exclusive-event-chain
// assertion is unimplementable without this fix.
func TestRunAction_NoApproval_EmitsExecutionStartedAndSucceeded(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	store := audit.NewMemStore()
	srv.auditRecorder = audit.NewRecorder(store, nil, nil)

	body := bytes.NewReader([]byte(`{"args":{"channel":"#engineering"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	req.Header.Set("X-Aileron-Session-Id", "sess-u8-1")
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	chain := eventTypesForSession(t, store, "sess-u8-1")
	want := []model.EventType{
		model.EventTypeExecutionStarted,
		model.EventTypeExecutionSucceeded,
	}
	if len(chain) != len(want) {
		t.Fatalf("event chain = %v; want %v", chain, want)
	}
	for i, w := range want {
		if chain[i] != w {
			t.Errorf("chain[%d] = %s; want %s", i, chain[i], w)
		}
	}

	// Verify the started event carries the action FQN.
	started := listEventsByType(t, store, model.EventTypeExecutionStarted)
	if len(started) != 1 {
		t.Fatalf("started events = %d, want 1", len(started))
	}
	if got, _ := started[0].Payload["aileron.action.name"].(string); got != "ship-update" {
		t.Errorf("started.action.name = %q, want ship-update", got)
	}
}

// TestRunAction_NoApproval_OmitsSessionIDWhenAbsent: when the request
// has no X-Aileron-Session-Id header (e.g., a CLI invocation or a
// pre-MCP-fix client), events still emit but carry no session id field.
// Regression so the empty-string isn't injected.
func TestRunAction_NoApproval_OmitsSessionIDWhenAbsent(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	store := audit.NewMemStore()
	srv.auditRecorder = audit.NewRecorder(store, nil, nil)

	body := bytes.NewReader([]byte(`{"args":{"channel":"#x"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	// no X-Aileron-Session-Id header
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, e := range events {
		if _, present := e.Payload["aileron.session.id"]; present {
			t.Errorf("event %s carried session.id when header was absent", e.EventType)
		}
	}
}

// TestRunAction_NoApproval_ExecutorError_EmitsExecutionFailed pins the
// Go-error path: when the executor returns a Go error (not an ADR-0010
// envelope), the audit chain is execution.started → execution.failed.
func TestRunAction_NoApproval_ExecutorError_EmitsExecutionFailed(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = erroringExecutor{}
	store := audit.NewMemStore()
	srv.auditRecorder = audit.NewRecorder(store, nil, nil)

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	req.Header.Set("X-Aileron-Session-Id", "sess-u8-err")
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	chain := eventTypesForSession(t, store, "sess-u8-err")
	want := []model.EventType{
		model.EventTypeExecutionStarted,
		model.EventTypeExecutionFailed,
	}
	if len(chain) != len(want) || chain[0] != want[0] || chain[1] != want[1] {
		t.Fatalf("event chain = %v; want %v", chain, want)
	}
	// Executor-error path carries the raw error text.
	failed := listEventsByType(t, store, model.EventTypeExecutionFailed)
	if got, _ := failed[0].Payload["aileron.executor.error"].(string); got == "" {
		t.Errorf("failed event missing executor.error payload; got %+v", failed[0].Payload)
	}
}

// TestRunAction_NoApproval_ActionSideFailure_EmitsExecutionFailed
// pins the ADR-0010 envelope path: when the executor returns a
// structured failure, the audit chain still ends in execution.failed
// and the payload names the failure class.
func TestRunAction_NoApproval_ActionSideFailure_EmitsExecutionFailed(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = failingExecutor{failure: failure.BindingRequiredFailure("slack binding missing")}
	store := audit.NewMemStore()
	srv.auditRecorder = audit.NewRecorder(store, nil, nil)

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	req.Header.Set("X-Aileron-Session-Id", "sess-u8-bind")
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
	chain := eventTypesForSession(t, store, "sess-u8-bind")
	if len(chain) < 2 || chain[len(chain)-1] != model.EventTypeExecutionFailed {
		t.Fatalf("event chain = %v; want last entry execution.failed", chain)
	}
	failed := listEventsByType(t, store, model.EventTypeExecutionFailed)
	if got, _ := failed[0].Payload["aileron.failure.class"].(string); got != "binding_required" {
		t.Errorf("failed event class = %q; want binding_required", got)
	}
}

// TestRunAction_Approval_FullChainEmitsApprovalAndExecutionEvents
// covers R6's load-bearing claim: an approval-gated action invoked via
// MCP produces the chain approval.requested → approval.approved →
// execution.started → execution.succeeded with the session id
// preserved across all four events.
func TestRunAction_Approval_FullChainEmitsApprovalAndExecutionEvents(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifestApproval,
	})
	store := audit.NewMemStore()
	srv.auditRecorder = audit.NewRecorder(store, nil, nil)
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	srv.actionApprovals.SetAuditRecorder(srv.auditRecorder)

	body := bytes.NewReader([]byte(`{"args":{"channel":"#x"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	req.Header.Set("X-Aileron-Session-Id", "sess-u8-approval")
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	// Decide approved.
	pending := srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	approvalID := pending[0].ID
	if err := srv.actionApprovals.Decide(approvalID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Background goroutine in executeApprovedAction emits execution
	// events asynchronously. Poll briefly for the terminal event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evs := listEventsByType(t, store, model.EventTypeExecutionSucceeded)
		if len(evs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// approval.approved (Decide path) and the execution events (background
	// executor goroutine) are written to the audit store by two goroutines
	// with no ordering guarantee, so approval.approved can land anywhere after
	// approval.requested — including after execution.succeeded. Assert the
	// partial order the system guarantees rather than a fixed sequence.
	chain := eventTypesForSession(t, store, "sess-u8-approval")
	assertApprovedExecutionChain(t, "", chain)
}

// TestRunAction_Approval_Denied_OmitsExecutionEvents pins R6a's deny
// path: denying an approval emits approval.requested →
// approval.denied with no execution.* events.
func TestRunAction_Approval_Denied_OmitsExecutionEvents(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifestApproval,
	})
	store := audit.NewMemStore()
	srv.auditRecorder = audit.NewRecorder(store, nil, nil)
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	srv.actionApprovals.SetAuditRecorder(srv.auditRecorder)

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	req.Header.Set("X-Aileron-Session-Id", "sess-u8-deny")
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	pending := srv.actionApprovals.List()
	if err := srv.actionApprovals.Decide(pending[0].ID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Give the background goroutine a brief moment to exit cleanly.
	time.Sleep(50 * time.Millisecond)

	chain := eventTypesForSession(t, store, "sess-u8-deny")
	for _, evt := range chain {
		if strings.HasPrefix(string(evt), "execution.") {
			t.Errorf("denied path emitted %s; want no execution.* events", evt)
		}
	}
	// Sanity: still see the approval lifecycle.
	if len(chain) < 2 {
		t.Fatalf("expected at least approval.requested + approval.denied; got %v", chain)
	}
}

func TestRunAction_NilExecutor(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = nil

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
