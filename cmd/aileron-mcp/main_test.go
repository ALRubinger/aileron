package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// --- JSON-RPC handling ---

func TestHandle_Initialize(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
	})
	if resp == nil || resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected map result")
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
}

func TestHandle_NotificationsInitialized(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{Method: "notifications/initialized"})
	if resp != nil {
		t.Errorf("expected nil response for notification, got %+v", resp)
	}
}

func TestHandle_Ping(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{Method: "ping", ID: json.RawMessage(`1`)})
	if resp == nil || resp.Error != nil {
		t.Fatalf("ping should succeed: %+v", resp)
	}
}

func TestHandle_UnknownMethod(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{Method: "no-such-method", ID: json.RawMessage(`1`)})
	if resp == nil || resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("code = %d, want -32601", resp.Error.Code)
	}
}

func TestHandle_ToolsCall_InvalidParams(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	resp := s.handle(jsonrpcRequest{
		Method: "tools/call",
		ID:     json.RawMessage(`1`),
		Params: json.RawMessage(`not-json`),
	})
	if resp == nil || resp.Error == nil {
		t.Fatal("expected error for invalid params")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602", resp.Error.Code)
	}
}

func TestDispatchTool_UnknownTool(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	result := s.dispatchTool(context.Background(), "nonexistent", nil)
	if !result.IsError {
		t.Fatal("expected error for unknown tool")
	}
}

// --- Tool listing ---

