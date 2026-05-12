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

// TestAvailableTools_* lists also include `check_action_status`,
// which the MCP server registers unconditionally so the agent can
// poll outcomes from any session.
func TestAvailableTools_CommsOnly(t *testing.T) {
	s := &server{commsURL:    "http://x", sessionID: "sess-x", httpClient: &http.Client{}}
	tools := s.availableTools()
	if len(tools) != 5 {
		t.Fatalf("expected 4 comms tools + check_action_status, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, td := range tools {
		names[td.Name] = true
	}
	for _, want := range []string{"read_messages", "draft_reply", "send_message", "http_request", "check_action_status"} {
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
	if len(tools) != 2 {
		t.Fatalf("expected 1 action tool + check_action_status, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, td := range tools {
		names[td.Name] = true
	}
	if !names["ship_update"] || !names["check_action_status"] {
		t.Errorf("tools = %v, want ship_update + check_action_status", names)
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
	if len(tools) != 7 {
		t.Fatalf("expected 4 comms + 2 action + check_action_status = 7 tools, got %d", len(tools))
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
// derived MCP tool description ends with a notice describing the
// async-return contract: the call does NOT block, the agent surfaces
// the daemon's verbatim `message` (which itself carries the URL and
// the `aileron open approval <id>` shell alternative), and
// check_action_status is the polling tool. The URL is supplied per-
// call by the 202 response, not embedded in the description.
func TestDeriveDescription_AppendsApprovalNoticeWhenRequired(t *testing.T) {
	required := true
	a := actionMeta{
		Body:     "# Send Email\n\nSends a Gmail message.",
		Approval: &actionApprovalPolicy{Required: &required},
	}
	got := deriveDescription(a)
	if !strings.Contains(got, "Sends a Gmail message.") {
		t.Errorf("base description lost; got %q", got)
	}
	for _, want := range []string{"requires user approval", "does NOT block", "pending_approval", "check_action_status"} {
		if !strings.Contains(got, want) {
			t.Errorf("approval notice missing %q; got %q", want, got)
		}
	}
}

// TestDeriveDescription_NoNoticeWhenApprovalNotRequired asserts that
// actions without `[approval] required = true` get the unmodified
// description — the legacy path is unchanged.
func TestDeriveDescription_NoNoticeWhenApprovalNotRequired(t *testing.T) {
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

// TestDeriveDescription_NoticeIsURLAgnostic asserts the new notice
// does NOT embed AILERON_APPROVAL_URL. Under the async contract the
// per-call review URL is on the daemon's 202 response; baking a URL
// into the description would make it stale across calls and across
// daemons. Regression-guards against a future revert.
func TestDeriveDescription_NoticeIsURLAgnostic(t *testing.T) {
	t.Setenv("AILERON_APPROVAL_URL", "http://127.0.0.1:54321/approvals")
	required := true
	a := actionMeta{
		Body:     "# Send Email\n\nSends a Gmail message.",
		Approval: &actionApprovalPolicy{Required: &required},
	}
	got := deriveDescription(a)
	if strings.Contains(got, "http://") {
		t.Errorf("description should not embed a URL under the async contract; got %q", got)
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

func TestDiscoverActions_OmitsDisabledActions(t *testing.T) {
	disabled := false
	enabled := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(actionListResponse{
			Items: []actionMeta{
				{Name: "ship-update", Body: "# Ship", Enabled: &enabled},
				{Name: "list-emails", Body: "# List", Enabled: &disabled},
				// Missing Enabled — older-daemon shape, treated as enabled.
				{Name: "legacy-tool", Body: "# Legacy"},
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
		t.Fatalf("len(tools) = %d, want 2 (disabled action filtered)", len(tools))
	}
	if _, ok := nameMap["list_emails"]; ok {
		t.Errorf("disabled action 'list-emails' leaked into nameMap")
	}
	if _, ok := nameMap["ship_update"]; !ok {
		t.Errorf("enabled action 'ship-update' missing from nameMap")
	}
	if _, ok := nameMap["legacy_tool"]; !ok {
		t.Errorf("nil-Enabled action 'legacy-tool' should default to enabled")
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

// --- Async approval flow ---

// TestRunAction_PendingApprovalSurfacedAsToolText: a 202 response from
// the daemon (approval-gated action) is NOT treated as an error. The
// daemon's `message` lands in the tool's text content so the LLM
// surfaces the approve-here instruction to the user. The approval id
// must be visible so a follow-up check_action_status call can
// reference it.
func TestRunAction_PendingApprovalSurfacedAsToolText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(actionRunPendingResponse{
			Status:     "pending_approval",
			ApprovalID: "act-42",
			ReviewURL:  "http://127.0.0.1:50419/approvals?focus=act-42",
			Message:    "Approval needed for send-email. Visit http://127.0.0.1:50419/approvals?focus=act-42 to approve, or run 'aileron open approval act-42' from any terminal.",
		})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.runAction(context.Background(), "send-email", nil)
	if got.IsError {
		t.Fatalf("202 must not be an error result; got error: %s", got.Content[0].Text)
	}
	text := got.Content[0].Text
	for _, want := range []string{"Approval needed", "act-42", "aileron open approval act-42"} {
		if !strings.Contains(text, want) {
			t.Errorf("content missing %q; got %q", want, text)
		}
	}
}

// TestRunAction_PendingApprovalFallbackWhenMessageEmpty: if a daemon
// sends a 202 with an empty `message` (shouldn't happen under the
// current contract, but defending against future spec drift), the
// MCP wrapper still surfaces a non-empty text block carrying the
// approval id so the LLM has something to work with.
func TestRunAction_PendingApprovalFallbackWhenMessageEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(actionRunPendingResponse{
			Status:     "pending_approval",
			ApprovalID: "act-99",
			Message:    "",
		})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.runAction(context.Background(), "send-email", nil)
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "act-99") {
		t.Errorf("fallback content should carry approval id; got %q", got.Content[0].Text)
	}
}

// TestCheckActionStatus_FormatsCompleted: the check_action_status
// tool's text output names the status, surfaces audit id and result
// when present. Keeps the format terse — LLMs read this, not humans.
func TestCheckActionStatus_FormatsCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/action-approvals/act-1/result" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		auditID := "audit_xyz"
		result := `{"sent":true}`
		_ = json.NewEncoder(w).Encode(actionApprovalResult{
			Status:  "completed",
			AuditID: &auditID,
			Result:  &result,
		})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.checkActionStatus(context.Background(), map[string]any{"approval_id": "act-1"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	for _, want := range []string{"status: completed", "audit_id=audit_xyz", `{"sent":true}`} {
		if !strings.Contains(got.Content[0].Text, want) {
			t.Errorf("output missing %q; got %q", want, got.Content[0].Text)
		}
	}
}

// TestCheckActionStatus_FormatsDeniedWithReason: denied entries
// expose the user's reason so the LLM can apologize / retry / pivot.
func TestCheckActionStatus_FormatsDeniedWithReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reason := "wrong recipient"
		_ = json.NewEncoder(w).Encode(actionApprovalResult{
			Status: "denied",
			Reason: &reason,
		})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.checkActionStatus(context.Background(), map[string]any{"approval_id": "act-1"})
	if got.IsError {
		t.Fatalf("denied is not an error result; got error: %s", got.Content[0].Text)
	}
	for _, want := range []string{"status: denied", "wrong recipient"} {
		if !strings.Contains(got.Content[0].Text, want) {
			t.Errorf("output missing %q; got %q", want, got.Content[0].Text)
		}
	}
}

// TestCheckActionStatus_FormatsPendingApproval: transient statuses
// (pending_approval, running) carry only the status word — no other
// fields are populated by the daemon. The formatter shouldn't invent
// any.
func TestCheckActionStatus_FormatsPendingApproval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(actionApprovalResult{Status: "pending_approval"})
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.checkActionStatus(context.Background(), map[string]any{"approval_id": "act-1"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	if got.Content[0].Text != "status: pending_approval" {
		t.Errorf("output = %q, want bare status line", got.Content[0].Text)
	}
}

// TestCheckActionStatus_NotFoundIsAnError: a 404 from the daemon
// (unknown approval id — including ids minted before a daemon
// restart) surfaces as a tool error so the LLM recognizes it as a
// failure to recover from rather than a transient state.
func TestCheckActionStatus_NotFoundIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found"}}`))
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	got := s.checkActionStatus(context.Background(), map[string]any{"approval_id": "ghost"})
	if !got.IsError {
		t.Fatal("404 should surface as a tool error")
	}
	if !strings.Contains(got.Content[0].Text, "ghost") {
		t.Errorf("error text should name the missing id; got %q", got.Content[0].Text)
	}
}

// TestCheckActionStatus_MissingApprovalIDReturnsError: the LLM
// occasionally calls a tool without all required args. Surface the
// missing-arg case as a usable error rather than letting it 400 from
// the daemon with a less obvious shape.
func TestCheckActionStatus_MissingApprovalIDReturnsError(t *testing.T) {
	s := &server{aileronURL: "http://x", httpClient: &http.Client{}}
	got := s.checkActionStatus(context.Background(), map[string]any{})
	if !got.IsError {
		t.Fatal("missing approval_id should be a tool error")
	}
	if !strings.Contains(got.Content[0].Text, "approval_id") {
		t.Errorf("error text should name the missing arg; got %q", got.Content[0].Text)
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
	if len(tools) != 5 {
		t.Errorf("expected 4 comms tools + check_action_status, got %d", len(tools))
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
