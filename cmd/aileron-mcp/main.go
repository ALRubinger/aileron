// Package main implements the Aileron MCP server.
//
// Aileron acts as an MCP server installed into agent hosts (Claude Code,
// Cursor, Continue, etc.). When AILERON_URL is set, the server queries
// the Aileron daemon's /v1/actions endpoint at startup, generates an MCP
// tool definition for each installed action, and routes incoming
// tools/call invocations to /v1/actions/{name}/run for synchronous
// execution. This is the "MCP canonical" path for action exposure
// ratified during the working session on 2026-05-03.
//
// When AILERON_COMMS_SOCKET is set (e.g. when launched by `aileron
// launch`), the server additionally exposes comms tools — read_messages,
// send_message, draft_reply, http_request — that talk to the launch
// product's CommsServer over a Unix socket for Slack/Discord inbound
// push handling.
//
// The binary communicates over stdio using JSON-RPC 2.0, per the MCP
// specification.
//
// Configuration:
//
//	AILERON_URL          - URL of the Aileron daemon (e.g. http://127.0.0.1:54321).
//	                       When set, action tools are discovered and exposed.
//	AILERON_TOKEN        - Optional bearer token for authenticating with
//	                       the Aileron API.
//	AILERON_COMMS_SOCKET - Path to the launch product's comms Unix
//	                       socket. When set, comms tools are exposed.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ALRubinger/aileron/internal/version"
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

// --- Action discovery / execution (Aileron daemon) ---

// actionMeta is the subset of the daemon's /v1/actions response we
// need to derive an MCP tool definition. Mirrors api.Action without
// pulling the generated types into this binary.
type actionMeta struct {
	Name     string                `json:"name"`
	Body     string                `json:"body"`
	Inputs   []actionInput         `json:"inputs"`
	Match    *actionMatch          `json:"match,omitempty"`
	Approval *actionApprovalPolicy `json:"approval,omitempty"`
}

type actionApprovalPolicy struct {
	Required *bool `json:"required,omitempty"`
}

// requiresApproval reports whether the action's manifest gates
// execution on user approval. Treats unset / nil as "no approval
// required" — matching the runtime's default behavior for actions
// without an [approval] block.
func (a actionMeta) requiresApproval() bool {
	return a.Approval != nil && a.Approval.Required != nil && *a.Approval.Required
}

type actionInput struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    *bool  `json:"required"`
	Description string `json:"description"`
}

type actionMatch struct {
	Intent string `json:"intent"`
}

type actionListResponse struct {
	Items []actionMeta `json:"items"`
}

type actionRunRequest struct {
	Args map[string]any `json:"args"`
}

type actionRunResponse struct {
	AuditID string  `json:"audit_id"`
	Result  *string `json:"result,omitempty"`
}

// --- Server ---

