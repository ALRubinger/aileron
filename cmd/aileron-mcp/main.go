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
// When AILERON_COMMS_URL + AILERON_SESSION_ID are set (e.g. when
// launched by `aileron launch`), the server additionally exposes comms
// tools — read_messages, send_message, draft_reply, http_request — that
// reach the daemon-owned comms surface via HTTP. Pre-9B these talked to
// a per-session unix socket; ADR-0012 step 9B-2 moved comms ownership
// to the daemon and switched the wire to HTTP long-poll.
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
//	AILERON_COMMS_URL    - URL of the daemon's comms surface (typically
//	                       the same as AILERON_URL). Pair with
//	                       AILERON_SESSION_ID to enable comms tools.
//	AILERON_SESSION_ID   - The launch session id the daemon stamps on
//	                       comms approval entries.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/observability"
	"github.com/ALRubinger/aileron/internal/version"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName names the OTel instrumentation library reported on
// spans this binary emits. Mirrors the convention in
// internal/action and internal/observability.
const tracerName = "github.com/ALRubinger/aileron/cmd/aileron-mcp"

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
	Type        string      `json:"type"`
	Description string      `json:"description"`
	Items       *schemaItem `json:"items,omitempty"`
}

// schemaItem is the JSON Schema `items` clause emitted for array
// properties. Empty struct serializes as `{}` (any-element), which is
// strictly more permissive than omitting the `items` field — some MCP
// hosts (Codex) project a missing `items` to `string[]`, and that
// breaks actions whose elements are objects.
type schemaItem struct {
	Type string `json:"type,omitempty"`
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
	// Enabled mirrors the daemon's per-action overlay state. Absent in
	// older responses; treat as enabled in that case so a daemon that
	// predates the toggle feature still exposes every installed action.
	Enabled *bool `json:"enabled,omitempty"`
}

