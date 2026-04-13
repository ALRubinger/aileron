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
	"net"
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
	commsSocket  string
	httpClient   *http.Client
}

var readMessagesTool = toolDef{
	Name:        "read_messages",
	Description: "Read pending messages from communication channels (Slack, Discord). Returns unread messages from the notification queue. Messages with draft_request=true need a reply drafted — call draft_reply with the message ID and your suggested reply.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"service": {Type: "string", Description: "Filter by service: 'slack', 'discord', or empty for all"},
			"channel": {Type: "string", Description: "Filter by channel name, or empty for all channels"},
		},
	},
}

var sendMessageTool = toolDef{
	Name:        "send_message",
	Description: "Send a message to a communication channel (Slack, Discord). Requires human approval.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"service": {Type: "string", Description: "Target service: 'slack' or 'discord'"},
			"channel": {Type: "string", Description: "Channel name or ID to send to"},
			"body":    {Type: "string", Description: "Message text to send"},
		},
		Required: []string{"service", "channel", "body"},
	},
}

var draftReplyTool = toolDef{
	Name:        "draft_reply",
	Description: "Submit a draft reply to a message. The draft is shown to the user for review — they can send, edit, or discard it. Use this when read_messages returns messages with draft_request=true. Do NOT use send_message for drafts; Aileron handles sending after user approval.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"message_id": {Type: "string", Description: "ID of the message to reply to (from read_messages)"},
			"body":       {Type: "string", Description: "Your suggested reply text"},
		},
		Required: []string{"message_id", "body"},
	},
}

var httpRequestTool = toolDef{
	Name:        "http_request",
	Description: "Make an authenticated HTTP request. Aileron matches the URL against configured secrets and injects credentials. Requires human approval.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"method":  {Type: "string", Description: "HTTP method (GET, POST, PUT, DELETE, PATCH)"},
			"url":     {Type: "string", Description: "Target URL"},
			"headers": {Type: "string", Description: "JSON object of request headers (optional)"},
			"body":    {Type: "string", Description: "Request body string (optional)"},
		},
		Required: []string{"method", "url"},
	},
}

// submitIntentTool is the legacy cloud tool.
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
	s := &server{
		aileronURL:   os.Getenv("AILERON_URL"),
		aileronToken: os.Getenv("AILERON_TOKEN"),
		commsSocket:  os.Getenv("AILERON_COMMS_SOCKET"),
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
		tools := s.availableTools()
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": tools,
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

func (s *server) availableTools() []toolDef {
	var tools []toolDef
	if s.commsSocket != "" {
		tools = append(tools, readMessagesTool, draftReplyTool, sendMessageTool, httpRequestTool)
	}
	if s.aileronURL != "" {
		tools = append(tools, submitIntentTool)
	}
	return tools
}

func (s *server) dispatchTool(ctx context.Context, name string, args map[string]any) toolResult {
	switch name {
	case "submit_intent":
		return s.submitIntent(ctx, args)
	case "read_messages":
		return s.readMessages(args)
	case "draft_reply":
		return s.draftReply(args)
	case "send_message":
		return s.sendMessage(args)
	case "http_request":
		return s.httpRequest(args)
	default:
		return errorResult("unknown tool: " + name)
	}
}

func (s *server) readMessages(args map[string]any) toolResult {
	if s.commsSocket == "" {
		return errorResult("comms not available (not launched via aileron)")
	}

	service, _ := args["service"].(string)
	channel, _ := args["channel"].(string)

	resp := requestComms(s.commsSocket, commsRequest{
		Method:  "read_messages",
		Service: service,
		Channel: channel,
	})
	if resp.Error != "" {
		return errorResult(resp.Error)
	}
	return jsonResult(resp.Messages)
}

func (s *server) draftReply(args map[string]any) toolResult {
	if s.commsSocket == "" {
		return errorResult("comms not available (not launched via aileron)")
	}

	messageID, _ := args["message_id"].(string)
	body, _ := args["body"].(string)

	if messageID == "" || body == "" {
		return errorResult("message_id and body are required")
	}

	resp := requestComms(s.commsSocket, commsRequest{
		Method:  "draft_reply",
		ReplyTo: messageID,
		Body:    body,
	})
	if resp.Error != "" {
		return errorResult(resp.Error)
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: "Draft submitted for user review."}},
	}
}

func (s *server) sendMessage(args map[string]any) toolResult {
	if s.commsSocket == "" {
		return errorResult("comms not available (not launched via aileron)")
	}

	service, _ := args["service"].(string)
	channel, _ := args["channel"].(string)
	body, _ := args["body"].(string)

	if service == "" || channel == "" || body == "" {
		return errorResult("service, channel, and body are required")
	}

	resp := requestComms(s.commsSocket, commsRequest{
		Method:  "send_message",
		Service: service,
		Channel: channel,
		Body:    body,
	})
	if resp.Error != "" {
		return errorResult(resp.Error)
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: "Message sent successfully."}},
	}
}

func (s *server) httpRequest(args map[string]any) toolResult {
	if s.commsSocket == "" {
		return errorResult("comms not available (not launched via aileron)")
	}

	method, _ := args["method"].(string)
	url, _ := args["url"].(string)
	headers, _ := args["headers"].(string)
	body, _ := args["body"].(string)

	if method == "" || url == "" {
		return errorResult("method and url are required")
	}

	resp := requestComms(s.commsSocket, commsRequest{
		Method:  "http_request",
		Service: method,  // repurpose Service field for HTTP method
		Channel: url,     // repurpose Channel field for URL
		Body:    body,
		ReplyTo: headers, // repurpose ReplyTo field for headers JSON
	})
	if resp.Error != "" {
		return errorResult(resp.Error)
	}
	// Response body is in the first message's Body field.
	if len(resp.Messages) > 0 {
		return toolResult{
			Content: []toolContent{{Type: "text", Text: resp.Messages[0].Body}},
		}
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: "Request completed (no response body)."}},
	}
}

// --- Comms IPC types (mirrors core/launch/commsserver.go) ---

type commsRequest struct {
	Method  string `json:"method"`
	Service string `json:"service,omitempty"`
	Channel string `json:"channel,omitempty"`
	Body    string `json:"body,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`
}

type commsResponse struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
	Messages []commsMessage `json:"messages,omitempty"`
}

type commsMessage struct {
	ID        string `json:"id"`
	Service   string `json:"service"`
	Channel   string `json:"channel"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp"`
}

func requestComms(socketPath string, req commsRequest) commsResponse {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return commsResponse{Error: "connection failed: " + err.Error()}
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return commsResponse{Error: "encode failed: " + err.Error()}
	}

	var resp commsResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return commsResponse{Error: "decode failed: " + err.Error()}
	}
	return resp
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
