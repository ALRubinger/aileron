// Package main implements an MCP (Model Context Protocol) server that exposes
// the Aileron control plane as tools callable by Claude Code and other MCP clients.
//
// It communicates over stdio using JSON-RPC 2.0, per the MCP specification.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	aileron "github.com/ALRubinger/aileron/sdk/go"
)

// --- JSON-RPC types ---

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- MCP types ---

type toolDef struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	InputSchema schema    `json:"inputSchema"`
}

type schema struct {
	Type       string                `json:"type"`
	Properties map[string]schemaProp `json:"properties,omitempty"`
	Required   []string              `json:"required,omitempty"`
}

type schemaProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// --- Server ---

type server struct {
	client *aileron.Client
}

func main() {
	apiURL := os.Getenv("AILERON_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}
	apiKey := os.Getenv("AILERON_API_KEY")

	client := aileron.NewClient(apiURL, aileron.WithAPIKey(apiKey))
	s := &server{client: client}

	scanner := bufio.NewScanner(os.Stdin)
	// Increase buffer size for large messages.
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		resp := s.handle(req)
		if resp != nil {
			data, _ := json.Marshal(resp)
			fmt.Fprintf(os.Stdout, "%s\n", data)
		}
	}
}

func (s *server) handle(req jsonrpcRequest) *jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "aileron",
					"version": "0.0.1",
				},
			},
		}

	case "notifications/initialized":
		return nil // no response for notifications

	case "tools/list":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": s.tools(),
			},
		}

	case "tools/call":
		var params callToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(req.ID, -32602, "invalid params: "+err.Error())
		}
		result := s.callTool(params)
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}

	case "ping":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}

	default:
		return errorResponse(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *server) tools() []toolDef {
	return []toolDef{
		{
			Name:        "submit_intent",
			Description: "Submit an action intent to the Aileron control plane for policy evaluation. Returns the disposition (allow/deny/require_approval) and any grant or approval IDs.",
			InputSchema: schema{
				Type: "object",
				Properties: map[string]schemaProp{
					"action_type":  {Type: "string", Description: "Dot-namespaced action type, e.g. 'git.pull_request.create', 'payment.charge'"},
					"summary":      {Type: "string", Description: "Human-readable summary of what the agent wants to do"},
					"justification": {Type: "string", Description: "Why this action is needed"},
					"repository":   {Type: "string", Description: "For git actions: owner/repo (e.g. 'acme/checkout')"},
					"branch":       {Type: "string", Description: "For git actions: source/head branch"},
					"base_branch":  {Type: "string", Description: "For git actions: target/base branch"},
					"pr_title":     {Type: "string", Description: "For git PR actions: pull request title"},
					"pr_body":      {Type: "string", Description: "For git PR actions: pull request body"},
					"workspace_id": {Type: "string", Description: "Workspace ID (default: 'default')"},
				},
				Required: []string{"action_type", "summary"},
			},
		},
		{
			Name:        "check_approval",
			Description: "Check the status of a pending approval request. Returns the current status and, if approved, the execution grant ID.",
			InputSchema: schema{
				Type: "object",
				Properties: map[string]schemaProp{
					"approval_id": {Type: "string", Description: "The approval ID to check"},
				},
				Required: []string{"approval_id"},
			},
		},
		{
			Name:        "execute_action",
			Description: "Execute an approved action using an execution grant. The grant must be active and not expired.",
			InputSchema: schema{
				Type: "object",
				Properties: map[string]schemaProp{
					"grant_id": {Type: "string", Description: "The execution grant ID"},
				},
				Required: []string{"grant_id"},
			},
		},
		{
			Name:        "list_pending_approvals",
			Description: "List all pending approval requests in the workspace.",
			InputSchema: schema{
				Type: "object",
				Properties: map[string]schemaProp{
					"workspace_id": {Type: "string", Description: "Workspace ID (default: 'default')"},
				},
			},
		},
	}
}

func (s *server) callTool(params callToolParams) toolResult {
	ctx := context.Background()

	switch params.Name {
	case "submit_intent":
		return s.submitIntent(ctx, params.Arguments)
	case "check_approval":
		return s.checkApproval(ctx, params.Arguments)
	case "execute_action":
		return s.executeAction(ctx, params.Arguments)
	case "list_pending_approvals":
		return s.listPendingApprovals(ctx, params.Arguments)
	default:
		return toolResult{
			Content: []toolContent{{Type: "text", Text: "Unknown tool: " + params.Name}},
			IsError: true,
		}
	}
}

