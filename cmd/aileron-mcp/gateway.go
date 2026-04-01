// Package main implements the Aileron MCP gateway.
//
// The gateway aggregates tools from downstream MCP servers, re-exposes them
// under namespaced names, and intercepts every tools/call for policy evaluation.
// When a call requires approval, the original request is queued and auto-executed
// once a human approves it through the Aileron UI.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/core/approval"
	"github.com/ALRubinger/aileron/core/audit"
	"github.com/ALRubinger/aileron/core/config"
	"github.com/ALRubinger/aileron/core/mcpclient"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/policy"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/google/uuid"
)

const (
	// namespaceSep separates the server name from the tool name in
	// the aggregated tool list (e.g. "github__create_pull_request").
	namespaceSep = "__"

	// checkApprovalTool is the meta-tool for checking approval status.
	checkApprovalTool = "aileron__check_approval"
)

// toolRoute maps a namespaced tool name back to its downstream client and
// the original (un-prefixed) tool name.
type toolRoute struct {
	client       *mcpclient.Client
	originalName string
	serverName   string
	// toolPrefix from the server's policy_mapping config, if any.
	toolPrefix string
}

// gateway manages downstream MCP server connections, policy evaluation,
// approval orchestration, and tool routing.
type gateway struct {
	clients      []*mcpclient.Client
	routes       map[string]toolRoute // namespaced tool name → route
	tools        []toolDef            // aggregated tool list (cached)
	policyEngine policy.Engine
	approvals    approval.Orchestrator
	auditStore   audit.Store
	queue        *callQueue
	workspaceID  string
	log          *slog.Logger
}

// gatewayConfig holds the dependencies for creating a new gateway.
type gatewayConfig struct {
	Cfg          *config.Config
	Vault        vault.Vault
	PolicyEngine policy.Engine
	Approvals    approval.Orchestrator
	AuditStore   audit.Store
	Log          *slog.Logger
}

// newGateway connects to all downstream MCP servers defined in cfg, discovers
// their tools, and builds the aggregated tool routing table.
func newGateway(ctx context.Context, gc gatewayConfig) (*gateway, error) {
	workspaceID := gc.Cfg.WorkspaceID
	if workspaceID == "" {
		workspaceID = "default"
	}

	g := &gateway{
		routes:       make(map[string]toolRoute),
		policyEngine: gc.PolicyEngine,
		approvals:    gc.Approvals,
		auditStore:   gc.AuditStore,
		queue:        newCallQueue(),
		workspaceID:  workspaceID,
		log:          gc.Log,
	}

	for _, ds := range gc.Cfg.DownstreamServers {
		// Resolve vault:// env vars.
		env, err := resolveEnv(ctx, ds.Env, gc.Vault)
		if err != nil {
			return nil, fmt.Errorf("gateway: server %q: %w", ds.Name, err)
		}

		// Convert map to KEY=VALUE slice for subprocess.
		envSlice := make([]string, 0, len(env))
		for k, val := range env {
			envSlice = append(envSlice, k+"="+val)
		}

		client, err := mcpclient.NewClient(ctx, ds.Name, ds.Command, envSlice)
		if err != nil {
			g.closeAll()
			return nil, fmt.Errorf("gateway: start server %q: %w", ds.Name, err)
		}

		g.clients = append(g.clients, client)

		// Determine tool prefix for policy mapping.
		var toolPrefix string
		if ds.PolicyMapping != nil {
			toolPrefix = ds.PolicyMapping.ToolPrefix
		}

		// Register each tool with a namespaced name.
		for _, tool := range client.Tools() {
			qualifiedName := ds.Name + namespaceSep + tool.Name
			g.routes[qualifiedName] = toolRoute{
				client:       client,
				originalName: tool.Name,
				serverName:   ds.Name,
				toolPrefix:   toolPrefix,
			}
			g.tools = append(g.tools, toolDef{
				Name:        qualifiedName,
				Description: fmt.Sprintf("%s (via %s)", tool.Description, ds.Name),
				InputSchema: toSchema(tool.InputSchema),
			})
		}

		gc.Log.Info("connected downstream server",
			"name", ds.Name,
			"tools", len(client.Tools()),
		)
	}

	// Always add the check_approval meta-tool.
	g.tools = append(g.tools, checkApprovalToolDef())

	return g, nil
}

