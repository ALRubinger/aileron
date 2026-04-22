package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/llm"
)

// anthropicStreamingResponse returns an SSE stream with text deltas.
func anthropicStreamingResponse(chunks []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		// message_start
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		flusher.Flush()

		// content_block_start
		fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		flusher.Flush()

		// text deltas
		for _, chunk := range chunks {
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", chunk)
			flusher.Flush()
		}

		// content_block_stop
		fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		flusher.Flush()

		// message_delta
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")
		flusher.Flush()

		// message_stop
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}
}

func TestGenerateStream_TextDeltas(t *testing.T) {
	chunks := []string{"Hello", " world", "!"}
	server := httptest.NewServer(anthropicStreamingResponse(chunks))
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	textCh, errCh := client.GenerateStream(context.Background(), llm.GenerateRequest{
		SystemPrompt: "You are a test assistant.",
		UserMessage:  "Say hello",
	})

	var received []string
	for text := range textCh {
		received = append(received, text)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(received) != 3 {
		t.Fatalf("expected 3 chunks, got %d: %v", len(received), received)
	}
	if received[0] != "Hello" || received[1] != " world" || received[2] != "!" {
		t.Errorf("unexpected chunks: %v", received)
	}
}

func TestGenerateStream_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(anthropicStreamingResponse(nil))
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	textCh, errCh := client.GenerateStream(context.Background(), llm.GenerateRequest{
		UserMessage: "Say nothing",
	})

	var received []string
	for text := range textCh {
		received = append(received, text)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(received) != 0 {
		t.Errorf("expected 0 chunks, got %d", len(received))
	}
}

func TestGenerateStream_ContextCancellation(t *testing.T) {
	// Server that blocks forever — we cancel the context.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		flusher.Flush()
		// Block until client disconnects.
		<-r.Context().Done()
	}))
	defer server.Close()

	client := llm.NewAnthropicClient("test-key", "claude-sonnet-4-6")
	client.SetBaseURL(server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	textCh, errCh := client.GenerateStream(ctx, llm.GenerateRequest{
		UserMessage: "Will be cancelled",
	})

	// Cancel immediately.
	cancel()

	// Drain channels.
	for range textCh {
	}
	// Error may or may not be present depending on timing — just don't hang.
	<-errCh
}

// Verify AnthropicClient implements StreamingClient.
var _ llm.StreamingClient = (*llm.AnthropicClient)(nil)
