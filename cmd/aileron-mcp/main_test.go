package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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
	s := &server{commsURL: "http://x", sessionID: "sess-x", httpClient: &http.Client{}}
	tools := s.availableTools()
	if len(tools) != 6 {
		t.Fatalf("expected 4 comms tools + check_action_status + resume_flight_plan, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, td := range tools {
		names[td.Name] = true
	}
	for _, want := range []string{"read_messages", "draft_reply", "send_message", "http_request", "check_action_status", "resume_flight_plan"} {
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
	if len(tools) != 3 {
		t.Fatalf("expected 1 action tool + check_action_status + resume_flight_plan, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, td := range tools {
		names[td.Name] = true
	}
	if !names["ship_update"] || !names["check_action_status"] || !names["resume_flight_plan"] {
		t.Errorf("tools = %v, want ship_update + check_action_status + resume_flight_plan", names)
	}
}

func TestAvailableTools_CommsAndActions(t *testing.T) {
	s := &server{
		commsURL: "http://x", sessionID: "sess-x",
		httpClient: &http.Client{},
		actionTools: []toolDef{
			{Name: "ship_update", Description: "x", InputSchema: schema{Type: "object"}},
			{Name: "list_emails", Description: "y", InputSchema: schema{Type: "object"}},
		},
	}
	tools := s.availableTools()
	if len(tools) != 8 {
		t.Fatalf("expected 4 comms + 2 action + check_action_status + resume_flight_plan = 8 tools, got %d", len(tools))
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

// TestDeriveInputSchema_StructuredTypes pins the LLM-facing schema for
// `array` and `object` input types. Per the ADR-0003 amendment, `array`
// inputs always emit an `items` clause: when the manifest declares
// `items_type` the clause carries the element type, otherwise the
// clause is an empty object (any-element). `object` continues to pass
// through with no `properties` constraint.
func TestDeriveInputSchema_StructuredTypes(t *testing.T) {
	a := actionMeta{
		Inputs: []actionInput{
			{Name: "requests", Type: "array", Description: "batchUpdate requests"},
			{Name: "options", Type: "object", Description: "config options"},
		},
	}
	got := deriveInputSchema(a)
	if got.Properties["requests"].Type != "array" {
		t.Errorf("requests type = %q, want array", got.Properties["requests"].Type)
	}
	// Bare array (no items_type) — items must be present but empty so
	// strict MCP clients (Codex) don't default the projection to
	// string[]. The empty-object clause is "any element."
	if got.Properties["requests"].Items == nil {
		t.Fatalf("requests.items = nil, want non-nil empty clause")
	}
	if got.Properties["requests"].Items.Type != "" {
		t.Errorf("requests.items.type = %q, want empty", got.Properties["requests"].Items.Type)
	}
	if got.Properties["options"].Type != "object" {
		t.Errorf("options type = %q, want object", got.Properties["options"].Type)
	}
	// Object inputs do not get an items clause; that field is
	// array-only on the JSON Schema side.
	if got.Properties["options"].Items != nil {
		t.Errorf("options.items = %+v, want nil", got.Properties["options"].Items)
	}
}

// TestDeriveInputSchema_ArrayItemsType pins that the schema deriver
// projects each accepted `items_type` value through to the
// LLM-facing `items.type` clause verbatim. Concrete motivating case:
// `update-doc` with `items_type = "object"` ensures Codex projects the
// `requests` parameter as `object[]` rather than defaulting to
// `string[]`.
func TestDeriveInputSchema_ArrayItemsType(t *testing.T) {
	cases := []struct {
		name  string
		items string
	}{
		{"string", "string"},
		{"integer", "integer"},
		{"number", "number"},
		{"boolean", "boolean"},
		{"object", "object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := actionMeta{
				Inputs: []actionInput{
					{Name: "vals", Type: "array", ItemsType: tc.items, Description: "values"},
				},
			}
			got := deriveInputSchema(a)
			prop := got.Properties["vals"]
			if prop.Type != "array" {
				t.Errorf("vals.type = %q, want array", prop.Type)
			}
			if prop.Items == nil {
				t.Fatalf("vals.items = nil, want non-nil")
			}
			if prop.Items.Type != tc.items {
				t.Errorf("vals.items.type = %q, want %q", prop.Items.Type, tc.items)
			}
		})
	}
}

// TestDeriveInputSchema_ArrayItemsJSONShape pins the wire-level JSON
// for both forms of array emission: arrays with `items_type` serialize
// as `"items": {"type": "<T>"}`, arrays without it serialize as
// `"items": {}` (any-element). The bare-`{}` form is the load-bearing
// fix for hosts that default missing `items` to string[].
func TestDeriveInputSchema_ArrayItemsJSONShape(t *testing.T) {
	a := actionMeta{
		Inputs: []actionInput{
			{Name: "requests", Type: "array", ItemsType: "object", Description: "Docs API Request objects"},
			{Name: "tags", Type: "array", Description: "free-form list"},
		},
	}
	got := deriveInputSchema(a)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal error = %v", err)
	}
	if !strings.Contains(string(raw), `"items":{"type":"object"}`) {
		t.Errorf("typed array missing %q in: %s", `"items":{"type":"object"}`, raw)
	}
	if !strings.Contains(string(raw), `"items":{}`) {
		t.Errorf("bare array missing %q in: %s", `"items":{}`, raw)
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

// --- CLI flag handling (U4 / #953) ---

func TestHandleEarlyArgs_VersionPrintsAndExits(t *testing.T) {
	var buf strings.Builder
	if !handleEarlyArgs([]string{"aileron-mcp", "--version"}, &buf) {
		t.Error("--version should return true (exit signal)")
	}
	if !strings.Contains(buf.String(), "dev") && len(strings.TrimSpace(buf.String())) == 0 {
		t.Errorf("--version output should be a version string; got %q", buf.String())
	}
	// -v alias
	buf.Reset()
	if !handleEarlyArgs([]string{"aileron-mcp", "-v"}, &buf) {
		t.Error("-v should return true (exit signal)")
	}
	if len(strings.TrimSpace(buf.String())) == 0 {
		t.Errorf("-v output empty; got %q", buf.String())
	}
}

func TestHandleEarlyArgs_HelpPrintsUsageAndExits(t *testing.T) {
	var buf strings.Builder
	if !handleEarlyArgs([]string{"aileron-mcp", "--help"}, &buf) {
		t.Error("--help should return true (exit signal)")
	}
	for _, want := range []string{"aileron-mcp", "AILERON_URL"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--help output missing %q; got %q", want, buf.String())
		}
	}
	// -h alias
	buf.Reset()
	if !handleEarlyArgs([]string{"aileron-mcp", "-h"}, &buf) {
		t.Error("-h should return true (exit signal)")
	}
	if len(strings.TrimSpace(buf.String())) == 0 {
		t.Errorf("-h output empty; got %q", buf.String())
	}
}

func TestHandleEarlyArgs_UnknownFlagFallsThrough(t *testing.T) {
	var buf strings.Builder
	if handleEarlyArgs([]string{"aileron-mcp", "--unknown"}, &buf) {
		t.Error("unknown flag should return false so main proceeds to stdio loop")
	}
	if buf.Len() != 0 {
		t.Errorf("unknown flag should not write to out; got %q", buf.String())
	}
}

func TestHandleEarlyArgs_NoArgsFallsThrough(t *testing.T) {
	var buf strings.Builder
	if handleEarlyArgs([]string{"aileron-mcp"}, &buf) {
		t.Error("no args should return false")
	}
	if buf.Len() != 0 {
		t.Errorf("no args should not write to out; got %q", buf.String())
	}
}

// --- Launch session id header injection (U7 / #953) ---

// recordedAuthHeaders captures the auth + session-id headers seen by a
// stubbed daemon endpoint so the U7 tests can assert their presence
// without coupling to a specific endpoint.
type recordedAuthHeaders struct {
	auth    string
	session string
	hasAuth bool
	hasSess bool
}

func captureAuthHandler(t *testing.T, recorded *recordedAuthHeaders, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		recorded.auth = r.Header.Get("Authorization")
		recorded.session = r.Header.Get("X-Aileron-Session-Id")
		_, recorded.hasAuth = r.Header["Authorization"]
		_, recorded.hasSess = r.Header["X-Aileron-Session-Id"]
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	}
}

func TestDiscoverActions_InjectsLaunchSessionHeader(t *testing.T) {
	var rec recordedAuthHeaders
	srv := httptest.NewServer(captureAuthHandler(t, &rec, http.StatusOK, `{"items":[]}`))
	defer srv.Close()

	s := &server{
		aileronURL:   srv.URL,
		aileronToken: "tok-abc",
		sessionID:    "sess-discover-123",
		httpClient:   srv.Client(),
	}
	if _, _, err := s.discoverActions(context.Background()); err != nil {
		t.Fatalf("discoverActions: %v", err)
	}
	if rec.auth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want Bearer tok-abc", rec.auth)
	}
	if rec.session != "sess-discover-123" {
		t.Errorf("X-Aileron-Session-Id = %q, want sess-discover-123", rec.session)
	}
}