func TestAvailableTools_CommsOnly(t *testing.T) {
	s := &server{commsURL:    "http://x", sessionID: "sess-x", httpClient: &http.Client{}}
	tools := s.availableTools()
	if len(tools) != 4 {
		t.Fatalf("expected 4 comms tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, td := range tools {
		names[td.Name] = true
	}
	for _, want := range []string{"read_messages", "draft_reply", "send_message", "http_request"} {
		if !names[want] {
			t.Errorf("missing tool: %s", want)
		}
	}
}

func TestAvailableTools_ActionsOnly(t *testing.T) {
	s := &server{
		httpClient: &http.Client{},
		actionTools: []toolDef{
			{Name: "ship_update", Description: "Posts a ship update.", InputSchema: schema{Type: "object"}},
		},
	}
	tools := s.availableTools()
	if len(tools) != 1 {
		t.Fatalf("expected 1 action tool, got %d", len(tools))
	}
	if tools[0].Name != "ship_update" {
		t.Errorf("got %q", tools[0].Name)
	}
}

func TestAvailableTools_CommsAndActions(t *testing.T) {
	s := &server{
		commsURL:    "http://x", sessionID: "sess-x",
		httpClient:  &http.Client{},
		actionTools: []toolDef{
			{Name: "ship_update", Description: "x", InputSchema: schema{Type: "object"}},
			{Name: "list_emails", Description: "y", InputSchema: schema{Type: "object"}},
		},
	}
	tools := s.availableTools()
	if len(tools) != 6 {
		t.Fatalf("expected 4 comms + 2 action = 6 tools, got %d", len(tools))
	}
}

// --- Pure helpers (mirror augment.Derive logic from internal/augment) ---

func TestToolName_KebabToSnake(t *testing.T) {
	if got := toolName("ship-update"); got != "ship_update" {
		t.Errorf("got %q, want ship_update", got)
	}
	if got := toolName("already_snake"); got != "already_snake" {
		t.Errorf("got %q, want already_snake", got)
	}
}

func TestDeriveDescription_StripsLeadingHeading(t *testing.T) {
	a := actionMeta{Body: "# Ship Update\n\nPosts a 'shipped' announcement to a Slack channel."}
	got := deriveDescription(a)
	want := "Posts a 'shipped' announcement to a Slack channel."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeriveDescription_EmptyBodyFallsBackToIntent(t *testing.T) {
	a := actionMeta{Match: &actionMatch{Intent: "tell team I shipped"}}
	if got := deriveDescription(a); got != "tell team I shipped" {
		t.Errorf("got %q", got)
	}
}

func TestDeriveDescription_NoHeadingReturnsBody(t *testing.T) {
	a := actionMeta{Body: "Just a body without a heading."}
	if got := deriveDescription(a); got != "Just a body without a heading." {
		t.Errorf("got %q", got)
	}
}

func TestDeriveDescription_EmptyBodyAndNilMatchReturnsEmpty(t *testing.T) {
	if got := deriveDescription(actionMeta{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDeriveDescription_HeadingOnlyFallsBackToIntent(t *testing.T) {
	a := actionMeta{Body: "# Title only", Match: &actionMatch{Intent: "do the thing"}}
	if got := deriveDescription(a); got != "do the thing" {
		t.Errorf("got %q", got)
	}
}

func TestDeriveDescription_HeadingOnlyAndNilMatchReturnsEmpty(t *testing.T) {
	a := actionMeta{Body: "# Title only"}
	if got := deriveDescription(a); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestDeriveDescription_AppendsApprovalNoticeWhenRequired asserts that
// when an action's manifest declares `[approval] required = true`, the
// derived MCP tool description ends with a notice naming the approval
// URL. The agent reads this as part of MCP tool discovery and surfaces
// it to the user when invoking the tool.
func TestDeriveDescription_AppendsApprovalNoticeWhenRequired(t *testing.T) {
	t.Setenv("AILERON_APPROVAL_URL", "http://127.0.0.1:54321/approvals")
	required := true
	a := actionMeta{
		Body:     "# Send Email\n\nSends a Gmail message.",
		Approval: &actionApprovalPolicy{Required: &required},
	}
	got := deriveDescription(a)
	if !strings.Contains(got, "Sends a Gmail message.") {
		t.Errorf("base description lost; got %q", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:54321/approvals") {
		t.Errorf("approval URL not present in description; got %q", got)
	}
	if !strings.Contains(got, "requires user approval") {
		t.Errorf("approval notice phrasing missing; got %q", got)
	}
}

// TestDeriveDescription_NoNoticeWhenApprovalNotRequired asserts that
// actions without `[approval] required = true` get the unmodified
// description — the legacy path is unchanged.
func TestDeriveDescription_NoNoticeWhenApprovalNotRequired(t *testing.T) {
	t.Setenv("AILERON_APPROVAL_URL", "http://127.0.0.1:54321/approvals")
	a := actionMeta{
		Body: "# Read Mail\n\nLists recent inbox messages.",
	}
	got := deriveDescription(a)
	if strings.Contains(got, "approval") {
		t.Errorf("unrequested approval notice in description; got %q", got)
	}
	if got != "Lists recent inbox messages." {
		t.Errorf("base description altered; got %q", got)
	}
}

// TestDeriveDescription_FallbackPhrasingWhenURLUnset asserts that when
// AILERON_APPROVAL_URL is not set (e.g. running aileron-mcp standalone
// without launch), the notice still fires but uses generic "the Aileron
// webapp" wording rather than dropping the warning entirely. Better to
// keep the agent informed even without an actionable URL.
func TestDeriveDescription_FallbackPhrasingWhenURLUnset(t *testing.T) {
	t.Setenv("AILERON_APPROVAL_URL", "")
	required := true
	a := actionMeta{
		Body:     "# Send Email\n\nSends a Gmail message.",
		Approval: &actionApprovalPolicy{Required: &required},
	}
	got := deriveDescription(a)
	if !strings.Contains(got, "the Aileron webapp") {
		t.Errorf("fallback phrasing missing; got %q", got)
	}
	if strings.Contains(got, "http://") {
		t.Errorf("unexpected URL in fallback path; got %q", got)
	}
}

func TestDeriveInputSchema_NoInputs(t *testing.T) {
	got := deriveInputSchema(actionMeta{})
	want := schema{Type: "object"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDeriveInputSchema_WithRequiredAndOptional(t *testing.T) {
	required := false
	a := actionMeta{
		Inputs: []actionInput{
			{Name: "channel", Type: "string", Description: "Slack channel name"},
			{Name: "preview", Type: "boolean", Required: &required, Description: "Skip send"},
		},
	}
	got := deriveInputSchema(a)

	if got.Type != "object" {
		t.Errorf("type = %q", got.Type)
	}
	if len(got.Properties) != 2 {
		t.Errorf("properties = %d", len(got.Properties))
	}
	if got.Properties["channel"].Type != "string" {
		t.Errorf("channel type = %q", got.Properties["channel"].Type)
	}
	// channel: Required omitted → defaults to true, included in Required.
	// preview: Required explicitly false → not in Required.
	wantReq := []string{"channel"}
	if !reflect.DeepEqual(got.Required, wantReq) {
		t.Errorf("required = %v, want %v", got.Required, wantReq)
	}
}

func TestActionToolDef(t *testing.T) {
	a := actionMeta{
		Name: "ship-update",
		Body: "# Ship Update\n\nPosts a Slack ship-update.",
		Inputs: []actionInput{
			{Name: "channel", Type: "string", Description: "Slack channel"},
		},
	}
	td := actionToolDef(a)
	if td.Name != "ship_update" {
		t.Errorf("name = %q", td.Name)
	}
	if td.Description != "Posts a Slack ship-update." {
		t.Errorf("description = %q", td.Description)
	}
	if td.InputSchema.Properties["channel"].Type != "string" {
		t.Error("channel schema missing or wrong type")
	}
}

// --- Action discovery ---

func TestDiscoverActions_NoURL(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	tools, nameMap, err := s.discoverActions(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if tools != nil || nameMap != nil {
		t.Errorf("expected nil/nil for no URL, got %+v / %+v", tools, nameMap)
	}
}

func TestDiscoverActions_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/actions" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(actionListResponse{
			Items: []actionMeta{
				{
					Name: "ship-update",
					Body: "# Ship Update\n\nPost a ship-update.",
					Inputs: []actionInput{
						{Name: "channel", Type: "string", Description: "channel"},
					},
				},
				{
					Name:  "list-emails",
					Body:  "List recent emails.",
					Match: &actionMatch{Intent: "show me recent emails"},
				},
			},
		})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	tools, nameMap, err := s.discoverActions(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d", len(tools))
	}
	if nameMap["ship_update"] != "ship-update" {
		t.Errorf("ship_update map: %q", nameMap["ship_update"])
	}
	if nameMap["list_emails"] != "list-emails" {
		t.Errorf("list_emails map: %q", nameMap["list_emails"])
	}
}

func TestDiscoverActions_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	if _, _, err := s.discoverActions(context.Background()); err == nil {
		t.Fatal("expected error for 500")
	}
}

// --- Action execution ---

func TestRunAction_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/actions/ship-update/run" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req actionRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Args["channel"] != "#engineering" {
			t.Errorf("args.channel = %v", req.Args["channel"])
		}
		content := "Posted to #engineering"
		_ = json.NewEncoder(w).Encode(actionRunResponse{
			AuditID: "audit_abc",
			Result:  &content,
		})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.runAction(context.Background(), "ship-update", map[string]any{"channel": "#engineering"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "Posted to #engineering") {
		t.Errorf("content = %q", got.Content[0].Text)
	}
}

func TestRunAction_NoURL(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	got := s.runAction(context.Background(), "ship-update", nil)
	if !got.IsError {
		t.Fatal("expected error when AILERON_URL not set")
	}
}

func TestRunAction_HTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error":{"class":"binding_required","message":"vault is locked"}}`))
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.runAction(context.Background(), "ship-update", nil)
	if !got.IsError {
		t.Fatal("expected error for 412")
	}
	if !strings.Contains(got.Content[0].Text, "binding_required") {
		t.Errorf("expected envelope passed through, got %q", got.Content[0].Text)
	}
}

func TestRunAction_DaemonUnreachable(t *testing.T) {
	s := &server{
		aileronURL: "http://127.0.0.1:1", // unreachable port
		httpClient: &http.Client{},
	}
	got := s.runAction(context.Background(), "ship-update", nil)
	if !got.IsError {
		t.Fatal("expected error when daemon unreachable")
	}
	if !strings.Contains(got.Content[0].Text, "daemon unreachable") {
		t.Errorf("expected 'daemon unreachable' prefix, got %q", got.Content[0].Text)
	}
}

func TestRunAction_DecodeFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not-json`))
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.runAction(context.Background(), "x", nil)
	if !got.IsError {
		t.Fatal("expected error for bad response JSON")
	}
}

// --- Dispatch routing ---

func TestDispatchTool_RoutesActionByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/actions/ship-update/run" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		content := "ok"
		_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "audit_x", Result: &content})
	}))
	defer srv.Close()

	s := &server{
		aileronURL:    srv.URL,
		httpClient:    srv.Client(),
		actionNameMap: map[string]string{"ship_update": "ship-update"},
	}
	got := s.dispatchTool(context.Background(), "ship_update", map[string]any{"channel": "#x"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
}

// --- Comms tools (preserve essential coverage) ---

func TestReadMessages_NoSocket(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	if !s.readMessages(map[string]any{}).IsError {
		t.Fatal("expected error without comms socket")
	}
}

func TestSendMessage_NoSocket(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	if !s.sendMessage(map[string]any{"service": "slack", "channel": "#x", "body": "hi"}).IsError {
		t.Fatal("expected error without comms socket")
	}
}

func TestSendMessage_MissingFields(t *testing.T) {
	s := commsServerWithFakeDaemon(t, nil)
	if !s.sendMessage(map[string]any{}).IsError {
		t.Fatal("expected error for missing fields")
	}
}

func TestDraftReply_NoCommsURL(t *testing.T) {
	s := &server{httpClient: &http.Client{}, commsHTTPClient: &http.Client{}}
	if !s.draftReply(map[string]any{"message_id": "1", "body": "hi"}).IsError {
		t.Fatal("expected error without comms URL + session ID")
	}
}

func TestDraftReply_MissingFields(t *testing.T) {
	s := commsServerWithFakeDaemon(t, nil)
	if !s.draftReply(map[string]any{}).IsError {
		t.Fatal("expected error for missing fields")
	}
}

func TestHttpRequest_NoCommsURL(t *testing.T) {
	s := &server{httpClient: &http.Client{}, commsHTTPClient: &http.Client{}}
	if !s.httpRequest(map[string]any{"method": "GET", "url": "https://x"}).IsError {
		t.Fatal("expected error without comms URL + session ID")
	}
}

func TestHttpRequest_MissingFields(t *testing.T) {
	s := commsServerWithFakeDaemon(t, nil)
	if !s.httpRequest(map[string]any{}).IsError {
		t.Fatal("expected error for missing fields")
	}
}

// --- Comms tools — integration against a fake daemon HTTP server ---

func TestReadMessages_WithDaemon(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/comms/messages") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(readMessagesResponse{
			Messages: []commsMessage{
				{ID: "1", Service: "slack", Channel: "#dev", Author: "alice", Body: "hello"},
			},
		})
	}))
	got := s.readMessages(map[string]any{})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "hello") {
		t.Errorf("expected message body in result, got %q", got.Content[0].Text)
	}
}

func TestReadMessages_PassesQueryFilters(t *testing.T) {
	got := make(chan string, 1)
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(readMessagesResponse{Messages: []commsMessage{}})
	}))
	_ = s.readMessages(map[string]any{"service": "slack", "channel": "#dev"})
	q := <-got
	if !strings.Contains(q, "service=slack") || !strings.Contains(q, "channel=") {
		t.Errorf("query = %q, want service=slack and channel filter", q)
	}
}

func TestSendMessage_WithDaemon(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/comms/send") {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["service"] != "slack" || body["channel"] != "#x" || body["body"] != "hi" {
			t.Errorf("body = %v", body)
		}
		_ = json.NewEncoder(w).Encode(commsToolResponse{OK: true})
	}))
	got := s.sendMessage(map[string]any{"service": "slack", "channel": "#x", "body": "hi"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
}

func TestSendMessage_DaemonDenies(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(commsToolResponse{OK: false, Error: "user denied"})
	}))
	got := s.sendMessage(map[string]any{"service": "slack", "channel": "#x", "body": "hi"})
	if !got.IsError {
		t.Fatal("expected error when daemon denies send")
	}
	if !strings.Contains(got.Content[0].Text, "user denied") {
		t.Errorf("expected daemon's error text in result, got %q", got.Content[0].Text)
	}
}

func TestDraftReply_WithDaemon(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/comms/draft") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(commsToolResponse{OK: true})
	}))
	got := s.draftReply(map[string]any{"message_id": "1", "body": "hi"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
}

func TestDraftReply_DaemonDenies(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(commsToolResponse{OK: false, Error: "discarded"})
	}))
	got := s.draftReply(map[string]any{"message_id": "1", "body": "hi"})
	if !got.IsError {
		t.Fatal("expected error when daemon discards draft")
	}
}

func TestHttpRequest_WithDaemon(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/comms/http") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(commsToolResponse{
			OK: true,
			Messages: []commsMessage{
				{ID: "200", Body: "{\"ok\":true}"},
			},
		})
	}))
	got := s.httpRequest(map[string]any{"method": "GET", "url": "https://example.com"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "ok") {
		t.Errorf("expected response body in result, got %q", got.Content[0].Text)
	}
}

func TestHttpRequest_DaemonDenies(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(commsToolResponse{OK: false, Error: "url not in allowlist"})
	}))
	got := s.httpRequest(map[string]any{"method": "GET", "url": "https://blocked"})
	if !got.IsError {
		t.Fatal("expected error when daemon denies")
	}
}

func TestDispatchTool_RoutesAllCommsTools(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /comms/messages is GET; the rest are POST.
		if strings.HasSuffix(r.URL.Path, "/comms/messages") {
			_ = json.NewEncoder(w).Encode(readMessagesResponse{Messages: []commsMessage{}})
			return
		}
		_ = json.NewEncoder(w).Encode(commsToolResponse{
			OK:       true,
			Messages: []commsMessage{{ID: "x", Body: "ok"}},
		})
	}))
	cases := []struct {
		name string
		args map[string]any
	}{
		{"read_messages", map[string]any{}},
		{"draft_reply", map[string]any{"message_id": "1", "body": "hi"}},
		{"send_message", map[string]any{"service": "slack", "channel": "#x", "body": "hi"}},
		{"http_request", map[string]any{"method": "GET", "url": "https://x"}},
	}
	for _, tc := range cases {
		got := s.dispatchTool(context.Background(), tc.name, tc.args)
		if got.IsError {
			t.Errorf("%s: unexpected error: %s", tc.name, got.Content[0].Text)
		}
	}
}

func TestHandle_ToolsList_ReturnsTools(t *testing.T) {
	s := &server{
		commsURL:        "http://127.0.0.1:1",
		sessionID:       "sess-x",
		httpClient:      &http.Client{},
		commsHTTPClient: &http.Client{},
	}
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/list",
	})
	if resp == nil || resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp)
	}
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]toolDef)
	if len(tools) != 4 {
		t.Errorf("expected 4 comms tools, got %d", len(tools))
	}
}

func TestHandle_ToolsCall_RoutesToTool(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(readMessagesResponse{Messages: []commsMessage{}})
	}))
	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"read_messages","arguments":{}}`),
	})
	if resp == nil || resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp)
	}
}