func (s *server) submitIntent(ctx context.Context, args map[string]any) toolResult {
	actionType := argStr(args, "action_type")
	summary := argStr(args, "summary")
	workspaceID := argStr(args, "workspace_id")
	if workspaceID == "" {
		workspaceID = "default"
	}

	req := aileron.CreateIntentRequest{
		WorkspaceID:    workspaceID,
		AgentID:        "claude_code",
		IdempotencyKey: fmt.Sprintf("mcp-%s-%d", actionType, os.Getpid()),
		Action: aileron.ActionIntent{
			Type:    actionType,
			Summary: summary,
		},
		Context: &aileron.IntentContext{
			SourcePlatform: strPtr("claude_code"),
			UserPresent:    boolPtr(true),
		},
	}

	if just := argStr(args, "justification"); just != "" {
		req.Action.Justification = &just
	}

	// Populate domain-specific fields.
	if repo := argStr(args, "repository"); repo != "" {
		branch := argStr(args, "branch")
		baseBranch := argStr(args, "base_branch")
		prTitle := argStr(args, "pr_title")
		prBody := argStr(args, "pr_body")
		provider := "github"
		req.Action.Domain = &aileron.DomainAction{
			Git: &aileron.GitAction{
				Provider:   &provider,
				Repository: &repo,
				Branch:     nilIfEmpty(branch),
				BaseBranch: nilIfEmpty(baseBranch),
				PRTitle:    nilIfEmpty(prTitle),
				PRBody:     nilIfEmpty(prBody),
			},
		}
	}

	intent, err := s.client.Intents.Create(ctx, req)
	if err != nil {
		return errorResult("Failed to submit intent: " + err.Error())
	}

	result := map[string]any{
		"intent_id":   intent.IntentID,
		"status":      intent.Status,
		"disposition": intent.Decision.Disposition,
		"risk_level":  intent.Decision.RiskLevel,
	}
	if intent.Decision.ApprovalID != nil {
		result["approval_id"] = *intent.Decision.ApprovalID
		result["message"] = fmt.Sprintf("This action requires approval. Approval ID: %s. The user can approve at the Aileron UI.", *intent.Decision.ApprovalID)
	}
	if intent.Decision.ExecutionGrantID != nil {
		result["grant_id"] = *intent.Decision.ExecutionGrantID
		result["message"] = "Action approved by policy. Use execute_action with the grant_id to proceed."
	}
	if intent.Decision.DenialReason != nil {
		result["denial_reason"] = *intent.Decision.DenialReason
		result["message"] = fmt.Sprintf("Action denied: %s", *intent.Decision.DenialReason)
	}

	return jsonResult(result)
}

func (s *server) checkApproval(ctx context.Context, args map[string]any) toolResult {
	approvalID := argStr(args, "approval_id")
	if approvalID == "" {
		return errorResult("approval_id is required")
	}

	approval, err := s.client.Approvals.Get(ctx, approvalID)
	if err != nil {
		return errorResult("Failed to get approval: " + err.Error())
	}

	result := map[string]any{
		"approval_id": approval.ApprovalID,
		"status":      approval.Status,
		"intent_id":   approval.IntentID,
	}

	switch approval.Status {
	case "approved":
		// Get the intent to find the grant ID.
		intent, err := s.client.Intents.Get(ctx, approval.IntentID)
		if err == nil && intent.Decision.ExecutionGrantID != nil {
			result["grant_id"] = *intent.Decision.ExecutionGrantID
			result["message"] = "Approved! Use execute_action with the grant_id to proceed."
		} else {
			result["message"] = "Approved, but grant ID not yet available."
		}
	case "denied":
		result["message"] = "The approval request was denied."
	case "pending":
		result["message"] = "Still waiting for human approval."
	default:
		result["message"] = "Approval status: " + approval.Status
	}

	return jsonResult(result)
}

func (s *server) executeAction(ctx context.Context, args map[string]any) toolResult {
	grantID := argStr(args, "grant_id")
	if grantID == "" {
		return errorResult("grant_id is required")
	}

	resp, err := s.client.Executions.Run(ctx, aileron.ExecutionRunRequest{
		GrantID: grantID,
	})
	if err != nil {
		return errorResult("Failed to execute: " + err.Error())
	}

	result := map[string]any{
		"execution_id": resp.ExecutionID,
		"status":       resp.Status,
		"message":      "Execution started. ID: " + resp.ExecutionID,
	}

	// Poll for completion (the execution may complete synchronously).
	exec, err := s.client.Executions.Get(ctx, resp.ExecutionID)
	if err == nil {
		result["status"] = exec.Status
		if exec.Output != nil {
			result["output"] = *exec.Output
		}
		if exec.ReceiptRef != nil {
			result["receipt_ref"] = *exec.ReceiptRef
			result["message"] = "Execution completed. Result: " + *exec.ReceiptRef
		}
	}

	return jsonResult(result)
}

func (s *server) listPendingApprovals(ctx context.Context, args map[string]any) toolResult {
	workspaceID := argStr(args, "workspace_id")
	if workspaceID == "" {
		workspaceID = "default"
	}

	resp, err := s.client.Approvals.List(ctx, workspaceID)
	if err != nil {
		return errorResult("Failed to list approvals: " + err.Error())
	}

	var pending []map[string]any
	for _, a := range resp.Items {
		if a.Status == "pending" {
			entry := map[string]any{
				"approval_id": a.ApprovalID,
				"intent_id":   a.IntentID,
				"status":      a.Status,
				"requested_at": a.RequestedAt.Format("2006-01-02T15:04:05Z"),
			}
			if a.Rationale != nil {
				entry["rationale"] = *a.Rationale
			}
			pending = append(pending, entry)
		}
	}

	result := map[string]any{
		"count":     len(pending),
		"approvals": pending,
	}
	if len(pending) == 0 {
		result["message"] = "No pending approvals."
	} else {
		result["message"] = fmt.Sprintf("%d pending approval(s).", len(pending))
	}

	return jsonResult(result)
}

// --- Helpers ---

func argStr(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func strPtr(s string) *string   { return &s }
func boolPtr(b bool) *bool      { return &b }

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func jsonResult(v any) toolResult {
	data, _ := json.MarshalIndent(v, "", "  ")
	return toolResult{
		Content: []toolContent{{Type: "text", Text: string(data)}},
	}
}

func errorResult(msg string) toolResult {
	return toolResult{
		Content: []toolContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}

func errorResponse(id json.RawMessage, code int, msg string) *jsonrpcResponse {
	return &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: msg},
	}
}
