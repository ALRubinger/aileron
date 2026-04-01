package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/approval"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/policy"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/google/uuid"
)

// --- Mock policy engine ---

type mockPolicyEngine struct {
	decision model.Decision
	err      error
}

func (m *mockPolicyEngine) Evaluate(_ context.Context, _ policy.EvaluationRequest) (model.Decision, error) {
	return m.decision, m.err
}

// --- Mock downstream client for routing ---

// testGateway creates a gateway with a mock policy engine, a real approval
// orchestrator, and pre-registered tool routes (no real subprocesses).
func testGateway(t *testing.T, pe policy.Engine) *gateway {
	t.Helper()
	approvalStore := mem.NewApprovalStore()
	idGen := func() string { return uuid.New().String() }
	orch := approval.NewInMemoryOrchestrator(approvalStore, idGen)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	return &gateway{
		routes:       make(map[string]toolRoute),
		policyEngine: pe,
		approvals:    orch,
		auditStore:   newMemAuditStore(),
		queue:        newCallQueue(),
		workspaceID:  "default",
		log:          log,
	}
}

// addFakeRoute adds a tool route without a real client. Calls that reach
// forwardToDownstream will fail, but policy interception tests don't need
// downstream forwarding to succeed.
func addFakeRoute(g *gateway, qualifiedName, serverName, originalName, toolPrefix string) {
	g.routes[qualifiedName] = toolRoute{
		client:       nil, // no real client — forwarding will error
		originalName: originalName,
		serverName:   serverName,
		toolPrefix:   toolPrefix,
	}
}

// --- Tests ---

func TestRouteToolCall_PolicyAllow(t *testing.T) {
	// Use a capturing engine to verify the allow path is taken and
	// audit events are emitted. We can't forward to a nil client,
	// so we verify via audit events that the "forwarded" event fires.
	var captured policy.EvaluationRequest
	pe := &capturingPolicyEngine{
		decision: model.Decision{
			Disposition: model.DispositionAllow,
			RiskLevel:   model.RiskLevelLow,
		},
		captured: &captured,
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "fs__read_file", "fs", "read_file", "filesystem")
	store := gw.auditStore.(*memAuditStore)

	ctx := context.Background()
	_ = gw.routeToolCall(ctx, "fs__read_file", map[string]any{"path": "/tmp/test"})

	// Verify policy was evaluated.
	if captured.Action.Type != "filesystem.read_file" {
		t.Errorf("Action.Type = %q, want %q", captured.Action.Type, "filesystem.read_file")
	}

	// Verify audit events: intercepted + forwarded.
	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.events) < 2 {
		t.Fatalf("expected at least 2 audit events, got %d", len(store.events))
	}
	if store.events[0].EventType != model.EventTypeToolCallIntercepted {
		t.Errorf("event[0] = %q, want intercepted", store.events[0].EventType)
	}
	if store.events[1].EventType != model.EventTypeToolCallForwarded {
		t.Errorf("event[1] = %q, want forwarded", store.events[1].EventType)
	}
}

func TestRouteToolCall_PolicyDeny(t *testing.T) {
	pe := &mockPolicyEngine{
		decision: model.Decision{
			Disposition:  model.DispositionDeny,
			RiskLevel:    model.RiskLevelCritical,
			DenialReason: "destructive operation not allowed",
		},
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "github__delete_repo", "github", "delete_repo", "git")

	ctx := context.Background()
	result := gw.routeToolCall(ctx, "github__delete_repo", map[string]any{"repo": "acme/checkout"})

	if !result.IsError {
		t.Error("expected error for denied tool call")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "destructive operation not allowed") {
		t.Errorf("expected denial reason in text, got: %s", text)
	}
}