// isEnabled reports whether the daemon currently exposes this action.
// Treats nil (older daemons) and explicit true as enabled.
func (a actionMeta) isEnabled() bool {
	return a.Enabled == nil || *a.Enabled
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
	ItemsType   string `json:"items_type,omitempty"`
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

// actionRunPendingResponse mirrors the 202 body the daemon returns
// for approval-gated actions: a discriminator, the approval id, a
// per-approval review URL, and the message the LLM is meant to
// surface to the user verbatim. JSON shape pinned by the OpenAPI
// spec; only the fields the MCP wrapper needs are decoded.
type actionRunPendingResponse struct {
	Status     string `json:"status"`
	ApprovalID string `json:"approval_id"`
	ReviewURL  string `json:"review_url,omitempty"`
	Message    string `json:"message"`
}

// actionApprovalResult mirrors the daemon's /v1/action-approvals/{id}/result
// body. Optional fields are pointer-typed so the formatter can
// distinguish "field omitted" from "present but empty" — the daemon
// only populates the fields relevant to the current status.
type actionApprovalResult struct {
	Status  string  `json:"status"`
	AuditID *string `json:"audit_id,omitempty"`
	Result  *string `json:"result,omitempty"`
	Reason  *string `json:"reason,omitempty"`
	// Failure is the structured ADR-0010 envelope when the daemon
	// surfaces one; decoded as raw JSON so the formatter can echo it
	// without re-implementing the FailureEnvelope shape here.
	Failure json.RawMessage `json:"failure,omitempty"`
}

// --- Server ---

type server struct {
	aileronURL   string
	aileronToken string
	commsURL     string
	sessionID    string
	httpClient   *http.Client
	// commsHTTPClient is a long-poll-tolerant client for the comms
	// endpoints — daemon caps its waits at the action-approval TTL
	// (5 min default). A dedicated client lets the action-discovery
	// path use a tighter timeout without affecting comms.
	commsHTTPClient *http.Client

	// Discovered actions, populated at startup when AILERON_URL is set.
	// Keys of actionNameMap are snake_case (LLM-facing) tool names;
	// values are manifest names (kebab-case) used in /v1/actions/{name}/run.
	actionTools   []toolDef
	actionNameMap map[string]string
}

// commsAvailable reports whether the env carries enough context to
// reach the daemon's comms endpoints. Both env vars must be set; a
// missing session id yields a 404 from the daemon, so fail-loud with
// "comms not available" beats a confusing 404.
func (s *server) commsAvailable() bool {
	return s.commsURL != "" && s.sessionID != ""
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

var checkActionStatusTool = toolDef{
	Name: "check_action_status",
	Description: "Check the status of an approval-gated action call. Call this when an earlier tool returned a `pending_approval` response carrying an approval_id, and you want to know whether the user has approved, denied, or whether the action has finished running. The response carries one of: pending_approval, running, completed (with the result), denied (with the user's reason), failed (with the failure details). Polling is optional — the user knows the next move once they see the approval prompt; this tool exists for agents that want to close the loop on an action they initiated.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"approval_id": {Type: "string", Description: "Approval id returned from the original tool call's pending_approval response."},
		},
		Required: []string{"approval_id"},
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
	// --version / -v / --help / -h: print and exit. The sandbox
	// container's validate step (ADR-0024) execs `aileron-mcp --version`
	// to smoke-check that the host-mounted binary is actually executable
	// inside the container — catches the cross-arch ENOEXEC case
	// `command -v` alone would miss.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Println(version.Version)
			return
		case "--help", "-h":
			fmt.Println("aileron-mcp — Aileron MCP server (stdio JSON-RPC). Usage: aileron-mcp [--version|--help]. Configured via env: AILERON_URL, AILERON_TOKEN, AILERON_SESSION_ID.")
			return
		}
	}

	// Initialize OpenTelemetry. Off by default; AILERON_OTEL_ENABLED
	// opts in. Outbound HTTP calls inject `traceparent` so the
	// daemon's middleware can root spans into the same trace tree
	// regardless of whether the agent (Claude Code, etc.) propagated
	// context across the MCP transport — MCP itself doesn't carry
	// W3C TraceContext today, so this binary is the trace's root in
	// the typical case.
	obsCfg, err := config.LoadObservabilityConfig()
	if err != nil {
		slog.Warn("observability config invalid; tracing disabled", "error", err.Error())
		obsCfg = nil
	}
	tp := observability.Init(obsCfg, slog.Default())
	defer func() { _ = tp.Shutdown(context.Background()) }()

	s := &server{
		aileronURL:   os.Getenv("AILERON_URL"),
		aileronToken: os.Getenv("AILERON_TOKEN"),
		commsURL:     os.Getenv("AILERON_COMMS_URL"),
		sessionID:    os.Getenv("AILERON_SESSION_ID"),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		// 6-minute deadline matches the daemon's 5-minute approval TTL
		// + a small buffer so the daemon's bounded response always
		// wins over a transport timeout.
		commsHTTPClient: &http.Client{Timeout: 6 * time.Minute},
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
	if s.commsAvailable() {
		tools = append(tools, readMessagesTool, draftReplyTool, sendMessageTool, httpRequestTool)
	}
	// Dynamically discovered Aileron actions from the daemon's
	// /v1/actions endpoint (per ADR-0008 — kept in MCP shape rather
	// than the OpenAI/Anthropic shape because the agent host's MCP
	// integration is the consumer here).
	tools = append(tools, s.actionTools...)
	// check_action_status is always available — even when no actions
	// are discovered (a fresh daemon), the agent might be working
	// against an approval id minted by an earlier session.
	tools = append(tools, checkActionStatusTool)
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
	case "check_action_status":
		return s.checkActionStatus(ctx, args)
	default:
		return errorResult("unknown tool: " + name)
	}
}

func (s *server) readMessages(args map[string]any) toolResult {
	if !s.commsAvailable() {
		return errorResult("comms not available (not launched via aileron)")
	}

	service, _ := args["service"].(string)
	channel, _ := args["channel"].(string)

	endpoint := s.commsEndpoint("messages")
	q := url.Values{}
	if service != "" {
		q.Set("service", service)
	}
	if channel != "" {
		q.Set("channel", channel)
	}
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	var resp readMessagesResponse
	if err := s.commsGET(endpoint, &resp); err != nil {
		return errorResult(err.Error())
	}
	return jsonResult(resp.Messages)
}

