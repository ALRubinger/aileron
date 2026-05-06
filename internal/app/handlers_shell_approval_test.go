package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
)

// /v1/sessions/{id}/approvals/shell contract:
//
//   - 200 with the user's decision string when the user clicks
//     Approve or Deny on the webapp /approvals page.
//   - 200 with `deny` when the action-approval TTL fires before
//     the user decides — the policy-enforced shell fails closed.
//   - 200 maps the EditedPayload[save_policy] key to the four-option
//     vocabulary aileron-sh understands.
//   - 400 when the request body is malformed or the command is empty.
//   - 503 when the daemon was not configured with an approval queue.

func newShellApprovalServer(t *testing.T, ttl time.Duration) *apiServer {
	t.Helper()
	q := approval.NewActionApprovalQueue(nil, nil)
	return &apiServer{
		log:               slog.Default(),
		actionApprovals:   q,
		actionApprovalTTL: ttl,
	}
}

func TestRequestShellApproval_AllowOnce(t *testing.T) {
	s := newShellApprovalServer(t, 5*time.Second)

	// Pre-decide: register a goroutine that approves the next pending
	// shell approval as soon as it appears.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pending := s.actionApprovals.List()
			for _, p := range pending {
				if p.Kind == approval.ApprovalKindShell {
					_ = s.actionApprovals.Decide(p.ID, true, "", nil)
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	body, _ := json.Marshal(api.ShellApprovalRequest{Command: "git push"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/approvals/shell", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestShellApproval(w, req, "sess-1")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp api.ShellApprovalResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision != api.AllowOnce {
		t.Errorf("decision = %q, want allow_once", resp.Decision)
	}
}

func TestRequestShellApproval_AllowProject(t *testing.T) {
	s := newShellApprovalServer(t, 5*time.Second)

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pending := s.actionApprovals.List()
			for _, p := range pending {
				if p.Kind == approval.ApprovalKindShell {
					_ = s.actionApprovals.Decide(p.ID, true, "", map[string]any{
						"save_policy": "project",
					})
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	body, _ := json.Marshal(api.ShellApprovalRequest{Command: "git push"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-2/approvals/shell", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestShellApproval(w, req, "sess-2")

	var resp api.ShellApprovalResponse
	mustDecode(t, w.Body, &resp)
	if resp.Decision != api.AllowProject {
		t.Errorf("decision = %q, want allow_project", resp.Decision)
	}
}

func TestRequestShellApproval_Deny(t *testing.T) {
	s := newShellApprovalServer(t, 5*time.Second)

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pending := s.actionApprovals.List()
			for _, p := range pending {
				if p.Kind == approval.ApprovalKindShell {
					_ = s.actionApprovals.Decide(p.ID, false, "rejected", nil)
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	body, _ := json.Marshal(api.ShellApprovalRequest{Command: "rm -rf /"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-3/approvals/shell", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestShellApproval(w, req, "sess-3")

	var resp api.ShellApprovalResponse
	mustDecode(t, w.Body, &resp)
	if resp.Decision != api.Deny {
		t.Errorf("decision = %q, want deny", resp.Decision)
	}
}

func TestRequestShellApproval_TimeoutCollapsesToDeny(t *testing.T) {
	// 50ms TTL — no goroutine decides — TTL fires.
	s := newShellApprovalServer(t, 50*time.Millisecond)

	body, _ := json.Marshal(api.ShellApprovalRequest{Command: "ls"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-4/approvals/shell", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestShellApproval(w, req, "sess-4")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (timeout still returns the wire decision)", w.Code)
	}
	var resp api.ShellApprovalResponse
	mustDecode(t, w.Body, &resp)
	if resp.Decision != api.Deny {
		t.Errorf("timeout decision = %q, want deny", resp.Decision)
	}
}

func TestRequestShellApproval_MissingCommand400(t *testing.T) {
	s := newShellApprovalServer(t, 5*time.Second)
	body, _ := json.Marshal(api.ShellApprovalRequest{}) // empty command
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/approvals/shell", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestShellApproval(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRequestShellApproval_GarbageBody400(t *testing.T) {
	s := newShellApprovalServer(t, 5*time.Second)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/approvals/shell", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.RequestShellApproval(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRequestShellApproval_NoQueue503(t *testing.T) {
	s := &apiServer{log: slog.Default()} // no actionApprovals
	body, _ := json.Marshal(api.ShellApprovalRequest{Command: "ls"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/approvals/shell", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.RequestShellApproval(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// _ keeps sync.WaitGroup linked even though it's not used here —
// future tests that race multiple approvers will need it.
var _ = sync.WaitGroup{}
