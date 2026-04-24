package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestHandle_Initialize(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
	})
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected map result")
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("unexpected protocol version: %v", result["protocolVersion"])
	}
	info, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatal("expected serverInfo map")
	}
	if info["name"] != "aileron" {
		t.Errorf("unexpected server name: %v", info["name"])
	}
}

func TestHandle_NotificationsInitialized(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	if resp != nil {
		t.Fatal("notifications should return nil response")
	}
}

func TestHandle_ToolsList_CloudOnly(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/list",
	})
	if resp == nil || resp.Error != nil {
		t.Fatal("expected successful response")
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected map result")
	}
	tools, ok := result["tools"].([]toolDef)
	if !ok || len(tools) != 5 {
		t.Fatalf("expected 5 tools (submit_intent + 4 write tools), got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"submit_intent", "send_email", "create_calendar_event", "create_github_issue", "comment_on_github_issue"} {
		if !names[want] {
			t.Errorf("expected tool %s in list", want)
		}
	}
}

func TestHandle_ToolsList_WithComms(t *testing.T) {
	s := &server{commsSocket: "/tmp/test.sock", httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/list",
	})
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]toolDef)
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools (read_messages, draft_reply, send_message, http_request), got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if !names["read_messages"] || !names["draft_reply"] || !names["send_message"] || !names["http_request"] {
		t.Errorf("expected read_messages, draft_reply, send_message, and http_request, got %v", names)
	}
}

func TestHandle_ToolsList_Both(t *testing.T) {
	s := &server{aileronURL: "http://localhost", commsSocket: "/tmp/test.sock", httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/list",
	})
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]toolDef)
	// 4 comms tools + 5 cloud tools (submit_intent + 4 write tools) = 9
	if len(tools) != 9 {
		t.Fatalf("expected 9 tools, got %d", len(tools))
	}
}

func TestReadMessages_NoSocket(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.readMessages(map[string]any{})
	if !result.IsError {
		t.Fatal("expected error without comms socket")
	}
}

func TestSendMessage_NoSocket(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.sendMessage(map[string]any{"service": "slack", "channel": "#test", "body": "hi"})
	if !result.IsError {
		t.Fatal("expected error without comms socket")
	}
}

func TestSendMessage_MissingFields(t *testing.T) {
	s := &server{commsSocket: "/tmp/test.sock", httpClient: &http.Client{}}
	result := s.sendMessage(map[string]any{})
	if !result.IsError {
		t.Fatal("expected error for missing fields")
	}
}

func TestRequestComms_NoSocket(t *testing.T) {
	resp := requestComms("/nonexistent/socket.sock", commsRequest{Method: "read_messages"})
	if resp.Error == "" {
		t.Fatal("expected error for missing socket")
	}
}

func TestDispatchTool_ReadMessages(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.dispatchTool(context.Background(), "read_messages", nil)
	if !result.IsError {
		t.Fatal("expected error without comms socket")
	}
}

func TestDispatchTool_SendMessage(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.dispatchTool(context.Background(), "send_message", map[string]any{"service": "slack", "channel": "#x", "body": "y"})
	if !result.IsError {
		t.Fatal("expected error without comms socket")
	}
}

// TestReadMessages_WithServer starts a real CommsServer to test the
// full read_messages path through the Unix socket.
func TestReadMessages_WithServer(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-mcp-test-read.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	// Start a minimal comms server that serves from a mock socket.
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Read request, respond with canned messages.
			var req commsRequest
			json.NewDecoder(conn).Decode(&req)
			json.NewEncoder(conn).Encode(commsResponse{
				OK: true,
				Messages: []commsMessage{
					{ID: "1", Service: "slack", Channel: "#backend", Author: "Alice", Body: "hello"},
				},
			})
			conn.Close()
		}
	}()

	s := &server{commsSocket: socketPath, httpClient: &http.Client{}}
	result := s.readMessages(map[string]any{})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content[0].Text)
	}
	// Result should contain the message data as JSON.
	text := result.Content[0].Text
	if !contains(text, "Alice") || !contains(text, "#backend") {
		t.Errorf("expected Alice and #backend in result, got %s", text)
	}
}

