package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ALRubinger/aileron/core/llm"
	"github.com/ALRubinger/aileron/core/source"
)

// mockAnthropicServer creates a mock Anthropic Messages API server.
func mockAnthropicServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

func anthropicTextResponse(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_test",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-sonnet-4-6",
			"stop_reason": "end_turn",
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
		})
	}
}

func anthropicToolUseResponse(toolID, toolName string, input map[string]any) http.HandlerFunc {
	var callCount atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		count := callCount.Add(1)
		if count == 1 {
			// First call: return tool_use
			json.NewEncoder(w).Encode(map[string]any{
				"id":          "msg_test",
				"type":        "message",
				"role":        "assistant",
				"model":       "claude-sonnet-4-6",
				"stop_reason": "tool_use",
				"content": []map[string]any{
					{"type": "tool_use", "id": toolID, "name": toolName, "input": input},
				},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
			})
		} else {
			// Second call: return text after tool result
			json.NewEncoder(w).Encode(map[string]any{
				"id":          "msg_test2",
				"type":        "message",
				"role":        "assistant",
				"model":       "claude-sonnet-4-6",
				"stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "text", "text": "Based on the channel history, the answer is yes."},
				},
				"usage": map[string]any{"input_tokens": 30, "output_tokens": 40},
			})
		}
	}
}

func TestConvertTools_SchemaStructure(t *testing.T) {
	// This test validates that the tool input_schema has the correct structure
	// for the Anthropic API. Previously, properties were double-nested:
	//   input_schema.properties = {"properties": {...}, "required": [...], "type": "object"}
	// which Anthropic rejected with "JSON schema is invalid."
	// The correct structure is:
	//   input_schema.properties = {"channel": {"type": "string", ...}}
	//   input_schema.required = ["channel"]

	tools := llm.ConvertTools([]source.ToolDefinition{
		{
			Name:        "slack_channel_history",
			Description: "Get channel messages",
			Parameters: []source.ToolParam{
				{Name: "channel", Type: "string", Description: "Channel ID", Required: true},
				{Name: "limit", Type: "integer", Description: "Max results", Required: false},
			},
		},
	})

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	// Marshal the tool to JSON and inspect the schema structure.
	toolJSON, err := json.Marshal(tools[0])
	if err != nil {
		t.Fatalf("failed to marshal tool: %v", err)
	}

	var toolMap map[string]any
	json.Unmarshal(toolJSON, &toolMap)

	schema, ok := toolMap["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("expected input_schema to be an object, got %T", toolMap["input_schema"])
	}

	// Properties should contain "channel" and "limit" directly — NOT a nested
	// {"properties": ..., "type": "object"} wrapper.
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties to be an object, got %T", schema["properties"])
	}

	if _, hasChannel := props["channel"]; !hasChannel {
		t.Error("expected 'channel' in properties")
	}
	if _, hasLimit := props["limit"]; !hasLimit {
		t.Error("expected 'limit' in properties")
	}

	// Properties should NOT contain a nested "type" key — that was the bug.
	if _, hasNestedType := props["type"]; hasNestedType {
		t.Error("properties contains nested 'type' key — schema is double-nested (the bug)")
	}
	if _, hasNestedRequired := props["required"]; hasNestedRequired {
		t.Error("properties contains nested 'required' key — schema is double-nested (the bug)")
	}

	// Required should be at the schema level, containing "channel".
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("expected required to be an array, got %T", schema["required"])
	}
	if len(required) != 1 || required[0] != "channel" {
		t.Errorf("expected required=[channel], got %v", required)
	}
}

func TestAnthropicClient_SimpleTextGeneration(t *testing.T) {
	server := mockAnthropicServer(anthropicTextResponse("Hello, I'm Claude."))
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	resp, err := client.GenerateWithTools(context.Background(), llm.GenerateRequest{
		SystemPrompt: "You are helpful.",
		UserMessage:  "Say hello.",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Hello, I'm Claude." {
		t.Errorf("expected 'Hello, I'm Claude.', got %q", resp.Text)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(resp.ToolCalls))
	}
}