func TestCommsAvailable_RequiresBothEnvVars(t *testing.T) {
	cases := []struct {
		name string
		s    *server
		want bool
	}{
		{"both set", &server{commsURL: "http://x", sessionID: "s"}, true},
		{"missing url", &server{sessionID: "s"}, false},
		{"missing session", &server{commsURL: "http://x"}, false},
		{"both missing", &server{}, false},
	}
	for _, tc := range cases {
		if got := tc.s.commsAvailable(); got != tc.want {
			t.Errorf("%s: commsAvailable() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- Helpers ---

func TestErrorResult(t *testing.T) {
	r := errorResult("oops")
	if !r.IsError || r.Content[0].Text != "oops" {
		t.Errorf("got %+v", r)
	}
}

func TestJsonResult(t *testing.T) {
	r := jsonResult(map[string]any{"k": "v"})
	if r.IsError {
		t.Fatal("unexpected error")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &got); err != nil {
		t.Fatal(err)
	}
	if got["k"] != "v" {
		t.Errorf("k = %v", got["k"])
	}
}

// --- Test infra ---

// commsServerWithFakeDaemon returns a *server pointed at an httptest
// server that runs the supplied handler. nil handler yields a server
// configured with comms env but no upstream — useful for missing-field
// tests where the request never reaches the wire.
func commsServerWithFakeDaemon(t *testing.T, handler http.Handler) *server {
	t.Helper()
	url := "http://127.0.0.1:1" // unreachable; sentinel for "env set, no daemon"
	if handler != nil {
		ts := httptest.NewServer(handler)
		t.Cleanup(ts.Close)
		url = ts.URL
	}
	return &server{
		commsURL:        url,
		sessionID:       "sess-x",
		httpClient:      &http.Client{},
		commsHTTPClient: &http.Client{},
	}
}