// TestSendMessage_WithServer tests the full send_message path.
// The mock server auto-approves.
func TestSendMessage_WithServer(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-mcp-test-send.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req commsRequest
			json.NewDecoder(conn).Decode(&req)
			json.NewEncoder(conn).Encode(commsResponse{OK: true})
			conn.Close()
		}
	}()

	s := &server{commsSocket: socketPath, httpClient: &http.Client{}}
	result := s.sendMessage(map[string]any{"service": "slack", "channel": "#test", "body": "hello"})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content[0].Text)
	}
	if result.Content[0].Text != "Message sent successfully." {
		t.Errorf("unexpected result: %s", result.Content[0].Text)
	}
}

// TestSendMessage_WithServer_Denied tests the denial path.
func TestSendMessage_WithServer_Denied(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-mcp-test-deny.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req commsRequest
			json.NewDecoder(conn).Decode(&req)
			json.NewEncoder(conn).Encode(commsResponse{Error: "message denied by user"})
			conn.Close()
		}
	}()

	s := &server{commsSocket: socketPath, httpClient: &http.Client{}}
	result := s.sendMessage(map[string]any{"service": "slack", "channel": "#test", "body": "denied"})
	if !result.IsError {
		t.Fatal("expected error for denied message")
	}
}

// TestReadMessages_FilterArgs tests that filter args are passed through.
func TestReadMessages_FilterArgs(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-mcp-test-filter.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	var receivedReq commsRequest
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		json.NewDecoder(conn).Decode(&receivedReq)
		json.NewEncoder(conn).Encode(commsResponse{OK: true})
		conn.Close()
	}()

	s := &server{commsSocket: socketPath, httpClient: &http.Client{}}
	s.readMessages(map[string]any{"service": "discord", "channel": "dev-chat"})

	if receivedReq.Service != "discord" || receivedReq.Channel != "dev-chat" {
		t.Errorf("expected filter args passed through, got service=%q channel=%q", receivedReq.Service, receivedReq.Channel)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestHttpRequest_NoSocket(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.httpRequest(map[string]any{"method": "GET", "url": "https://example.com"})
	if !result.IsError {
		t.Fatal("expected error without comms socket")
	}
}

func TestHttpRequest_MissingFields(t *testing.T) {
	s := &server{commsSocket: "/tmp/test.sock", httpClient: &http.Client{}}
	result := s.httpRequest(map[string]any{})
	if !result.IsError {
		t.Fatal("expected error for missing fields")
	}
}

func TestHttpRequest_WithServer(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-mcp-test-http.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req commsRequest
			json.NewDecoder(conn).Decode(&req)
			json.NewEncoder(conn).Encode(commsResponse{
				OK: true,
				Messages: []commsMessage{
					{ID: "200", Body: `{"result": "ok"}`},
				},
			})
			conn.Close()
		}
	}()

	s := &server{commsSocket: socketPath, httpClient: &http.Client{}}
	result := s.httpRequest(map[string]any{"method": "GET", "url": "https://api.example.com/test"})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content[0].Text)
	}
	if !contains(result.Content[0].Text, "ok") {
		t.Errorf("expected response body, got %s", result.Content[0].Text)
	}
}

func TestHttpRequest_WithServer_NoBody(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-mcp-test-http-nobody.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req commsRequest
			json.NewDecoder(conn).Decode(&req)
			json.NewEncoder(conn).Encode(commsResponse{OK: true})
			conn.Close()
		}
	}()

	s := &server{commsSocket: socketPath, httpClient: &http.Client{}}
	result := s.httpRequest(map[string]any{"method": "GET", "url": "https://example.com"})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content[0].Text)
	}
	if !contains(result.Content[0].Text, "no response body") {
		t.Errorf("expected 'no response body' message, got %s", result.Content[0].Text)
	}
}

func TestDispatchTool_HttpRequest(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.dispatchTool(context.Background(), "http_request", map[string]any{"method": "GET", "url": "https://example.com"})
	if !result.IsError {
		t.Fatal("expected error without comms socket")
	}
}

func TestHandle_Ping(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`3`),
		Method:  "ping",
	})
	if resp == nil || resp.Error != nil {
		t.Fatal("expected successful response")
	}
}

func TestHandle_UnknownMethod(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`4`),
		Method:  "unknown/method",
	})
	if resp == nil {
		t.Fatal("expected error response")
	}
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected method not found code -32601, got %d", resp.Error.Code)
	}
}

