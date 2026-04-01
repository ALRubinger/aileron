package main

import (
	"encoding/json"
	"testing"
)

func TestHandle_Initialize(t *testing.T) {
	s := &server{gw: &gateway{routes: make(map[string]toolRoute), queue: newCallQueue()}}

	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
	}

	resp := s.handle(req)
	if resp == nil {
		t.Fatal("expected response")
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected result to be map")
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want %q", result["protocolVersion"], "2024-11-05")
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatal("expected serverInfo")
	}
	if serverInfo["name"] != "aileron" {
		t.Errorf("name = %v, want %q", serverInfo["name"], "aileron")
	}
}

func TestHandle_Ping(t *testing.T) {
	s := &server{gw: &gateway{routes: make(map[string]toolRoute), queue: newCallQueue()}}

	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "ping",
	})

	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
}

func TestHandle_NotificationsInitialized(t *testing.T) {
	s := &server{gw: &gateway{routes: make(map[string]toolRoute), queue: newCallQueue()}}

	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})

	if resp != nil {
		t.Fatal("expected nil response for notification")
	}
}

func TestHandle_UnknownMethod(t *testing.T) {
	s := &server{gw: &gateway{routes: make(map[string]toolRoute), queue: newCallQueue()}}

	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`3`),
		Method:  "unknown/method",
	})

	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want %d", resp.Error.Code, -32601)
	}
}

func TestHandle_ToolsList_IncludesCheckApproval(t *testing.T) {
	gw := &gateway{
		routes: make(map[string]toolRoute),
		tools:  []toolDef{checkApprovalToolDef()},
		queue:  newCallQueue(),
	}
	s := &server{gw: gw}

	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`4`),
		Method:  "tools/list",
	})

	if resp == nil {
		t.Fatal("expected response")
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected result map")
	}

	tools, ok := result["tools"].([]toolDef)
	if !ok {
		t.Fatal("expected tools array")
	}

	found := false
	for _, tool := range tools {
		if tool.Name == checkApprovalTool {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("tool %q not found in tools list", checkApprovalTool)
	}
}

func TestRouteToolCall_UnknownTool(t *testing.T) {
	gw := &gateway{routes: make(map[string]toolRoute), queue: newCallQueue()}

	result := gw.routeToolCall(nil, "nonexistent_tool", map[string]any{})
	if !result.IsError {
		t.Error("expected error for unknown tool")
	}
}

func TestRouteToolCall_CheckApprovalStub(t *testing.T) {
	gw := &gateway{routes: make(map[string]toolRoute), queue: newCallQueue()}

	result := gw.routeToolCall(nil, checkApprovalTool, map[string]any{
		"approval_id": "apr_test123",
	})

	if result.IsError {
		t.Error("expected no error for check_approval stub")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}
}

func TestRouteToolCall_CheckApprovalMissingID(t *testing.T) {
	gw := &gateway{routes: make(map[string]toolRoute), queue: newCallQueue()}

	result := gw.routeToolCall(nil, checkApprovalTool, map[string]any{})
	if !result.IsError {
		t.Error("expected error when approval_id is missing")
	}
}

func TestHelpers_ArgStr(t *testing.T) {
	args := map[string]any{
		"key1": "value1",
		"key2": 42,
	}

	if got := argStr(args, "key1"); got != "value1" {
		t.Errorf("argStr(key1) = %q, want %q", got, "value1")
	}
	if got := argStr(args, "key2"); got != "" {
		t.Errorf("argStr(key2) = %q, want empty (non-string)", got)
	}
	if got := argStr(args, "missing"); got != "" {
		t.Errorf("argStr(missing) = %q, want empty", got)
	}
}

func TestToSchema_NilInput(t *testing.T) {
	s := toSchema(nil)
	if s.Type != "object" {
		t.Errorf("Type = %q, want %q", s.Type, "object")
	}
}

func TestToSchema_WithProperties(t *testing.T) {
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The name",
			},
		},
		"required": []any{"name"},
	}

	s := toSchema(input)
	if s.Type != "object" {
		t.Errorf("Type = %q, want %q", s.Type, "object")
	}
	if len(s.Properties) != 1 {
		t.Fatalf("Properties len = %d, want 1", len(s.Properties))
	}
	prop, ok := s.Properties["name"]
	if !ok {
		t.Fatal("expected 'name' property")
	}
	if prop.Type != "string" {
		t.Errorf("prop.Type = %q, want %q", prop.Type, "string")
	}
	if prop.Description != "The name" {
		t.Errorf("prop.Description = %q, want %q", prop.Description, "The name")
	}
	if len(s.Required) != 1 || s.Required[0] != "name" {
		t.Errorf("Required = %v, want [name]", s.Required)
	}
}

func TestServerNameFromQualified(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github__create_pr", "github"},
		{"no_separator", ""},
		{"multi__sep__name", "multi"},
	}
	for _, tc := range tests {
		if got := serverNameFromQualified(tc.input); got != tc.want {
			t.Errorf("serverNameFromQualified(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestOriginalNameFromQualified(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github__create_pr", "create_pr"},
		{"no_separator", "no_separator"},
		{"multi__sep__name", "sep__name"},
	}
	for _, tc := range tests {
		if got := originalNameFromQualified(tc.input); got != tc.want {
			t.Errorf("originalNameFromQualified(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