func TestRunAction_InjectsLaunchSessionHeader(t *testing.T) {
	var rec recordedAuthHeaders
	content := "ok"
	body, _ := json.Marshal(actionRunResponse{AuditID: "audit_x", Result: &content})
	srv := httptest.NewServer(captureAuthHandler(t, &rec, http.StatusOK, string(body)))
	defer srv.Close()

	s := &server{
		aileronURL:   srv.URL,
		aileronToken: "tok-abc",
		sessionID:    "sess-run-456",
		httpClient:   srv.Client(),
	}
	got := s.runAction(context.Background(), "ship-update", map[string]any{"channel": "#x"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	if rec.auth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want Bearer tok-abc", rec.auth)
	}
	if rec.session != "sess-run-456" {
		t.Errorf("X-Aileron-Session-Id = %q, want sess-run-456", rec.session)
	}
}

func TestCheckActionStatus_InjectsLaunchSessionHeader(t *testing.T) {
	var rec recordedAuthHeaders
	body, _ := json.Marshal(actionApprovalResult{Status: "pending_approval"})
	srv := httptest.NewServer(captureAuthHandler(t, &rec, http.StatusOK, string(body)))
	defer srv.Close()

	s := &server{
		aileronURL:   srv.URL,
		aileronToken: "tok-abc",
		sessionID:    "sess-status-789",
		httpClient:   srv.Client(),
	}
	got := s.checkActionStatus(context.Background(), map[string]any{"approval_id": "act-1"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	if rec.auth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want Bearer tok-abc", rec.auth)
	}
	if rec.session != "sess-status-789" {
		t.Errorf("X-Aileron-Session-Id = %q, want sess-status-789", rec.session)
	}
}

// TestActionEndpoints_OmitSessionHeaderWhenUnset: empty AILERON_SESSION_ID
// must NOT result in an empty-string X-Aileron-Session-Id header.
// Daemon middleware treats empty strings as a different signal than
// "header absent", so we want absence, not blank.
func TestActionEndpoints_OmitSessionHeaderWhenUnset(t *testing.T) {
	var rec recordedAuthHeaders
	content := "ok"
	body, _ := json.Marshal(actionRunResponse{AuditID: "audit_x", Result: &content})
	srv := httptest.NewServer(captureAuthHandler(t, &rec, http.StatusOK, string(body)))
	defer srv.Close()

	s := &server{
		aileronURL:   srv.URL,
		aileronToken: "tok-abc",
		// sessionID intentionally empty
		httpClient: srv.Client(),
	}
	_ = s.runAction(context.Background(), "ship-update", nil)
	if !rec.hasAuth {
		t.Error("Authorization header should still be set when token is present")
	}
	if rec.hasSess {
		t.Errorf("X-Aileron-Session-Id should be absent when sessionID is empty; got %q", rec.session)
	}
}

// TestCommsEndpoints_DoNotCarrySessionHeader: comms endpoints encode
// the session id in the path. Setting the header on those calls is
// redundant; assert we don't accidentally start doing it.
func TestCommsEndpoints_DoNotCarrySessionHeader(t *testing.T) {
	var rec recordedAuthHeaders
	srv := httptest.NewServer(captureAuthHandler(t, &rec, http.StatusOK, `{"messages":[]}`))
	defer srv.Close()

	s := &server{
		commsURL:        srv.URL,
		aileronToken:    "tok-abc",
		sessionID:       "sess-comms-1",
		commsHTTPClient: srv.Client(),
	}
	var out readMessagesResponse
	if err := s.commsGET(s.commsEndpoint("messages"), &out); err != nil {
		t.Fatalf("commsGET: %v", err)
	}
	if rec.auth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want Bearer tok-abc", rec.auth)
	}
	if rec.hasSess {
		t.Errorf("comms endpoints should not carry X-Aileron-Session-Id; got %q", rec.session)
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

// writePendingApproval encodes the 202 ActionRunPendingResponse the
// daemon returns for an approval-gated comms POST. It is the real
// contract for /comms/send, /comms/draft, and /comms/http (the daemon
// always answers 202 — see internal/app/handlers_comms.go) — never a
// 200 with an `ok`/`error` body.
func writePendingApproval(w http.ResponseWriter, approvalID, message string) {
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(actionRunPendingResponse{
		Status:     "pending_approval",
		ApprovalID: approvalID,
		ReviewURL:  "http://review/" + approvalID,
		Message:    message,
	})
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
		writePendingApproval(w, "appr-send", "Approval needed for send_message. Visit http://review/appr-send to approve.")
	}))
	got := s.sendMessage(map[string]any{"service": "slack", "channel": "#x", "body": "hi"})
	if got.IsError {
		t.Fatalf("202 pending_approval must not be an error result, got: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "Approval needed for send_message") {
		t.Errorf("expected the pending-approval message verbatim, got %q", got.Content[0].Text)
	}
}

// TestSendMessage_PendingMessageFallback covers the older-daemon case
// where the 202 omits `message`: the wrapper still returns a non-error
// result naming the approval id so the agent can poll check_action_status.
func TestSendMessage_PendingMessageFallback(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writePendingApproval(w, "appr-77", "")
	}))
	got := s.sendMessage(map[string]any{"service": "slack", "channel": "#x", "body": "hi"})
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "appr-77") {
		t.Errorf("expected approval id in fallback message, got %q", got.Content[0].Text)
	}
}