func TestHandle_ToolsCall_InvalidParams(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`5`),
		Method:  "tools/call",
		Params:  json.RawMessage(`not-json`),
	})
	if resp == nil || resp.Error == nil {
		t.Fatal("expected error response for invalid params")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("expected invalid params code -32602, got %d", resp.Error.Code)
	}
}

func TestDispatchTool_UnknownTool(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	result := s.dispatchTool(context.Background(), "nonexistent", nil)
	if !result.IsError {
		t.Fatal("expected error result for unknown tool")
	}
	if result.Content[0].Text != "unknown tool: nonexistent" {
		t.Errorf("unexpected error text: %s", result.Content[0].Text)
	}
}

func TestSubmitIntent_MissingIntent(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	result := s.submitIntent(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error for missing intent")
	}
	if result.Content[0].Text != "intent parameter is required" {
		t.Errorf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestSubmitIntent_EmptyIntent(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	result := s.submitIntent(context.Background(), map[string]any{"intent": ""})
	if !result.IsError {
		t.Fatal("expected error for empty intent")
	}
}

func TestSubmitIntent_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/intents" {
			t.Errorf("expected /v1/intents, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json content type")
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		action, _ := body["action"].(map[string]any)
		if action["summary"] != "deploy my app" {
			t.Errorf("unexpected intent text: %v", action["summary"])
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "int_123",
			"status": "pending_policy",
		})
	}))
	defer ts.Close()

	s := &server{
		aileronURL:   ts.URL,
		aileronToken: "test-token",
		httpClient:   ts.Client(),
	}

	result := s.submitIntent(context.Background(), map[string]any{
		"intent": "deploy my app",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatal("expected single text content")
	}
}

func TestSubmitIntent_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "internal", "message": "something broke"},
		})
	}))
	defer ts.Close()

	s := &server{
		aileronURL: ts.URL,
		httpClient: ts.Client(),
	}

	result := s.submitIntent(context.Background(), map[string]any{
		"intent": "deploy my app",
	})
	// The server returns a JSON body even on error, so submitIntent decodes it
	// (it doesn't check status codes). The result is not flagged as an error
	// at the MCP level — the API error is in the response body.
	if result.IsError {
		t.Fatal("expected non-error result (API error is in the body)")
	}
}

func TestSubmitIntent_NoToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("expected no Authorization header when token is empty")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "int_456"})
	}))
	defer ts.Close()

	s := &server{
		aileronURL:   ts.URL,
		aileronToken: "",
		httpClient:   ts.Client(),
	}

	result := s.submitIntent(context.Background(), map[string]any{
		"intent": "test without token",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
}

func TestHandle_ToolsCall_SubmitIntent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "int_789"})
	}))
	defer ts.Close()

	s := &server{
		aileronURL: ts.URL,
		httpClient: ts.Client(),
	}

	params, _ := json.Marshal(callToolParams{
		Name:      "submit_intent",
		Arguments: map[string]any{"intent": "do something"},
	})
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`6`),
		Method:  "tools/call",
		Params:  json.RawMessage(params),
	})
	if resp == nil || resp.Error != nil {
		t.Fatal("expected successful response")
	}
	result, ok := resp.Result.(toolResult)
	if !ok {
		t.Fatal("expected toolResult")
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content[0].Text)
	}
}

func TestErrorResult(t *testing.T) {
	r := errorResult("something went wrong")
	if !r.IsError {
		t.Fatal("expected IsError=true")
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatal("expected single text content")
	}
	if r.Content[0].Text != "something went wrong" {
		t.Errorf("unexpected text: %s", r.Content[0].Text)
	}
}

func TestJsonResult(t *testing.T) {
	r := jsonResult(map[string]string{"key": "value"})
	if r.IsError {
		t.Fatal("expected IsError=false")
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatal("expected single text content")
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(r.Content[0].Text), &parsed); err != nil {
		t.Fatalf("result should be valid JSON: %v", err)
	}
	if parsed["key"] != "value" {
		t.Errorf("unexpected parsed value: %v", parsed)
	}
}

func TestDraftReply_NoSocket(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.draftReply(map[string]any{"message_id": "1", "body": "text"})
	if !result.IsError {
		t.Error("expected error when comms socket not set")
	}
}

