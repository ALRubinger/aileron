// Package main implements the Aileron MCP server.
//
// Aileron acts as an MCP server installed into agent hosts (Claude Code, etc.).
// It exposes a submit_intent tool that agents use to submit governed intents to
// the Aileron execution plane for policy evaluation and execution.
//
// It communicates over stdio using JSON-RPC 2.0, per the MCP specification.
//
// Configuration:
//
//	AILERON_URL   - URL of the Aileron API server (required)
//	AILERON_TOKEN - Bearer token for authenticating with the Aileron API
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ALRubinger/aileron/core/version"
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
	aileronURL   string
	aileronToken string
	httpClient   *http.Client
}

// submitIntentTool is the single tool exposed by the Aileron MCP server.
var submitIntentTool = toolDef{
	Name:        "submit_intent",
	Description: "Submit a governed intent to Aileron for execution",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"intent": {
				Type:        "string",
				Description: "Natural language description of the action to perform",
			},
		},
		Required: []string{"intent"},
	},
}

func main() {
	aileronURL := os.Getenv("AILERON_URL")
	aileronToken := os.Getenv("AILERON_TOKEN")

	if aileronURL == "" {
		fmt.Fprintln(os.Stderr, "aileron-mcp: AILERON_URL is required")
		os.Exit(1)
	}

	s := &server{
		aileronURL:   aileronURL,
		aileronToken: aileronToken,
		httpClient:   &http.Client{},
	}

	// Handle SIGTERM and SIGINT for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

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
					"version": version.Version,
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
				"tools": []toolDef{submitIntentTool},
			},
		}

	case "tools/call":
		var params callToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(req.ID, -32602, "invalid params: "+err.Error())
		}
		ctx := context.Background()
		result := s.dispatchTool(ctx, params.Name, params.Arguments)
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

func (s *server) dispatchTool(ctx context.Context, name string, args map[string]any) toolResult {
	switch name {
	case "submit_intent":
		return s.submitIntent(ctx, args)
	default:
		return errorResult("unknown tool: " + name)
	}
}

func (s *server) submitIntent(ctx context.Context, args map[string]any) toolResult {
	intentText, ok := args["intent"].(string)
	if !ok || intentText == "" {
		return errorResult("intent parameter is required")
	}

	body, err := json.Marshal(map[string]any{
		"action": map[string]any{
			"summary": intentText,
			"type":    "custom",
		},
		"agent_id":     "aileron-mcp",
		"workspace_id": "default",
	})
	if err != nil {
		return errorResult("failed to encode request: " + err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.aileronURL+"/v1/intents", bytes.NewReader(body))
	if err != nil {
		return errorResult("failed to create request: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorResult("failed to submit intent: " + err.Error())
	}
	defer resp.Body.Close()

	var result any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return errorResult("failed to decode response: " + err.Error())
	}

	return jsonResult(result)
}

// --- Helpers ---

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