// TestSendMessage_DaemonError verifies a genuine non-202 (the request
// never reached the queue, e.g. a 400 no_listener) still surfaces as an
// error result, not the pending path.
func TestSendMessage_DaemonError(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"no_listener","message":"no listener for service: slack"}`))
	}))
	got := s.sendMessage(map[string]any{"service": "slack", "channel": "#x", "body": "hi"})
	if !got.IsError {
		t.Fatal("expected error result on non-202 from daemon")
	}
	if !strings.Contains(got.Content[0].Text, "no_listener") {
		t.Errorf("expected daemon's error body in result, got %q", got.Content[0].Text)
	}
}

func TestDraftReply_WithDaemon(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/comms/draft") {
			t.Errorf("path = %q", r.URL.Path)
		}
		writePendingApproval(w, "appr-draft", "Approval needed for draft_reply. Visit http://review/appr-draft to approve.")
	}))
	got := s.draftReply(map[string]any{"message_id": "1", "body": "hi"})
	if got.IsError {
		t.Fatalf("202 pending_approval must not be an error result, got: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "Approval needed for draft_reply") {
		t.Errorf("expected the pending-approval message verbatim, got %q", got.Content[0].Text)
	}
}

func TestDraftReply_DaemonError(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"action_approvals_disabled"}`))
	}))
	got := s.draftReply(map[string]any{"message_id": "1", "body": "hi"})
	if !got.IsError {
		t.Fatal("expected error result on non-202 from daemon")
	}
}