func (s *server) draftReply(args map[string]any) toolResult {
	if !s.commsAvailable() {
		return errorResult("comms not available (not launched via aileron)")
	}

	messageID, _ := args["message_id"].(string)
	body, _ := args["body"].(string)

	if messageID == "" || body == "" {
		return errorResult("message_id and body are required")
	}

	resp, err := s.commsPOST(s.commsEndpoint("draft"), map[string]string{
		"reply_to": messageID,
		"body":     body,
	})
	if err != nil {
		return errorResult(err.Error())
	}
	if !resp.OK {
		return errorResult(resp.Error)
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: "Draft submitted for user review."}},
	}
}

func (s *server) sendMessage(args map[string]any) toolResult {
	if !s.commsAvailable() {
		return errorResult("comms not available (not launched via aileron)")
	}

	service, _ := args["service"].(string)
	channel, _ := args["channel"].(string)
	body, _ := args["body"].(string)

	if service == "" || channel == "" || body == "" {
		return errorResult("service, channel, and body are required")
	}

	resp, err := s.commsPOST(s.commsEndpoint("send"), map[string]string{
		"service": service,
		"channel": channel,
		"body":    body,
	})
	if err != nil {
		return errorResult(err.Error())
	}
	if !resp.OK {
		return errorResult(resp.Error)
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: "Message sent successfully."}},
	}
}

func (s *server) httpRequest(args map[string]any) toolResult {
	if !s.commsAvailable() {
		return errorResult("comms not available (not launched via aileron)")
	}

	method, _ := args["method"].(string)
	url, _ := args["url"].(string)
	headers, _ := args["headers"].(string)
	body, _ := args["body"].(string)

	if method == "" || url == "" {
		return errorResult("method and url are required")
	}

	payload := map[string]string{
		"method": method,
		"url":    url,
	}
	if body != "" {
		payload["body"] = body
	}
	if headers != "" {
		payload["headers"] = headers
	}

	resp, err := s.commsPOST(s.commsEndpoint("http"), payload)
	if err != nil {
		return errorResult(err.Error())
	}
	if !resp.OK {
		return errorResult(resp.Error)
	}
	// Response body is in the first message's Body field; the daemon
	// stamps the upstream HTTP status code into Id.
	if len(resp.Messages) > 0 {
		return toolResult{
			Content: []toolContent{{Type: "text", Text: resp.Messages[0].Body}},
		}
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: "Request completed (no response body)."}},
	}
}

// --- Comms wire shapes (mirrors internal/api/openapi.yaml CommsToolResponse) ---

type commsToolResponse struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
	Messages []commsMessage `json:"messages,omitempty"`
}

type readMessagesResponse struct {
	Messages []commsMessage `json:"messages"`
}

type commsMessage struct {
	ID           string `json:"id"`
	Service      string `json:"service"`
	Channel      string `json:"channel"`
	Author       string `json:"author"`
	Body         string `json:"body"`
	Timestamp    string `json:"timestamp"`
	DraftRequest bool   `json:"draft_request,omitempty"`
}

// commsEndpoint composes the daemon's per-session comms URL for the
// given suffix ("messages", "send", "draft", "http"). The daemon
// expects `/v1/sessions/{sessionID}/comms/<suffix>`.
func (s *server) commsEndpoint(suffix string) string {
	return strings.TrimRight(s.commsURL, "/") + "/v1/sessions/" + s.sessionID + "/comms/" + suffix
}

// commsGET issues a GET against the daemon's comms surface and decodes
// the JSON body into out. Any non-200 status, transport error, or
// decode failure surfaces as an error so the agent sees the failure
// rather than silently dropping the call.
func (s *server) commsGET(endpoint string, out any) error {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("comms request: %w", err)
	}
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}
	resp, err := s.commsHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// commsPOST issues a POST with the JSON-encoded body and returns the
