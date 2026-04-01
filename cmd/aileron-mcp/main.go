// Package main implements the Aileron MCP gateway server.
//
// The gateway is the single MCP server that an agent host (Claude Code, etc.)
// connects to. It aggregates tools from downstream MCP servers, re-exposes
// them under namespaced names, and intercepts every tools/call for governance.
//
// It communicates over stdio using JSON-RPC 2.0, per the MCP specification.
//
// Modes:
//   - Embedded (default): runs an in-process Aileron server with in-memory stores.
//   - Remote: set AILERON_API_URL to connect to an existing Aileron server.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/core/app"
	"github.com/ALRubinger/aileron/core/approval"
	"github.com/ALRubinger/aileron/core/config"
	"github.com/ALRubinger/aileron/core/policy"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/google/uuid"
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
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema schema `json:"inputSchema"`
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
	gw  *gateway
	log *slog.Logger
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()

	// Start embedded Aileron server if no remote URL is configured.
	// This provides the control plane (policy engine, approval orchestrator, vault).
	var v vault.Vault
	apiURL := os.Getenv("AILERON_API_URL")
	if apiURL == "" {
		discardLog := slog.New(slog.NewJSONHandler(io.Discard, nil))
		handler, err := app.NewHandler(discardLog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aileron-mcp: failed to start embedded server: %v\n", err)
			os.Exit(1)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "aileron-mcp: failed to listen: %v\n", err)
			os.Exit(1)
		}
		go http.Serve(listener, handler)
		apiURL = "http://" + listener.Addr().String()

		// In embedded mode, create an in-memory vault for credential injection.
		v = vault.NewMemVault()
	}

	// Load gateway configuration.
	cfg, err := config.LoadFromEnvOrDefault()
	if err != nil {
		fmt.Fprintf(os.Stderr, "aileron-mcp: failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Build policy engine with seeded policies.
	policyStore := mem.NewPolicyStore()
	if err := policy.SeedPolicies(ctx, policyStore); err != nil {
		fmt.Fprintf(os.Stderr, "aileron-mcp: failed to seed policies: %v\n", err)
		os.Exit(1)
	}
	policyEngine := policy.NewRuleEngine(policyStore)

	// Build approval orchestrator.
	approvalStore := mem.NewApprovalStore()
	idGen := func() string { return uuid.New().String() }
	orchestrator := approval.NewInMemoryOrchestrator(approvalStore, idGen)

	// Build in-memory audit store.
	auditStore := newMemAuditStore()

	// Connect to downstream MCP servers.
	gw, err := newGateway(ctx, gatewayConfig{
		Cfg:          cfg,
		Vault:        v,
		PolicyEngine: policyEngine,
		Approvals:    orchestrator,
		AuditStore:   auditStore,
		Log:          log,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "aileron-mcp: failed to start gateway: %v\n", err)
		os.Exit(1)
	}

	// Handle SIGTERM and SIGINT for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		gw.Shutdown(shutdownCtx)
		os.Exit(0)
	}()
	defer gw.closeAll()

	s := &server{gw: gw, log: log}

	scanner := bufio.NewScanner(os.Stdin)
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
					"version": "0.0.2",
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
				"tools": s.gw.aggregatedTools(),
			},
		}

	case "tools/call":
		var params callToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(req.ID, -32602, "invalid params: "+err.Error())
		}
		ctx := context.Background()
		result := s.gw.routeToolCall(ctx, params.Name, params.Arguments)
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