// aggregatedTools returns the full list of tools from all downstream servers
// plus the aileron meta-tools.
func (g *gateway) aggregatedTools() []toolDef {
	return g.tools
}

// routeToolCall intercepts a tool call, evaluates it against policy, and
// either forwards it to the downstream server, blocks it for approval, or
// denies it.
func (g *gateway) routeToolCall(ctx context.Context, name string, arguments map[string]any) toolResult {
	if name == checkApprovalTool {
		return g.handleCheckApproval(ctx, arguments)
	}

	route, ok := g.routes[name]
	if !ok {
		return toolResult{
			Content: []toolContent{{Type: "text", Text: "Unknown tool: " + name}},
			IsError: true,
		}
	}

	// Emit intercepted audit event.
	g.emitAuditEvent(ctx, model.EventTypeToolCallIntercepted, map[string]any{
		"tool":        name,
		"server_name": route.serverName,
		"original":    route.originalName,
	})

	// Build the policy evaluation request.
	actionType := route.originalName
	if route.toolPrefix != "" {
		actionType = route.toolPrefix + "." + route.originalName
	}

	evalReq := policy.EvaluationRequest{
		WorkspaceID: g.workspaceID,
		AgentID:     "mcp_agent",
		Action: model.ActionIntent{
			Type:    actionType,
			Summary: fmt.Sprintf("Tool call: %s", name),
		},
		Context: model.IntentContext{
			SourcePlatform: "aileron_gateway",
		},
		ToolCall: &policy.ToolCallContext{
			ServerName:    route.serverName,
			ToolName:      route.originalName,
			QualifiedName: name,
			Arguments:     arguments,
		},
	}

	// Evaluate policy.
	decision, err := g.policyEngine.Evaluate(ctx, evalReq)
	if err != nil {
		g.log.Error("policy evaluation failed", "tool", name, "error", err)
		// On policy error, deny by default for safety.
		g.emitAuditEvent(ctx, model.EventTypeToolCallDenied, map[string]any{
			"tool":   name,
			"reason": fmt.Sprintf("policy evaluation error: %v", err),
		})
		return toolResult{
			Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Policy evaluation failed: %v", err)}},
			IsError: true,
		}
	}

	switch decision.Disposition {
	case model.DispositionAllow, model.DispositionAllowModified:
		g.emitAuditEvent(ctx, model.EventTypeToolCallForwarded, map[string]any{
			"tool":        name,
			"disposition": string(decision.Disposition),
		})
		return g.forwardToDownstream(ctx, route, arguments)

	case model.DispositionDeny:
		reason := decision.DenialReason
		if reason == "" {
			reason = "denied by policy"
		}
		g.emitAuditEvent(ctx, model.EventTypeToolCallDenied, map[string]any{
			"tool":   name,
			"reason": reason,
		})
		return toolResult{
			Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Action denied: %s", reason)}},
			IsError: true,
		}

	case model.DispositionRequireApproval:
		g.emitAuditEvent(ctx, model.EventTypeToolCallPendingApproval, map[string]any{
			"tool": name,
		})
		return g.holdForApproval(ctx, name, route, arguments, decision)

	default:
		g.emitAuditEvent(ctx, model.EventTypeToolCallForwarded, map[string]any{
			"tool":        name,
			"disposition": string(decision.Disposition),
		})
		return g.forwardToDownstream(ctx, route, arguments)
	}
}

// emitAuditEvent records an audit event if the audit store is configured.
func (g *gateway) emitAuditEvent(ctx context.Context, eventType model.EventType, payload map[string]any) {
	if g.auditStore == nil {
		return
	}
	event := audit.Event{
		EventID:   uuid.New().String(),
		EventType: eventType,
		Actor: model.ActorRef{
			ID:   "aileron_gateway",
			Type: model.ActorTypeService,
		},
		Payload:   payload,
		Timestamp: time.Now(),
	}
	if err := g.auditStore.Append(ctx, event); err != nil {
		g.log.Warn("failed to record audit event",
			"event_type", string(eventType),
			"error", err,
		)
	}
}