func TestRouteToolCall_PolicyRequireApproval(t *testing.T) {
	pe := &mockPolicyEngine{
		decision: model.Decision{
			Disposition: model.DispositionRequireApproval,
			RiskLevel:   model.RiskLevelMedium,
		},
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "github__create_pr", "github", "create_pr", "git")

	ctx := context.Background()
	result := gw.routeToolCall(ctx, "github__create_pr", map[string]any{
		"base": "main",
		"head": "fix/tax",
	})

	if result.IsError {
		t.Errorf("expected no error for pending approval, got: %v", result.Content)
	}

	// Parse the JSON result.
	var resp map[string]any
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("failed to parse result JSON: %v", err)
	}

	if resp["status"] != "pending_approval" {
		t.Errorf("status = %v, want pending_approval", resp["status"])
	}
	approvalID, ok := resp["approval_id"].(string)
	if !ok || approvalID == "" {
		t.Fatal("expected non-empty approval_id")
	}

	// Verify the call was queued.
	call, found := gw.queue.peek(approvalID)
	if !found {
		t.Fatal("expected call to be queued")
	}
	if call.QualifiedName != "github__create_pr" {
		t.Errorf("queued tool = %q, want %q", call.QualifiedName, "github__create_pr")
	}
	if call.Arguments["base"] != "main" {
		t.Errorf("queued argument base = %v, want %q", call.Arguments["base"], "main")
	}
}

func TestRouteToolCall_PolicyEngineError(t *testing.T) {
	pe := &mockPolicyEngine{
		err: context.DeadlineExceeded,
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "slack__send_message", "slack", "send_message", "")

	ctx := context.Background()
	result := gw.routeToolCall(ctx, "slack__send_message", map[string]any{})

	if !result.IsError {
		t.Error("expected error when policy engine fails")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "Policy evaluation failed") {
		t.Errorf("expected policy error message, got: %s", text)
	}
}

func TestCheckApproval_NotQueued(t *testing.T) {
	gw := testGateway(t, &mockPolicyEngine{})

	ctx := context.Background()
	result := gw.handleCheckApproval(ctx, map[string]any{
		"approval_id": "apr_nonexistent",
	})

	if result.IsError {
		t.Error("expected no error for not-found approval")
	}

	var resp map[string]any
	json.Unmarshal([]byte(result.Content[0].Text), &resp)
	if resp["status"] != "not_found" {
		t.Errorf("status = %v, want not_found", resp["status"])
	}
}

func TestCheckApproval_PendingApproval(t *testing.T) {
	pe := &mockPolicyEngine{
		decision: model.Decision{
			Disposition: model.DispositionRequireApproval,
			RiskLevel:   model.RiskLevelMedium,
		},
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "github__create_pr", "github", "create_pr", "git")

	ctx := context.Background()

	// First, trigger the approval hold.
	holdResult := gw.routeToolCall(ctx, "github__create_pr", map[string]any{"base": "main"})
	var holdResp map[string]any
	json.Unmarshal([]byte(holdResult.Content[0].Text), &holdResp)
	approvalID := holdResp["approval_id"].(string)

	// Now check — should be pending.
	checkResult := gw.handleCheckApproval(ctx, map[string]any{
		"approval_id": approvalID,
	})

	var checkResp map[string]any
	json.Unmarshal([]byte(checkResult.Content[0].Text), &checkResp)
	if checkResp["status"] != "pending" {
		t.Errorf("status = %v, want pending", checkResp["status"])
	}
}

func TestCheckApproval_ApprovedAutoExecutes(t *testing.T) {
	pe := &mockPolicyEngine{
		decision: model.Decision{
			Disposition: model.DispositionRequireApproval,
			RiskLevel:   model.RiskLevelMedium,
		},
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "github__create_pr", "github", "create_pr", "git")

	ctx := context.Background()

	// Trigger the approval hold.
	holdResult := gw.routeToolCall(ctx, "github__create_pr", map[string]any{"base": "main"})
	var holdResp map[string]any
	json.Unmarshal([]byte(holdResult.Content[0].Text), &holdResp)
	approvalID := holdResp["approval_id"].(string)

	// Approve it via the orchestrator directly.
	_, err := gw.approvals.Approve(ctx, approvalID, approval.ApproveRequest{
		Comment: "Looks good",
	})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Now check — should auto-execute. Since we have no real downstream
	// client, it'll error at forwarding, but the approval path is exercised.
	checkResult := gw.handleCheckApproval(ctx, map[string]any{
		"approval_id": approvalID,
	})

	// The queued call should have been dequeued.
	_, stillQueued := gw.queue.peek(approvalID)
	if stillQueued {
		t.Error("expected call to be dequeued after auto-execution")
	}

	// Result should be an error from forwarding (nil client).
	if len(checkResult.Content) == 0 {
		t.Fatal("expected content")
	}
}