func TestDraftReply_MissingFields(t *testing.T) {
	s := &server{commsSocket: "/tmp/fake.sock", httpClient: &http.Client{}}
	result := s.draftReply(map[string]any{})
	if !result.IsError {
		t.Error("expected error for missing fields")
	}
}

// --- Write tool tests ---

func TestSendEmail_MissingFields(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	tests := []struct {
		name string
		args map[string]any
	}{
		{"empty", map[string]any{}},
		{"missing subject", map[string]any{"to": "a@b.com", "body_text": "hi"}},
		{"missing to", map[string]any{"subject": "hi", "body_text": "hi"}},
		{"missing body", map[string]any{"to": "a@b.com", "subject": "hi"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := s.sendEmail(context.Background(), tt.args)
			if !result.IsError {
				t.Fatal("expected error for missing fields")
			}
		})
	}
}

func TestSendEmail_Success(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "int_email_1", "status": "pending_approval"})
	}))
	defer ts.Close()

	s := &server{aileronURL: ts.URL, aileronToken: "tok", httpClient: ts.Client()}
	result := s.sendEmail(context.Background(), map[string]any{
		"to":        "alice@example.com, bob@example.com",
		"cc":        "carol@example.com",
		"subject":   "Weekly sync",
		"body_text": "See you Thursday",
		"send_mode": "draft_only",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	action, _ := receivedBody["action"].(map[string]any)
	if action["type"] != "email.draft" {
		t.Errorf("expected email.draft action type for draft_only, got %v", action["type"])
	}
	domain, _ := action["domain"].(map[string]any)
	email, _ := domain["email"].(map[string]any)
	to, _ := email["to"].([]any)
	if len(to) != 2 {
		t.Errorf("expected 2 recipients, got %d", len(to))
	}
}

func TestSendEmail_NoURL(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.sendEmail(context.Background(), map[string]any{
		"to": "a@b.com", "subject": "hi", "body_text": "hello",
	})
	if !result.IsError {
		t.Fatal("expected error without AILERON_URL")
	}
}

func TestCreateCalendarEvent_MissingFields(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	tests := []struct {
		name string
		args map[string]any
	}{
		{"empty", map[string]any{}},
		{"missing times", map[string]any{"title": "Standup"}},
		{"missing title", map[string]any{"start_time": "2025-03-15T10:00:00Z", "end_time": "2025-03-15T11:00:00Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := s.createCalendarEvent(context.Background(), tt.args)
			if !result.IsError {
				t.Fatal("expected error for missing fields")
			}
		})
	}
}

func TestCreateCalendarEvent_Success(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "int_cal_1", "status": "pending_approval"})
	}))
	defer ts.Close()

	s := &server{aileronURL: ts.URL, httpClient: ts.Client()}
	result := s.createCalendarEvent(context.Background(), map[string]any{
		"title":           "Standup",
		"start_time":      "2025-03-15T10:00:00Z",
		"end_time":        "2025-03-15T10:30:00Z",
		"attendees":       "alice@example.com, bob@example.com",
		"conference_type": "google_meet",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	action, _ := receivedBody["action"].(map[string]any)
	if action["type"] != "calendar.event.create" {
		t.Errorf("expected calendar.event.create, got %v", action["type"])
	}
	domain, _ := action["domain"].(map[string]any)
	cal, _ := domain["calendar"].(map[string]any)
	attendees, _ := cal["attendees"].([]any)
	if len(attendees) != 2 {
		t.Errorf("expected 2 attendees, got %d", len(attendees))
	}
}

func TestCreateGithubIssue_MissingFields(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	tests := []struct {
		name string
		args map[string]any
	}{
		{"empty", map[string]any{}},
		{"missing title", map[string]any{"repository": "acme/app"}},
		{"missing repo", map[string]any{"title": "Bug report"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := s.createGithubIssue(context.Background(), tt.args)
			if !result.IsError {
				t.Fatal("expected error for missing fields")
			}
		})
	}
}

func TestCreateGithubIssue_Success(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "int_gh_1", "status": "pending_approval"})
	}))
	defer ts.Close()

	s := &server{aileronURL: ts.URL, httpClient: ts.Client()}
	result := s.createGithubIssue(context.Background(), map[string]any{
		"repository": "acme/backend",
		"title":      "Fix login bug",
		"body":       "Users can't log in after password reset",
		"labels":     "bug, priority:high",
		"assignees":  "alice",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	action, _ := receivedBody["action"].(map[string]any)
	if action["type"] != "git.issue.create" {
		t.Errorf("expected git.issue.create, got %v", action["type"])
	}
	domain, _ := action["domain"].(map[string]any)
	git, _ := domain["git"].(map[string]any)
	labels, _ := git["issue_labels"].([]any)
	if len(labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(labels))
	}
}