// forwardToDownstream sends a tool call to the downstream MCP server and
// returns its result.
func (g *gateway) forwardToDownstream(ctx context.Context, route toolRoute, arguments map[string]any) toolResult {
	if route.client == nil {
		return toolResult{
			Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Downstream server not available for tool: %s", route.originalName)}},
			IsError: true,
		}
	}
	result, err := route.client.CallTool(ctx, route.originalName, arguments)
	if err != nil {
		return toolResult{
			Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Error calling %s: %v", route.originalName, err)}},
			IsError: true,
		}
	}

	var content []toolContent
	for _, c := range result.Content {
		content = append(content, toolContent{Type: c.Type, Text: c.Text})
	}
	return toolResult{
		Content: content,
		IsError: result.IsError,
	}
}

// holdForApproval queues a tool call for later execution and returns a
// pending-approval response to the agent.
func (g *gateway) holdForApproval(ctx context.Context, qualifiedName string, route toolRoute, arguments map[string]any, decision model.Decision) toolResult {
	// Create an approval request if orchestrator is available.
	approvalID := ""
	if g.approvals != nil {
		rationale := "Policy requires approval"
		if len(decision.MatchedPolicies) > 0 {
			rationale = decision.MatchedPolicies[0].Explanation
		}

		appr, err := g.approvals.Request(ctx, approval.ApprovalRequest{
			IntentID:    fmt.Sprintf("tool-call-%s-%d", qualifiedName, g.queue.len()),
			WorkspaceID: g.workspaceID,
			Rationale:   rationale,
		})
		if err != nil {
			g.log.Error("failed to create approval request", "tool", qualifiedName, "error", err)
			return toolResult{
				Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Failed to create approval: %v", err)}},
				IsError: true,
			}
		}
		approvalID = appr.ApprovalID
	} else {
		// No orchestrator — generate a placeholder ID.
		approvalID = fmt.Sprintf("apr_pending_%d", g.queue.len())
	}

	// Queue the original call for auto-execution after approval.
	g.queue.enqueue(approvalID, queuedCall{
		ServerName:    route.serverName,
		ToolName:      route.originalName,
		QualifiedName: qualifiedName,
		Arguments:     arguments,
	})

	g.log.Info("tool call held for approval",
		"tool", qualifiedName,
		"approval_id", approvalID,
	)

	return jsonResult(map[string]any{
		"status":      "pending_approval",
		"approval_id": approvalID,
		"message":     fmt.Sprintf("This action requires human approval. Use %s with the approval_id to check status.", checkApprovalTool),
		"tool":        qualifiedName,
	})
}

// handleCheckApproval checks an approval's status and, if approved,
// auto-executes the queued tool call.
func (g *gateway) handleCheckApproval(ctx context.Context, args map[string]any) toolResult {
	approvalID := argStr(args, "approval_id")
	if approvalID == "" {
		return errorResult("approval_id is required")
	}

	// Check if we have a queued call for this approval.
	call, queued := g.queue.peek(approvalID)
	if !queued {
		return jsonResult(map[string]any{
			"approval_id": approvalID,
			"status":      "not_found",
			"message":     "No pending tool call found for this approval ID.",
		})
	}

	// Check the approval status via the orchestrator.
	if g.approvals != nil {
		appr, err := g.approvals.Get(ctx, approvalID)
		if err != nil {
			return jsonResult(map[string]any{
				"approval_id": approvalID,
				"status":      "error",
				"message":     fmt.Sprintf("Failed to check approval: %v", err),
			})
		}

		switch appr.Status {
		case approval.StatusPending:
			return jsonResult(map[string]any{
				"approval_id": approvalID,
				"status":      "pending",
				"message":     "Still waiting for human approval.",
				"tool":        call.QualifiedName,
			})

		case approval.StatusDenied:
			g.queue.dequeue(approvalID)
			return toolResult{
				Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Action denied: %s → %s", call.summary(), approvalID)}},
				IsError: true,
			}

		case approval.StatusApproved, approval.StatusModified:
			// Approved — auto-execute the queued call.
			return g.executeQueuedCall(ctx, approvalID)

		default:
			return jsonResult(map[string]any{
				"approval_id": approvalID,
				"status":      string(appr.Status),
				"message":     fmt.Sprintf("Approval status: %s", appr.Status),
			})
		}
	}

	// No orchestrator — the call stays queued until manually managed.
	return jsonResult(map[string]any{
		"approval_id": approvalID,
		"status":      "pending",
		"message":     "Waiting for approval. No approval orchestrator configured.",
		"tool":        call.QualifiedName,
	})
}