type server struct {
	aileronURL   string
	aileronToken string
	commsSocket  string
	httpClient   *http.Client

	// Discovered actions, populated at startup when AILERON_URL is set.
	// Keys of actionNameMap are snake_case (LLM-facing) tool names;
	// values are manifest names (kebab-case) used in /v1/actions/{name}/run.
	actionTools   []toolDef
	actionNameMap map[string]string
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
	Description: "Make an authenticated HTTP request to a URL covered by an api_key binding. Aileron matches the URL against configured api_key bindings (vault entries with kind=api_key and a url-pattern label) and injects the secret as a Bearer token. Does NOT inject OAuth credentials — OAuth bindings are scoped per-connector and reachable only via the bound connector's actions (see `aileron action add` for installed actions and `aileron binding list` for OAuth bindings). Requires human approval.",
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

func main() {
	s := &server{
		aileronURL:   os.Getenv("AILERON_URL"),
		aileronToken: os.Getenv("AILERON_TOKEN"),
		commsSocket:  os.Getenv("AILERON_COMMS_SOCKET"),
		httpClient:   &http.Client{},
	}

	// Discover installed actions from the Aileron daemon. Best-effort:
	// if discovery fails (daemon not running yet, vault locked, etc.)
	// we proceed without action tools. Comms tools remain available.
	if s.aileronURL != "" {
		if tools, nameMap, err := s.discoverActions(context.Background()); err != nil {
			slog.Warn("action discovery failed; continuing without action tools",
				"url", s.aileronURL, "error", err)
		} else {
			s.actionTools = tools
			s.actionNameMap = nameMap
		}
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
	// Dynamically discovered Aileron actions from the daemon's
	// /v1/actions endpoint (per ADR-0008 — kept in MCP shape rather
	// than the OpenAI/Anthropic shape because the agent host's MCP
	// integration is the consumer here).
	tools = append(tools, s.actionTools...)
	return tools
}

func (s *server) dispatchTool(ctx context.Context, name string, args map[string]any) toolResult {
	// Discovered actions: route to /v1/actions/{name}/run on the daemon.
	if manifestName, ok := s.actionNameMap[name]; ok {
		return s.runAction(ctx, manifestName, args)
	}
	// Comms tools: handled in-process via the launch product's Unix socket.
	switch name {
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

// --- Action discovery / execution against the Aileron daemon ---

// discoverActions queries /v1/actions and returns one MCP tool def per
// installed action plus a snake_case → manifest-name lookup map. Per
// ADR-0008 the LLM-facing tool name is snake_case (mapped from the
// kebab-case manifest name); /v1/actions/{name}/run uses the manifest
// name, so the map lets dispatchTool route correctly.
func (s *server) discoverActions(ctx context.Context) ([]toolDef, map[string]string, error) {
	if s.aileronURL == "" {
		return nil, nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.aileronURL+"/v1/actions", nil)
	if err != nil {
		return nil, nil, err
	}
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("/v1/actions: %s", resp.Status)
	}
	var alr actionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&alr); err != nil {
		return nil, nil, fmt.Errorf("decoding /v1/actions: %w", err)
	}
	tools := make([]toolDef, 0, len(alr.Items))
	nameMap := make(map[string]string, len(alr.Items))
	for _, a := range alr.Items {
		td := actionToolDef(a)
		tools = append(tools, td)
		nameMap[td.Name] = a.Name
	}
	return tools, nameMap, nil
}

// runAction synchronously executes an action via the Aileron daemon's
// /v1/actions/{name}/run endpoint and returns an MCP tool result.
// Failures are surfaced as toolResult{IsError: true} so the agent host
// reports them to the LLM as normal tool errors per MCP semantics.
func (s *server) runAction(ctx context.Context, manifestName string, args map[string]any) toolResult {
	if s.aileronURL == "" {
		return errorResult("Aileron daemon not configured (AILERON_URL not set)")
	}
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(actionRunRequest{Args: args})
	if err != nil {
		return errorResult("encoding request: " + err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.aileronURL+"/v1/actions/"+manifestName+"/run", bytes.NewReader(body))
	if err != nil {
		return errorResult("creating request: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorResult("daemon unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult("reading response: " + err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		// Non-2xx: surface the FailureEnvelope (or whatever body the
		// daemon returned) so the agent sees the actionable detail.
		return errorResult(string(rawBody))
	}
	var arr actionRunResponse
	if err := json.Unmarshal(rawBody, &arr); err != nil {
		return errorResult("decoding response: " + err.Error())
	}
	content := ""
	if arr.Result != nil {
		content = *arr.Result
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: content}},
	}
}

// actionToolDef converts an action manifest into an MCP tool
// definition, mirroring augment.Derive (internal/augment/augment.go)
// but in MCP shape (name/description/inputSchema).
func actionToolDef(a actionMeta) toolDef {
	return toolDef{
		Name:        toolName(a.Name),
		Description: deriveDescription(a),
		InputSchema: deriveInputSchema(a),
	}
}

// toolName maps a manifest's kebab-case name to the snake_case
// identifier the LLM sees, per ADR-0008.
func toolName(manifestName string) string {
	return strings.ReplaceAll(manifestName, "-", "_")
}

// deriveDescription extracts the LLM-facing description from the
// action body. Strips a leading "# Heading" line; falls back to
// match.intent when the body is empty.
//
// When the action's manifest declares `[approval] required = true`,
// appends a notice instructing the agent to surface the approval URL
// to the user immediately on tool invocation. The URL is read from
// AILERON_APPROVAL_URL (set by launch's embedded gateway) so the
// agent's prompt to the user names a real, clickable target rather
// than a generic "check the webapp." Tool descriptions are part of
// the MCP system context the LLM factors into planning, so this is
// the natural place for the signal — no mid-conversation injection.
func deriveDescription(a actionMeta) string {
	desc := deriveBaseDescription(a)
	if a.requiresApproval() {
		approvalURL := os.Getenv("AILERON_APPROVAL_URL")
		if approvalURL == "" {
			// Fall back to a generic instruction when launch hasn't
			// set the URL (e.g. running aileron-mcp standalone).
			// Better than dropping the notice entirely.
			approvalURL = "the Aileron webapp"
		}
		notice := fmt.Sprintf(
			"\n\n⚠️ This action requires user approval before it runs. When you call this tool, "+
				"immediately tell the user: \"This action needs your approval — please review and "+
				"approve at %s\". The tool call will block until they decide. Do not paraphrase the "+
				"URL; deliver it verbatim so the user can click through.",
			approvalURL,
		)
		desc = strings.TrimSpace(desc) + notice
	}
	return desc
}

// deriveBaseDescription is the original deriveDescription logic
// without the approval-notice templating. Split out so the templating
// step is the only thing future readers need to understand to follow
// the approval signaling path.
func deriveBaseDescription(a actionMeta) string {
	body := strings.TrimSpace(a.Body)
	if body == "" {
		if a.Match != nil {
			return strings.TrimSpace(a.Match.Intent)
		}
		return ""
	}
	lines := strings.SplitN(body, "\n", 2)
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "# ") {
		if len(lines) == 1 {
			if a.Match != nil {
				return strings.TrimSpace(a.Match.Intent)
			}
			return ""
		}
		return strings.TrimSpace(lines[1])
	}
	return body
}

// deriveInputSchema builds the JSON Schema parameters object from the
// manifest's inputs. Mirrors augment.deriveParameters: type=object
// always, properties only when inputs exist, required when the input
// has Required=true (default true when omitted, per ADR-0003).
func deriveInputSchema(a actionMeta) schema {
	s := schema{Type: "object"}
	if len(a.Inputs) == 0 {
		return s
	}
	s.Properties = make(map[string]schemaProp, len(a.Inputs))
	for _, in := range a.Inputs {
		s.Properties[in.Name] = schemaProp{
			Type:        in.Type,
			Description: in.Description,
		}
		required := true
		if in.Required != nil {
			required = *in.Required
		}
		if required {
			s.Required = append(s.Required, in.Name)
		}
	}
	return s
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