func TestCommentOnGithubIssue_MissingFields(t *testing.T) {
	s := &server{aileronURL: "http://localhost", httpClient: &http.Client{}}
	tests := []struct {
		name string
		args map[string]any
	}{
		{"empty", map[string]any{}},
		{"missing body", map[string]any{"repository": "acme/app", "issue_number": "42"}},
		{"missing issue_number", map[string]any{"repository": "acme/app", "body": "LGTM"}},
		{"missing repo", map[string]any{"issue_number": "42", "body": "LGTM"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := s.commentOnGithubIssue(context.Background(), tt.args)
			if !result.IsError {
				t.Fatal("expected error for missing fields")
			}
		})
	}
}

func TestCommentOnGithubIssue_Success(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "int_ghc_1", "status": "pending_approval"})
	}))
	defer ts.Close()

	s := &server{aileronURL: ts.URL, httpClient: ts.Client()}
	result := s.commentOnGithubIssue(context.Background(), map[string]any{
		"repository":   "acme/backend",
		"issue_number": "123",
		"body":         "This is fixed in PR #456",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}

	action, _ := receivedBody["action"].(map[string]any)
	if action["type"] != "git.issue.comment" {
		t.Errorf("expected git.issue.comment, got %v", action["type"])
	}
}

func TestDispatchTool_WriteTools(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	tools := []string{"send_email", "create_calendar_event", "create_github_issue", "comment_on_github_issue"}
	for _, tool := range tools {
		t.Run(tool, func(t *testing.T) {
			result := s.dispatchTool(context.Background(), tool, map[string]any{})
			if !result.IsError {
				t.Fatal("expected error (missing required fields or no AILERON_URL)")
			}
		})
	}
}

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"a", 1},
		{"a, b, c", 3},
		{"  a , b ,, c ", 3},
	}
	for _, tt := range tests {
		got := splitCSV(tt.input)
		if len(got) != tt.want {
			t.Errorf("splitCSV(%q) = %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestSplitRecipients(t *testing.T) {
	got := splitRecipients("alice@example.com, bob@example.com")
	if len(got) != 2 {
		t.Fatalf("expected 2 recipients, got %d", len(got))
	}
	if got[0]["email"] != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %s", got[0]["email"])
	}
	if got[1]["email"] != "bob@example.com" {
		t.Errorf("expected bob@example.com, got %s", got[1]["email"])
	}

	empty := splitRecipients("")
	if empty != nil {
		t.Errorf("expected nil for empty input, got %v", empty)
	}
}

func TestDraftReply_WithServer(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-mcp-test-draft.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req commsRequest
			json.NewDecoder(conn).Decode(&req)
			if req.Method == "draft_reply" && req.ReplyTo != "" && req.Body != "" {
				json.NewEncoder(conn).Encode(commsResponse{OK: true})
			} else {
				json.NewEncoder(conn).Encode(commsResponse{Error: "bad request"})
			}
			conn.Close()
		}
	}()

	s := &server{commsSocket: socketPath, httpClient: &http.Client{}}
	result := s.draftReply(map[string]any{"message_id": "msg-1", "body": "my draft"})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content[0].Text)
	}
	if !contains(result.Content[0].Text, "Draft submitted") {
		t.Errorf("expected success message, got %s", result.Content[0].Text)
	}
}

func TestDispatchTool_DraftReply(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.dispatchTool(context.Background(), "draft_reply", map[string]any{"message_id": "1", "body": "text"})
	// No comms socket → error, but should not panic.
	if !result.IsError {
		t.Error("expected error without comms socket")
	}
}

func TestErrorResponse(t *testing.T) {
	resp := errorResponse(json.RawMessage(`99`), -32600, "invalid request")
	if resp.JSONRPC != "2.0" {
		t.Errorf("expected JSONRPC 2.0")
	}
	if resp.Error == nil || resp.Error.Code != -32600 || resp.Error.Message != "invalid request" {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
}