func TestAnthropicClient_ToolUseLoop(t *testing.T) {
	server := mockAnthropicServer(anthropicToolUseResponse(
		"tool_1", "slack_channel_history", map[string]any{"channel": "C123"},
	))
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	executorCalled := false
	resp, err := client.GenerateWithTools(context.Background(), llm.GenerateRequest{
		SystemPrompt: "You are helpful.",
		UserMessage:  "What happened in the channel?",
		Tools: []source.ToolDefinition{
			{
				Name:        "slack_channel_history",
				Description: "Get channel messages",
				Parameters: []source.ToolParam{
					{Name: "channel", Type: "string", Required: true},
				},
			},
		},
		ToolExecutor: func(_ context.Context, tool string, params map[string]any) (map[string]any, error) {
			executorCalled = true
			return map[string]any{"messages": []string{"Hello", "World"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !executorCalled {
		t.Error("expected tool executor to be called")
	}
	if resp.Text != "Based on the channel history, the answer is yes." {
		t.Errorf("unexpected response: %q", resp.Text)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Tool != "slack_channel_history" {
		t.Errorf("expected slack_channel_history, got %s", resp.ToolCalls[0].Tool)
	}
}

func TestAnthropicClient_NoToolExecutor(t *testing.T) {
	server := mockAnthropicServer(anthropicToolUseResponse(
		"tool_1", "slack_channel_history", map[string]any{},
	))
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	_, err := client.GenerateWithTools(context.Background(), llm.GenerateRequest{
		UserMessage:  "Hello",
		ToolExecutor: nil, // no executor
		Tools: []source.ToolDefinition{
			{Name: "slack_channel_history", Description: "test"},
		},
	})
	if err == nil {
		t.Fatal("expected error when tool use requested but no executor")
	}
}

func TestAnthropicClient_ToolRoundsExhausted_GracefulFallback(t *testing.T) {
	// Regression test: when tool rounds are exhausted, the LLM should make
	// one final call WITHOUT tools and return whatever text it can produce.
	// Previously, this returned an error and the user got no draft.
	var callCount atomic.Int32
	server := mockAnthropicServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		count := callCount.Add(1)

		// Check if tools are present in the request to detect the final call.
		body, _ := io.ReadAll(r.Body)
		hasTools := strings.Contains(string(body), `"tools"`)

		if !hasTools {
			// Final call without tools — return text.
			json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_final", "type": "message", "role": "assistant",
				"model": "claude-sonnet-4-6", "stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "text", "text": "Here's what I found with the context I gathered."},
				},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
			})
			return
		}

		// Keep returning tool_use to exhaust rounds.
		json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("msg_%d", count), "type": "message", "role": "assistant",
			"model": "claude-sonnet-4-6", "stop_reason": "tool_use",
			"content": []map[string]any{
				{"type": "tool_use", "id": fmt.Sprintf("tool_%d", count),
					"name": "slack_search_messages", "input": map[string]any{"query": "test"}},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
		})
	})
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	resp, err := client.GenerateWithTools(context.Background(), llm.GenerateRequest{
		SystemPrompt:  "You are helpful.",
		UserMessage:   "Summarize the week.",
		MaxToolRounds: 2, // low limit to trigger exhaustion quickly
		Tools: []source.ToolDefinition{
			{Name: "slack_search_messages", Description: "Search messages"},
		},
		ToolExecutor: func(_ context.Context, tool string, params map[string]any) (map[string]any, error) {
			return map[string]any{"messages": []string{"result"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("expected graceful fallback, got error: %v", err)
	}
	if resp.Text == "" {
		t.Fatal("expected non-empty text from graceful fallback")
	}
	if resp.Text != "Here's what I found with the context I gathered." {
		t.Errorf("unexpected text: %q", resp.Text)
	}
	if len(resp.ToolCalls) == 0 {
		t.Error("expected tool calls to be recorded before exhaustion")
	}
}

func TestAnthropicClient_ParallelToolExecution(t *testing.T) {
	// When the LLM returns multiple tool_use blocks in one response,
	// they should be executed concurrently. We verify:
	// 1. All tools run in parallel (total time ≈ max, not sum)
	// 2. Results are returned in the same order as the tool_use blocks
	var callCount atomic.Int32
	server := mockAnthropicServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		count := callCount.Add(1)
		if count == 1 {
			// Return 3 tool_use blocks in one response.
			json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant",
				"model": "claude-sonnet-4-6", "stop_reason": "tool_use",
				"content": []map[string]any{
					{"type": "tool_use", "id": "t1", "name": "tool_a", "input": map[string]any{"q": "a"}},
					{"type": "tool_use", "id": "t2", "name": "tool_b", "input": map[string]any{"q": "b"}},
					{"type": "tool_use", "id": "t3", "name": "tool_c", "input": map[string]any{"q": "c"}},
				},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
			})
		} else {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_2", "type": "message", "role": "assistant",
				"model": "claude-sonnet-4-6", "stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "text", "text": "done"},
				},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
			})
		}
	})
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	// Track concurrent execution: each tool signals arrival, then waits
	// until all 3 are running before returning.
	var running atomic.Int32
	allRunning := make(chan struct{})
	go func() {
		for running.Load() < 3 {
			// spin until all 3 goroutines are in-flight
		}
		close(allRunning)
	}()

	resp, err := client.GenerateWithTools(context.Background(), llm.GenerateRequest{
		SystemPrompt: "You are helpful.",
		UserMessage:  "Do three things.",
		Tools: []source.ToolDefinition{
			{Name: "tool_a", Description: "A"},
			{Name: "tool_b", Description: "B"},
			{Name: "tool_c", Description: "C"},
		},
		ToolExecutor: func(ctx context.Context, tool string, params map[string]any) (map[string]any, error) {
			running.Add(1)
			<-allRunning // block until all 3 are running concurrently
			return map[string]any{"tool": tool}, nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all 3 tool calls recorded in order.
	if len(resp.ToolCalls) != 3 {
		t.Fatalf("expected 3 tool calls, got %d", len(resp.ToolCalls))
	}
	expectedOrder := []string{"tool_a", "tool_b", "tool_c"}
	for i, name := range expectedOrder {
		if resp.ToolCalls[i].Tool != name {
			t.Errorf("tool call %d: expected %s, got %s", i, name, resp.ToolCalls[i].Tool)
		}
	}
}

func TestAnthropicClient_ParallelToolExecution_PartialError(t *testing.T) {
	// When one tool fails, the others should still complete and the error
	// should be reported in the correct position.
	var callCount atomic.Int32
	server := mockAnthropicServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		count := callCount.Add(1)
		if count == 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant",
				"model": "claude-sonnet-4-6", "stop_reason": "tool_use",
				"content": []map[string]any{
					{"type": "tool_use", "id": "t1", "name": "tool_ok", "input": map[string]any{}},
					{"type": "tool_use", "id": "t2", "name": "tool_fail", "input": map[string]any{}},
				},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
			})
		} else {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_2", "type": "message", "role": "assistant",
				"model": "claude-sonnet-4-6", "stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "text", "text": "handled error"},
				},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
			})
		}
	})
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	resp, err := client.GenerateWithTools(context.Background(), llm.GenerateRequest{
		SystemPrompt: "You are helpful.",
		UserMessage:  "Do two things.",
		Tools: []source.ToolDefinition{
			{Name: "tool_ok", Description: "OK"},
			{Name: "tool_fail", Description: "Fail"},
		},
		ToolExecutor: func(_ context.Context, tool string, params map[string]any) (map[string]any, error) {
			if tool == "tool_fail" {
				return nil, fmt.Errorf("connection refused")
			}
			return map[string]any{"status": "ok"}, nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "handled error" {
		t.Errorf("unexpected text: %q", resp.Text)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Tool != "tool_ok" {
		t.Errorf("expected tool_ok first, got %s", resp.ToolCalls[0].Tool)
	}
	if resp.ToolCalls[1].Tool != "tool_fail" {
		t.Errorf("expected tool_fail second, got %s", resp.ToolCalls[1].Tool)
	}
}

func TestAnthropicClient_DefaultModel(t *testing.T) {
	server := mockAnthropicServer(anthropicTextResponse("ok"))
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "") // empty model = default
	client.SetBaseURL(server.URL)

	_, err := client.GenerateWithTools(context.Background(), llm.GenerateRequest{
		UserMessage: "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