// parsed CommsToolResponse. The daemon's send-shaped endpoints always
// return 200 — the verdict rides in `ok` + `error` — so a non-200
// here means the call never reached the queue.
func (s *server) commsPOST(endpoint string, body any) (commsToolResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return commsToolResponse{}, fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return commsToolResponse{}, fmt.Errorf("comms request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}
	resp, err := s.commsHTTPClient.Do(req)
	if err != nil {
		return commsToolResponse{}, fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return commsToolResponse{}, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(rawBody)))
	}
	var out commsToolResponse
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return commsToolResponse{}, fmt.Errorf("decoding response: %w", err)
	}
	return out, nil
}

// --- Action discovery / execution against the Aileron daemon ---

// setActionAuthHeaders sets the auth + launch-session headers daemon
// /v1/actions/* and /v1/action-approvals/* endpoints expect. Bearer
// token always; X-Aileron-Session-Id only when a session id is
// configured (host launch with a session, sandbox launch). The header
// name matches the shims surface (internal/sandbox/discovery/tools.go).
// Comms endpoints encode the session id in the path and don't take this
// header.
func (s *server) setActionAuthHeaders(req *http.Request) {
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}
	if s.sessionID != "" {
		req.Header.Set("X-Aileron-Session-Id", s.sessionID)
	}
}

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
	s.setActionAuthHeaders(req)
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
		// Disabled actions are hidden from tools/list so the LLM never
		// learns they exist this session. The MCP server caches at boot,
		// so a re-enable requires a restart to surface the action again
		// — documented behavior, deliberate trade-off vs. polling.
		if !a.isEnabled() {
			continue
		}
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
//
// Emits an `aileron.mcp.tool.call` span around the outbound HTTP call
// and injects W3C TraceContext (`traceparent`) so the daemon's
// middleware extracts the trace and parents its action.execute /
// connector.call spans to this one. With tracing off (the default)
// span operations are no-ops — call shape unchanged.
func (s *server) runAction(ctx context.Context, manifestName string, args map[string]any) toolResult {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "aileron.mcp.tool.call",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("aileron.action.name", manifestName)),
	)
	defer span.End()

	res := s.runActionInner(ctx, manifestName, args)
	if res.IsError {
		span.SetStatus(codes.Error, errorText(res))
	}
	return res
}

// runActionInner is the unwrapped implementation, separated so the
// span lifecycle in [runAction] reads cleanly. Inject traceparent on
// the outbound request via the registered text-map propagator —
// W3C TraceContext is always installed (even when emission is off)
// so this is safe regardless of OTel state.
func (s *server) runActionInner(ctx context.Context, manifestName string, args map[string]any) toolResult {
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
	s.setActionAuthHeaders(req)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorResult("daemon unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult("reading response: " + err.Error())
	}
	switch resp.StatusCode {
	case http.StatusOK:
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
	case http.StatusAccepted:
		// Approval-gated path: the daemon registered a pending entry
		// and returned the agent-facing instruction in `message`. Pass
		// it through verbatim so the LLM surfaces it to the user. Not
		// an error result — the tool call succeeded (we successfully
		// requested approval); the action's outcome is a separate
		// concern reachable via check_action_status.
		var pending actionRunPendingResponse
		if err := json.Unmarshal(rawBody, &pending); err != nil {
			return errorResult("decoding pending response: " + err.Error())
		}
		text := pending.Message
		if text == "" {
			text = "Approval requested. Approval id: " + pending.ApprovalID
		}
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
		}
	default:
		// Non-2xx: surface the FailureEnvelope (or whatever body the
		// daemon returned) so the agent sees the actionable detail.
		return errorResult(string(rawBody))
	}
}

