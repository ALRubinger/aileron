package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestHandle_ToolsList(t *testing.T) {
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
	if !ok || len(tools) != 1 {
		t.Fatal("expected exactly one tool")
	}
	if tools[0].Name != "submit_intent" {
		t.Errorf("expected submit_intent tool, got %s", tools[0].Name)
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

func TestErrorResponse(t *testing.T) {
	resp := errorResponse(json.RawMessage(`99`), -32600, "invalid request")
	if resp.JSONRPC != "2.0" {
		t.Errorf("expected JSONRPC 2.0")
	}
	if resp.Error == nil || resp.Error.Code != -32600 || resp.Error.Message != "invalid request" {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
}
