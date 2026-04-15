package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