// checkActionStatus implements the check_action_status MCP tool. It
// calls the daemon's `GET /v1/action-approvals/{id}/result` endpoint
// and formats the response as a single text block for the agent.
//
// The response shape varies by status: completed entries return the
// result payload; denied entries return the user's reason; failed
// entries return the failure envelope or executor-error text;
// transient statuses return only the status word. The agent decides
// how to react.
func (s *server) checkActionStatus(ctx context.Context, args map[string]any) toolResult {
	if s.aileronURL == "" {
		return errorResult("Aileron daemon not configured (AILERON_URL not set)")
	}
	approvalID, _ := args["approval_id"].(string)
	if approvalID == "" {
		return errorResult("check_action_status requires approval_id")
	}
	url := s.aileronURL + "/v1/action-approvals/" + approvalID + "/result"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errorResult("creating request: " + err.Error())
	}
	s.setActionAuthHeaders(req)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorResult("daemon unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult("reading response: " + err.Error())
	}
	if resp.StatusCode == http.StatusNotFound {
		return errorResult("approval id not found: " + approvalID +
			" (the approval queue is in-memory; ids from a previous daemon process do not survive restart)")
	}
	if resp.StatusCode != http.StatusOK {
		return errorResult(string(rawBody))
	}
	var result actionApprovalResult
	if err := json.Unmarshal(rawBody, &result); err != nil {
		return errorResult("decoding response: " + err.Error())
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: formatActionApprovalResult(result)}},
	}
}

// formatActionApprovalResult turns the API response into an LLM-
// friendly text block. The status word is always the first line; for
// terminal states the relevant payload follows. Kept terse — the LLM
// is the consumer here, not a human reading logs.
func formatActionApprovalResult(r actionApprovalResult) string {
	switch r.Status {
	case "completed":
		audit := ""
		if r.AuditID != nil && *r.AuditID != "" {
			audit = " (audit_id=" + *r.AuditID + ")"
		}
		result := ""
		if r.Result != nil {
			result = *r.Result
		}
		return "status: completed" + audit + "\nresult: " + result
	case "denied":
		reason := ""
		if r.Reason != nil {
			reason = *r.Reason
		}
		if reason == "" {
			return "status: denied (user did not provide a reason)"
		}
		return "status: denied\nreason: " + reason
	case "failed":
		if r.Failure != nil {
			b, _ := json.Marshal(r.Failure)
			return "status: failed\nfailure: " + string(b)
		}
		reason := ""
		if r.Reason != nil {
			reason = *r.Reason
		}
		return "status: failed\nreason: " + reason
	default:
		// pending_approval, running — terminal statuses are above.
		return "status: " + r.Status
	}
}

// errorText extracts the human-readable error text from a
// toolResult{IsError: true} for use as a span status description.
// Falls back to a generic message if the result is malformed.
func errorText(r toolResult) string {
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			return c.Text
		}
	}
	return "tool error"
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
// appends a notice describing the asynchronous approval contract: the
// tool call returns immediately with a `pending_approval` response
// (carrying the approval id and a verbatim message for the user),
// the action runs server-side after the user approves, and
// check_action_status is available for closing the loop. Tool
// descriptions are part of the MCP system context the LLM factors
// into planning, so this is the natural place for the signal — no
// mid-conversation injection.
func deriveDescription(a actionMeta) string {
	desc := deriveBaseDescription(a)
	if a.requiresApproval() {
		notice := "\n\nThis action requires user approval. Calling it does NOT block: " +
			"the daemon returns a `pending_approval` response with an `approval_id` and a `message` " +
			"naming the review URL and an `aileron open approval <id>` shell alternative. " +
			"Surface the `message` to the user verbatim — do not paraphrase the URL or the command. " +
			"The action runs server-side once the user approves; you may continue with other work. " +
			"Call `check_action_status` with the `approval_id` later if you want to learn the outcome."
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
		prop := schemaProp{
			Type:        in.Type,
			Description: in.Description,
		}
		// Array inputs always emit an `items` clause. When the manifest
		// declares `items_type`, the clause carries the element type;
		// otherwise it is an empty object (any-element). The empty
		// object is strictly more permissive than omitting `items`
		// entirely — strict-defaulting MCP hosts (Codex) treat a
		// missing `items` as `string[]`, which silently breaks
		// object-element arrays.
		if in.Type == "array" {
			prop.Items = &schemaItem{}
			if in.ItemsType != "" {
				prop.Items.Type = in.ItemsType
			}
		}
		s.Properties[in.Name] = prop
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