func TestHttpRequest_WithDaemon(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/comms/http") {
			t.Errorf("path = %q", r.URL.Path)
		}
		writePendingApproval(w, "appr-http", "Approval needed for http_request. Visit http://review/appr-http to approve.")
	}))
	got := s.httpRequest(map[string]any{"method": "GET", "url": "https://example.com"})
	if got.IsError {
		t.Fatalf("202 pending_approval must not be an error result, got: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "Approval needed for http_request") {
		t.Errorf("expected the pending-approval message verbatim, got %q", got.Content[0].Text)
	}
}

func TestHttpRequest_DaemonError(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"url_not_allowed"}`))
	}))
	got := s.httpRequest(map[string]any{"method": "GET", "url": "https://blocked"})
	if !got.IsError {
		t.Fatal("expected error result on non-202 from daemon")
	}
}

func TestDispatchTool_RoutesAllCommsTools(t *testing.T) {
	s := commsServerWithFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /comms/messages is GET (200 body); the approval-gated POSTs
		// answer 202 ActionRunPendingResponse.
		if strings.HasSuffix(r.URL.Path, "/comms/messages") {
			_ = json.NewEncoder(w).Encode(readMessagesResponse{Messages: []commsMessage{}})
			return
		}
		writePendingApproval(w, "appr-x", "Approval needed. Visit http://review/appr-x to approve.")
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
	if len(tools) != 6 {
		t.Errorf("expected 4 comms tools + check_action_status + resume_flight_plan, got %d", len(tools))
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

// --- Mid-session action refresh (issue #897) ---

// syncBuffer is a goroutine-safe bytes.Buffer so a test can read the
// poller's emitted notifications while the refresh goroutine (or a
// direct refreshOnce call) writes to s.out under writeMu.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// emittedMethods parses every newline-delimited JSON-RPC frame written
// to the buffer and returns the "method" field of each. Non-JSON or
// frames without a method are skipped.
func emittedMethods(t *testing.T, s string) []string {
	t.Helper()
	var methods []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var frame struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		if frame.Method != "" {
			methods = append(methods, frame.Method)
		}
	}
	return methods
}

func countListChanged(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, m := range emittedMethods(t, s) {
		if m == "notifications/tools/list_changed" {
			n++
		}
	}
	return n
}

func TestHandle_Initialize_AdvertisesListChanged(t *testing.T) {
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
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing or wrong type: %#v", result["capabilities"])
	}
	tools, ok := caps["tools"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.tools missing or wrong type: %#v", caps["tools"])
	}
	if tools["listChanged"] != true {
		t.Errorf("capabilities.tools.listChanged = %v, want true", tools["listChanged"])
	}
}

// mutableActions is a goroutine-safe holder for the action set an
// httptest daemon serves, so a test can swap what GET /v1/actions
// returns between poll cycles while the poller goroutine reads it
// concurrently (no data race on the slice).
type mutableActions struct {
	mu    sync.Mutex
	items []actionMeta
}

func newMutableActions(items ...actionMeta) *mutableActions {
	return &mutableActions{items: items}
}

func (m *mutableActions) set(items ...actionMeta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = items
}

func (m *mutableActions) get() []actionMeta {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]actionMeta(nil), m.items...)
}

func (m *mutableActions) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(actionListResponse{Items: m.get()})
	})
}

func enabledPtr() *bool  { b := true; return &b }
func disabledPtr() *bool { b := false; return &b }

func TestRefreshOnce_EmitsOnActionAdded(t *testing.T) {
	actions := newMutableActions(actionMeta{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()})
	srv := httptest.NewServer(actions.handler())
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	// Seed the cache with the startup surface.
	tools, nameMap, err := s.discoverActions(context.Background())
	if err != nil {
		t.Fatalf("discoverActions: %v", err)
	}
	s.setActions(tools, nameMap)

	// Add a second action and poll.
	actions.set(
		actionMeta{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()},
		actionMeta{Name: "list-emails", Body: "# List", Enabled: enabledPtr()},
	)
	if !s.refreshOnce(context.Background()) {
		t.Fatal("refreshOnce returned false; expected a change (action added)")
	}
	if got := countListChanged(t, out.String()); got != 1 {
		t.Fatalf("list_changed emissions = %d, want 1", got)
	}
	// The new action is now routable.
	s.actionsMu.RLock()
	_, ok := s.actionNameMap["list_emails"]
	s.actionsMu.RUnlock()
	if !ok {
		t.Error("added action 'list-emails' not present in nameMap after refresh")
	}
}

func TestRefreshOnce_EmitsOnActionEnabled(t *testing.T) {
	actions := newMutableActions(actionMeta{Name: "list-emails", Body: "# List", Enabled: disabledPtr()})
	srv := httptest.NewServer(actions.handler())
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	tools, nameMap, _ := s.discoverActions(context.Background())
	s.setActions(tools, nameMap)
	// Disabled action is hidden at boot.
	s.actionsMu.RLock()
	hidden := len(s.actionTools)
	s.actionsMu.RUnlock()
	if hidden != 0 {
		t.Fatalf("disabled action surfaced at boot: %d tools", hidden)
	}

	actions.set(actionMeta{Name: "list-emails", Body: "# List", Enabled: enabledPtr()})
	if !s.refreshOnce(context.Background()) {
		t.Fatal("refreshOnce returned false; expected a change (action enabled)")
	}
	if got := countListChanged(t, out.String()); got != 1 {
		t.Fatalf("list_changed emissions = %d, want 1", got)
	}
}

func TestRefreshOnce_EmitsOnActionDisabled(t *testing.T) {
	actions := newMutableActions(actionMeta{Name: "list-emails", Body: "# List", Enabled: enabledPtr()})
	srv := httptest.NewServer(actions.handler())
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	tools, nameMap, _ := s.discoverActions(context.Background())
	s.setActions(tools, nameMap)

	actions.set(actionMeta{Name: "list-emails", Body: "# List", Enabled: disabledPtr()})
	if !s.refreshOnce(context.Background()) {
		t.Fatal("refreshOnce returned false; expected a change (action disabled)")
	}
	if got := countListChanged(t, out.String()); got != 1 {
		t.Fatalf("list_changed emissions = %d, want 1", got)
	}
	s.actionsMu.RLock()
	remaining := len(s.actionTools)
	s.actionsMu.RUnlock()
	if remaining != 0 {
		t.Errorf("disabled action still surfaced after refresh: %d tools", remaining)
	}
}

func TestRefreshOnce_EmitsOnActionRemoved(t *testing.T) {
	actions := newMutableActions(
		actionMeta{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()},
		actionMeta{Name: "list-emails", Body: "# List", Enabled: enabledPtr()},
	)
	srv := httptest.NewServer(actions.handler())
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	tools, nameMap, _ := s.discoverActions(context.Background())
	s.setActions(tools, nameMap)

	actions.set(actionMeta{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()}) // remove list-emails
	if !s.refreshOnce(context.Background()) {
		t.Fatal("refreshOnce returned false; expected a change (action removed)")
	}
	if got := countListChanged(t, out.String()); got != 1 {
		t.Fatalf("list_changed emissions = %d, want 1", got)
	}
	s.actionsMu.RLock()
	_, ok := s.actionNameMap["list_emails"]
	s.actionsMu.RUnlock()
	if ok {
		t.Error("removed action 'list-emails' still routable after refresh")
	}
}

func TestRefreshOnce_NoEmitWhenUnchanged(t *testing.T) {
	actions := newMutableActions(actionMeta{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()})
	srv := httptest.NewServer(actions.handler())
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	tools, nameMap, _ := s.discoverActions(context.Background())
	s.setActions(tools, nameMap)

	if s.refreshOnce(context.Background()) {
		t.Fatal("refreshOnce returned true; expected no change")
	}
	if got := countListChanged(t, out.String()); got != 0 {
		t.Fatalf("list_changed emissions = %d, want 0 (no change)", got)
	}
}

func TestRefreshOnce_FailureLeavesSurfaceIntact(t *testing.T) {
	// First call succeeds, subsequent calls 500 — proves a refresh
	// failure does not overwrite a good cache (the #897 contract:
	// failures must not corrupt the working tool surface).
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(actionListResponse{
				Items: []actionMeta{{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()}},
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	tools, nameMap, err := s.discoverActions(context.Background())
	if err != nil {
		t.Fatalf("seed discovery: %v", err)
	}
	s.setActions(tools, nameMap)

	if s.refreshOnce(context.Background()) {
		t.Fatal("refreshOnce returned true on a failed discovery")
	}
	if got := countListChanged(t, out.String()); got != 0 {
		t.Fatalf("list_changed emitted on failure: %d", got)
	}
	// The good surface survives.
	s.actionsMu.RLock()
	_, ok := s.actionNameMap["ship_update"]
	n := len(s.actionTools)
	s.actionsMu.RUnlock()
	if !ok || n != 1 {
		t.Errorf("failure corrupted surface: tools=%d, ship_update present=%v", n, ok)
	}
}

func TestRefreshInterval(t *testing.T) {
	cases := []struct {
		raw     string
		wantDur time.Duration
		wantOn  bool
	}{
		{"", defaultRefreshInterval, true},
		{"10s", 10 * time.Second, true},
		{"2m", 2 * time.Minute, true},
		{"3", 3 * time.Second, true}, // bare integer → seconds
		{"0", 0, false},              // explicit disable
		{"0s", 0, false},             // zero duration → disable
		{"  ", defaultRefreshInterval, true},
		{"-5s", 0, false},                         // non-positive → disable
		{"garbage", defaultRefreshInterval, true}, // typo falls back to default
	}
	for _, tc := range cases {
		gotDur, gotOn := refreshInterval(tc.raw)
		if gotOn != tc.wantOn {
			t.Errorf("refreshInterval(%q) enabled = %v, want %v", tc.raw, gotOn, tc.wantOn)
			continue
		}
		if gotOn && gotDur != tc.wantDur {
			t.Errorf("refreshInterval(%q) = %v, want %v", tc.raw, gotDur, tc.wantDur)
		}
	}
}

func TestWriteLine_NilOutIsNoop(t *testing.T) {
	s := &server{} // out is nil
	// Must not panic.
	s.writeLine([]byte(`{"jsonrpc":"2.0"}`))
}

func TestRefreshLoop_StopsOnContextCancel(t *testing.T) {
	actions := newMutableActions(actionMeta{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()})
	srv := httptest.NewServer(actions.handler())
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	tools, nameMap, _ := s.discoverActions(context.Background())
	s.setActions(tools, nameMap)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.refreshLoop(ctx, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshLoop did not return after context cancel")
	}
}

func TestMaybeStartRefreshPoller_NoURLDoesNotStart(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	if s.maybeStartRefreshPoller(context.Background(), "5s") {
		t.Error("poller started without AILERON_URL")
	}
}

func TestMaybeStartRefreshPoller_DisabledIntervalDoesNotStart(t *testing.T) {
	s := &server{aileronURL: "http://127.0.0.1:1", httpClient: &http.Client{}}
	if s.maybeStartRefreshPoller(context.Background(), "0") {
		t.Error("poller started with interval=0 (disabled)")
	}
}

func TestMaybeStartRefreshPoller_StartsAndStopsOnCancel(t *testing.T) {
	actions := newMutableActions(actionMeta{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()})
	srv := httptest.NewServer(actions.handler())
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	tools, nameMap, _ := s.discoverActions(context.Background())
	s.setActions(tools, nameMap)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !s.maybeStartRefreshPoller(ctx, "10ms") {
		t.Fatal("poller did not start with valid URL + interval")
	}

	// Flip the served set; the running poller should emit list_changed.
	actions.set(
		actionMeta{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()},
		actionMeta{Name: "list-emails", Body: "# List", Enabled: enabledPtr()},
	)
	deadline := time.After(2 * time.Second)
	for {
		if countListChanged(t, out.String()) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("poller never emitted list_changed after action added")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
}

// --- Discovery failure classification + aileron_diagnostics tool ---

// TestDiscoverActions_UnreachableClassified asserts a transport failure
// (daemon not listening) classifies as reasonUnreachable so the warning
// and diagnostics tool can say "daemon unreachable", not a generic error.
func TestDiscoverActions_UnreachableClassified(t *testing.T) {
	// Point at a closed port so the request fails at the transport layer.
	s := &server{aileronURL: "http://127.0.0.1:1", httpClient: &http.Client{Timeout: time.Second}}
	_, _, err := s.discoverActions(context.Background())
	if err == nil {
		t.Fatal("expected transport error for unreachable daemon")
	}
	diag := classifyDiscovery(nil, err)
	if diag.reason != reasonUnreachable {
		t.Fatalf("reason = %v, want reasonUnreachable", diag.reason)
	}
	if !strings.Contains(diag.summary(s.aileronURL), "unreachable") {
		t.Errorf("summary = %q, want it to mention 'unreachable'", diag.summary(s.aileronURL))
	}
}

// TestDiscoverActions_UnauthorizedClassified asserts a 401 classifies as
// reasonUnauthorized — distinct from a generic HTTP error — so the
// operator learns the token/session is stale.
func TestDiscoverActions_UnauthorizedClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	_, _, err := s.discoverActions(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	diag := classifyDiscovery(nil, err)
	if diag.reason != reasonUnauthorized {
		t.Fatalf("reason = %v, want reasonUnauthorized", diag.reason)
	}
	if !strings.Contains(diag.summary(srv.URL), "unauthorized") {
		t.Errorf("summary = %q, want 'unauthorized'", diag.summary(srv.URL))
	}
	if !strings.Contains(diag.remediation(), "Re-authenticate") {
		t.Errorf("remediation = %q, want re-auth guidance", diag.remediation())
	}
}

// TestDiscoverActions_HTTPErrorClassified asserts a non-401 non-200 (500)
// classifies as reasonHTTPError, keeping it distinct from unreachable /
// unauthorized.
func TestDiscoverActions_HTTPErrorClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	_, _, err := s.discoverActions(context.Background())
	if err == nil {
		t.Fatal("expected error for 500")
	}
	diag := classifyDiscovery(nil, err)
	if diag.reason != reasonHTTPError {
		t.Fatalf("reason = %v, want reasonHTTPError", diag.reason)
	}
}

// TestClassifyDiscovery_EmptyIsReasonEmpty asserts a successful response
// carrying zero actions classifies as reasonEmpty — the wire succeeded
// but the daemon exposes nothing, distinct from a discovery failure.
func TestClassifyDiscovery_EmptyIsReasonEmpty(t *testing.T) {
	diag := classifyDiscovery([]toolDef{}, nil)
	if diag.reason != reasonEmpty {
		t.Fatalf("reason = %v, want reasonEmpty", diag.reason)
	}
	if diag.ok() {
		t.Error("ok() = true for empty result")
	}
	if !strings.Contains(diag.summary("http://x"), "0 actions") {
		t.Errorf("summary = %q, want '0 actions'", diag.summary("http://x"))
	}
}

// TestClassifyDiscovery_SuccessIsReasonOK asserts a non-empty result
// classifies as reasonOK with the action count.
func TestClassifyDiscovery_SuccessIsReasonOK(t *testing.T) {
	diag := classifyDiscovery([]toolDef{{Name: "a"}, {Name: "b"}}, nil)
	if !diag.ok() {
		t.Fatalf("ok() = false, reason = %v", diag.reason)
	}
	if diag.count != 2 {
		t.Errorf("count = %d, want 2", diag.count)
	}
}

// TestAvailableTools_IncludesDiagnosticsWhenURLSet asserts the synthetic
// aileron_diagnostics tool is present whenever AILERON_URL is set, so the
// agent can always ask why connector actions are missing.
func TestAvailableTools_IncludesDiagnosticsWhenURLSet(t *testing.T) {
	s := &server{aileronURL: "http://127.0.0.1:1", httpClient: &http.Client{}}
	names := map[string]bool{}
	for _, td := range s.availableTools() {
		names[td.Name] = true
	}
	if !names["aileron_diagnostics"] {
		t.Errorf("aileron_diagnostics missing; tools = %v", names)
	}
}

// TestAvailableTools_OmitsDiagnosticsWhenNoURL asserts the diagnostics
// tool is absent when there is no daemon to discover against — it would
// have nothing to report and would inflate the host-launch tool surface.
func TestAvailableTools_OmitsDiagnosticsWhenNoURL(t *testing.T) {
	s := &server{commsURL: "http://x", sessionID: "sess-x", httpClient: &http.Client{}}
	for _, td := range s.availableTools() {
		if td.Name == "aileron_diagnostics" {
			t.Fatal("aileron_diagnostics present without AILERON_URL")
		}
	}
}

// TestDiagnostics_ReportsUnreachable asserts the synthetic tool's result
// names the unreachable daemon and the remediation when discovery failed
// at the transport layer — the operator's core complaint, answerable
// from the agent.
func TestDiagnostics_ReportsUnreachable(t *testing.T) {
	s := &server{aileronURL: "http://127.0.0.1:1", httpClient: &http.Client{}}
	s.setDiscovery(discoveryDiagnostic{reason: reasonUnreachable, err: context.DeadlineExceeded})
	res := s.diagnostics()
	if res.IsError {
		t.Fatal("diagnostics should not be an error result")
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "degraded") || !strings.Contains(text, "unreachable") {
		t.Errorf("text = %q, want degraded + unreachable", text)
	}
	if !strings.Contains(text, "aileron launch") {
		t.Errorf("text = %q, want remediation mentioning aileron launch", text)
	}
}

// TestDiagnostics_ReportsUnauthorized asserts a 401 surfaces as a
// re-auth prompt the agent can relay.
func TestDiagnostics_ReportsUnauthorized(t *testing.T) {
	s := &server{aileronURL: "http://daemon", httpClient: &http.Client{}}
	s.setDiscovery(discoveryDiagnostic{reason: reasonUnauthorized})
	text := s.diagnostics().Content[0].Text
	if !strings.Contains(text, "unauthorized") {
		t.Errorf("text = %q, want 'unauthorized'", text)
	}
	if !strings.Contains(text, "Re-authenticate") {
		t.Errorf("text = %q, want re-auth remediation", text)
	}
}

// TestDiagnostics_ReportsEmpty asserts a reachable-but-empty daemon is
// reported as such, distinct from a transport/auth failure.
func TestDiagnostics_ReportsEmpty(t *testing.T) {
	s := &server{aileronURL: "http://daemon", httpClient: &http.Client{}}
	s.setDiscovery(discoveryDiagnostic{reason: reasonEmpty})
	text := s.diagnostics().Content[0].Text
	if !strings.Contains(text, "0 actions") {
		t.Errorf("text = %q, want '0 actions'", text)
	}
	if !strings.Contains(text, "reachable and authorized") {
		t.Errorf("text = %q, want note that daemon is reachable", text)
	}
}

// TestDiagnostics_ReportsHealthy asserts a successful discovery reports a
// healthy state with the action count.
func TestDiagnostics_ReportsHealthy(t *testing.T) {
	s := &server{aileronURL: "http://daemon", httpClient: &http.Client{}}
	s.setDiscovery(discoveryDiagnostic{reason: reasonOK, count: 21})
	text := s.diagnostics().Content[0].Text
	if !strings.Contains(text, "healthy") || !strings.Contains(text, "21") {
		t.Errorf("text = %q, want healthy + count", text)
	}
}

// TestDiagnostics_NoURL asserts that without AILERON_URL the tool still
// answers (it is only registered when URL is set, but the dispatch path
// is defensive) and explains that no connector actions are expected.
func TestDiagnostics_NoURL(t *testing.T) {
	s := &server{httpClient: &http.Client{}}
	text := s.diagnostics().Content[0].Text
	if !strings.Contains(text, "AILERON_URL not set") {
		t.Errorf("text = %q, want AILERON_URL note", text)
	}
}

// TestRefreshOnce_RecordsDiagnosticOnFailure asserts a refresh failure
// updates the cached diagnostic so a later aileron_diagnostics call
// reflects the current (degraded) state, while leaving the tool surface
// intact.
func TestRefreshOnce_RecordsDiagnosticOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	// Seed a healthy prior surface so we can confirm it survives.
	s.setActions([]toolDef{{Name: "ship_update"}}, map[string]string{"ship_update": "ship-update"})
	if s.refreshOnce(context.Background()) {
		t.Error("refreshOnce reported a change on a failed discovery")
	}
	if d := s.discoveryState(); d.reason != reasonUnauthorized {
		t.Fatalf("recorded reason = %v, want reasonUnauthorized", d.reason)
	}
	// Tool surface must be untouched.
	s.actionsMu.RLock()
	got := len(s.actionTools)
	s.actionsMu.RUnlock()
	if got != 1 {
		t.Errorf("action surface mutated on failure: len = %d, want 1", got)
	}
}

// TestDispatchTool_RoutesDiagnostics asserts the dispatcher routes the
// synthetic tool name to the diagnostics handler.
func TestDispatchTool_RoutesDiagnostics(t *testing.T) {
	s := &server{aileronURL: "http://daemon", httpClient: &http.Client{}}
	s.setDiscovery(discoveryDiagnostic{reason: reasonOK, count: 3})
	res := s.dispatchTool(context.Background(), "aileron_diagnostics", nil)
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "healthy") {
		t.Errorf("text = %q, want healthy report", res.Content[0].Text)
	}
}

// TestDiagnostics_ReportsHTTPError asserts a non-401 daemon error surfaces
// the daemon-error summary (including the status) so the operator can tell
// it apart from unreachable / unauthorized.
func TestDiagnostics_ReportsHTTPError(t *testing.T) {
	s := &server{aileronURL: "http://daemon", httpClient: &http.Client{}}
	s.setDiscovery(discoveryDiagnostic{reason: reasonHTTPError, err: errors.New("/v1/actions: 500 Internal Server Error")})
	text := s.diagnostics().Content[0].Text
	if !strings.Contains(text, "daemon error") || !strings.Contains(text, "500") {
		t.Errorf("text = %q, want daemon error with status", text)
	}
}