// executeQueuedCall dequeues and forwards a previously held tool call.
func (g *gateway) executeQueuedCall(ctx context.Context, approvalID string) toolResult {
	call, ok := g.queue.dequeue(approvalID)
	if !ok {
		return errorResult("queued call not found for approval: " + approvalID)
	}

	route, ok := g.routes[call.QualifiedName]
	if !ok {
		return errorResult("downstream server no longer available for tool: " + call.QualifiedName)
	}

	g.log.Info("auto-executing approved tool call",
		"tool", call.QualifiedName,
		"approval_id", approvalID,
	)

	return g.forwardToDownstream(ctx, route, call.Arguments)
}

// closeAll terminates all downstream client connections and logs each shutdown.
func (g *gateway) closeAll() {
	for _, c := range g.clients {
		g.log.Info("closing downstream server", "name", c.Name())
		if err := c.Close(); err != nil {
			g.log.Warn("error closing downstream server",
				"name", c.Name(),
				"error", err,
			)
		}
	}
}

// Shutdown gracefully shuts down the gateway, closing all downstream
// connections. The provided context can be used to set a deadline.
func (g *gateway) Shutdown(ctx context.Context) {
	g.log.Info("gateway shutdown starting")

	done := make(chan struct{})
	go func() {
		g.closeAll()
		close(done)
	}()

	select {
	case <-done:
		g.log.Info("gateway shutdown complete")
	case <-ctx.Done():
		g.log.Warn("gateway shutdown timed out", "error", ctx.Err())
	}
}

// resolveEnv resolves vault:// prefixed env values using the vault, or
// passes them through unchanged. If vault is nil, vault:// references are
// treated as an error.
func resolveEnv(ctx context.Context, env map[string]string, v vault.Vault) (map[string]string, error) {
	if len(env) == 0 {
		return nil, nil
	}

	vaultGet := func(path string) ([]byte, error) {
		if v == nil {
			return nil, fmt.Errorf("vault not available; cannot resolve vault://%s", path)
		}
		secret, err := v.Get(ctx, path)
		if err != nil {
			return nil, err
		}
		return secret.Value, nil
	}

	return config.ResolveVaultEnv(env, vaultGet)
}

// checkApprovalToolDef returns the tool definition for the aileron meta-tool.
func checkApprovalToolDef() toolDef {
	return toolDef{
		Name:        checkApprovalTool,
		Description: "Check the status of a pending approval. If the action has been approved, the original tool call is automatically executed and its result is returned.",
		InputSchema: schema{
			Type: "object",
			Properties: map[string]schemaProp{
				"approval_id": {Type: "string", Description: "The approval ID returned when a tool call was held for approval"},
			},
			Required: []string{"approval_id"},
		},
	}
}

// toSchema converts a generic map (from MCP tools/list) to our schema type.
func toSchema(raw map[string]any) schema {
	if raw == nil {
		return schema{Type: "object"}
	}

	s := schema{Type: "object"}

	if t, ok := raw["type"].(string); ok {
		s.Type = t
	}

	if props, ok := raw["properties"].(map[string]any); ok {
		s.Properties = make(map[string]schemaProp, len(props))
		for name, v := range props {
			prop := schemaProp{}
			if pm, ok := v.(map[string]any); ok {
				if t, ok := pm["type"].(string); ok {
					prop.Type = t
				}
				if d, ok := pm["description"].(string); ok {
					prop.Description = d
				}
			}
			s.Properties[name] = prop
		}
	}

	if req, ok := raw["required"].([]any); ok {
		for _, r := range req {
			if rs, ok := r.(string); ok {
				s.Required = append(s.Required, rs)
			}
		}
	}

	return s
}

// lookupRoute returns the route for a namespaced tool name, if it exists.
func (g *gateway) lookupRoute(qualifiedName string) (toolRoute, bool) {
	r, ok := g.routes[qualifiedName]
	return r, ok
}

// serverNameFromQualified extracts the server name from a namespaced tool name.
func serverNameFromQualified(qualifiedName string) string {
	if idx := strings.Index(qualifiedName, namespaceSep); idx >= 0 {
		return qualifiedName[:idx]
	}
	return ""
}

// originalNameFromQualified extracts the original tool name from a namespaced tool name.
func originalNameFromQualified(qualifiedName string) string {
	if idx := strings.Index(qualifiedName, namespaceSep); idx >= 0 {
		return qualifiedName[idx+len(namespaceSep):]
	}
	return qualifiedName
}
