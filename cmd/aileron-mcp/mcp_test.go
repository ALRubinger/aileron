package main

import (
	"encoding/json"
	"testing"
)

func TestHandle_Initialize(t *testing.T) {
	s := &server{client: nil} // client not needed for initialize

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
	s := &server{client: nil}

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
	s := &server{client: nil}

	resp := s.handle(jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})

	if resp != nil {
		t.Fatal("expected nil response for notification")
	}
}

func TestHandle_UnknownMethod(t *testing.T) {
	s := &server{client: nil}

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

func TestHandle_ToolsList(t *testing.T) {
	s := &server{client: nil}

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

	expectedTools := map[string]bool{
		"submit_intent":          false,
		"check_approval":        false,
		"execute_action":        false,
		"list_pending_approvals": false,
	}

	for _, tool := range tools {
		if _, exists := expectedTools[tool.Name]; exists {
			expectedTools[tool.Name] = true
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("tool %q not found in tools list", name)
		}
	}
}

func TestCallTool_UnknownTool(t *testing.T) {
	s := &server{client: nil}

	result := s.callTool(callToolParams{
		Name:      "nonexistent_tool",
		Arguments: map[string]any{},
	})

	if !result.IsError {
		t.Error("expected error for unknown tool")
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

func TestHelpers_NilIfEmpty(t *testing.T) {
	if nilIfEmpty("") != nil {
		t.Error("nilIfEmpty(\"\") should return nil")
	}
	if got := nilIfEmpty("foo"); got == nil || *got != "foo" {
		t.Error("nilIfEmpty(\"foo\") should return pointer to \"foo\"")
	}
}