func TestCheckApproval_DeniedRemovesFromQueue(t *testing.T) {
	pe := &mockPolicyEngine{
		decision: model.Decision{
			Disposition: model.DispositionRequireApproval,
			RiskLevel:   model.RiskLevelMedium,
		},
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "github__create_pr", "github", "create_pr", "git")

	ctx := context.Background()

	// Trigger approval hold.
	holdResult := gw.routeToolCall(ctx, "github__create_pr", map[string]any{"base": "main"})
	var holdResp map[string]any
	json.Unmarshal([]byte(holdResult.Content[0].Text), &holdResp)
	approvalID := holdResp["approval_id"].(string)

	// Deny it.
	_, err := gw.approvals.Deny(ctx, approvalID, approval.DenyRequest{
		Reason: "Not now",
	})
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}

	// Check — should return denied and remove from queue.
	checkResult := gw.handleCheckApproval(ctx, map[string]any{
		"approval_id": approvalID,
	})

	if !checkResult.IsError {
		t.Error("expected error for denied approval")
	}

	_, stillQueued := gw.queue.peek(approvalID)
	if stillQueued {
		t.Error("expected call to be removed from queue after denial")
	}
}

func TestAuditStore_RecordsEvents(t *testing.T) {
	pe := &mockPolicyEngine{
		decision: model.Decision{
			Disposition: model.DispositionDeny,
			RiskLevel:   model.RiskLevelCritical,
			DenialReason: "test denial",
		},
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "github__delete_repo", "github", "delete_repo", "")
	store := gw.auditStore.(*memAuditStore)

	ctx := context.Background()
	gw.routeToolCall(ctx, "github__delete_repo", map[string]any{})

	store.mu.RLock()
	defer store.mu.RUnlock()

	if len(store.events) < 2 {
		t.Fatalf("expected at least 2 audit events (intercepted + denied), got %d", len(store.events))
	}

	// First event: intercepted.
	if store.events[0].EventType != model.EventTypeToolCallIntercepted {
		t.Errorf("event[0].Type = %q, want %q", store.events[0].EventType, model.EventTypeToolCallIntercepted)
	}
	// Second event: denied.
	if store.events[1].EventType != model.EventTypeToolCallDenied {
		t.Errorf("event[1].Type = %q, want %q", store.events[1].EventType, model.EventTypeToolCallDenied)
	}
}

func TestRouteToolCall_PolicyMappingToolPrefix(t *testing.T) {
	// Verify that the tool prefix from config is used when building
	// the action type for policy evaluation.
	var capturedReq policy.EvaluationRequest
	pe := &capturingPolicyEngine{
		decision: model.Decision{
			Disposition: model.DispositionAllow,
			RiskLevel:   model.RiskLevelLow,
		},
		captured: &capturedReq,
	}
	gw := testGateway(t, pe)
	addFakeRoute(gw, "github__create_pull_request", "github", "create_pull_request", "git")

	ctx := context.Background()
	gw.routeToolCall(ctx, "github__create_pull_request", map[string]any{"base": "main"})

	// The action type should include the tool prefix.
	if capturedReq.Action.Type != "git.create_pull_request" {
		t.Errorf("Action.Type = %q, want %q", capturedReq.Action.Type, "git.create_pull_request")
	}

	// ToolCall context should be populated.
	if capturedReq.ToolCall == nil {
		t.Fatal("expected ToolCall context")
	}
	if capturedReq.ToolCall.ServerName != "github" {
		t.Errorf("ToolCall.ServerName = %q, want %q", capturedReq.ToolCall.ServerName, "github")
	}
	if capturedReq.ToolCall.Arguments["base"] != "main" {
		t.Errorf("ToolCall.Arguments[base] = %v, want %q", capturedReq.ToolCall.Arguments["base"], "main")
	}
}

// capturingPolicyEngine records the last evaluation request.
type capturingPolicyEngine struct {
	decision model.Decision
	captured *policy.EvaluationRequest
}

func (c *capturingPolicyEngine) Evaluate(_ context.Context, req policy.EvaluationRequest) (model.Decision, error) {
	*c.captured = req
	return c.decision, nil
}
