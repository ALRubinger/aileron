package draft_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/draft"
	"github.com/ALRubinger/aileron/core/llm"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/source"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

// mockLLMClient records requests and returns a configured response.
type mockLLMClient struct {
	lastRequest *llm.GenerateRequest
	response    *llm.GenerateResponse
	err         error
}

func (m *mockLLMClient) GenerateWithTools(_ context.Context, req llm.GenerateRequest) (*llm.GenerateResponse, error) {
	m.lastRequest = &req
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

// mockSourceConnector for testing tool execution through the pipeline.
type mockSourceConnector struct{}

func (m *mockSourceConnector) Provider() string { return "slack" }
func (m *mockSourceConnector) Tools() []source.ToolDefinition {
	return []source.ToolDefinition{
		{Name: "slack_channel_history", Description: "Get channel messages"},
	}
}
func (m *mockSourceConnector) Execute(_ context.Context, tool string, params map[string]any, _ []byte) (map[string]any, error) {
	return map[string]any{"tool": tool, "executed": true}, nil
}

func TestPipeline_GenerateDraft_Simple(t *testing.T) {
	mock := &mockLLMClient{
		response: &llm.GenerateResponse{
			Text: "The JWT claims structure isn't changing. PR #247 only moves validation into middleware.",
		},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	sourceReg := source.NewRegistry()

	p := draft.NewPipeline(mock, sourceReg, accounts, v, slog.Default())

	draftText, err := p.GenerateDraft(context.Background(), "usr_1", comms.IncomingMessage{
		ID:      "msg_1",
		Service: "slack",
		Channel: "#backend",
		Author:  "Sarah",
		Body:    "Does the JWT refactor change the claims structure?",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draftText == "" {
		t.Fatal("expected non-empty draft")
	}
	if draftText != "The JWT claims structure isn't changing. PR #247 only moves validation into middleware." {
		t.Errorf("unexpected draft: %s", draftText)
	}

	// Verify the system prompt was included.
	if mock.lastRequest.SystemPrompt == "" {
		t.Error("expected system prompt to be set")
	}

	// Verify the user message includes the original message.
	if mock.lastRequest.UserMessage == "" {
		t.Error("expected user message to be set")
	}
}

func TestPipeline_GenerateDraft_WithTools(t *testing.T) {
	mock := &mockLLMClient{
		response: &llm.GenerateResponse{
			Text:      "Based on the channel history, the answer is yes.",
			ToolCalls: []llm.ToolCall{{Tool: "slack_channel_history"}},
		},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	ctx := context.Background()

	// Seed a connected Slack account.
	accounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	v.Put(ctx, "connected-accounts/usr_1/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{})

	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})

	p := draft.NewPipeline(mock, sourceReg, accounts, v, slog.Default())

	draftText, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID:      "msg_1",
		Service: "slack",
		Channel: "#backend",
		Author:  "Sarah",
		Body:    "What happened in this channel today?",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draftText == "" {
		t.Fatal("expected non-empty draft")
	}

	// Verify tools were provided to the LLM.
	if len(mock.lastRequest.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(mock.lastRequest.Tools))
	}
	if mock.lastRequest.Tools[0].Name != "slack_channel_history" {
		t.Errorf("expected slack_channel_history, got %s", mock.lastRequest.Tools[0].Name)
	}

	// Verify tool executor was provided.
	if mock.lastRequest.ToolExecutor == nil {
		t.Error("expected ToolExecutor to be set")
	}
}

func TestPipeline_GenerateDraft_ToolExecutor(t *testing.T) {
	// This test verifies the tool executor resolves credentials and calls the source connector.
	var executorCalled bool
	mock := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "draft"},
	}

	// Override: capture the executor and call it ourselves.
	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	v.Put(ctx, "connected-accounts/usr_1/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{})

	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})

	p := draft.NewPipeline(mock, sourceReg, accounts, v, slog.Default())

	_, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Now call the tool executor that was passed to the LLM.
	if mock.lastRequest.ToolExecutor != nil {
		result, execErr := mock.lastRequest.ToolExecutor(ctx, "slack_channel_history", map[string]any{"channel": "C123"})
		if execErr != nil {
			t.Fatalf("tool executor error: %v", execErr)
		}
		if result["executed"] != true {
			t.Error("expected tool to be executed")
		}
		executorCalled = true
	}
	if !executorCalled {
		t.Error("expected executor to be callable")
	}
}

func TestPipeline_GenerateDraft_NoConnectedAccounts(t *testing.T) {
	mock := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "I don't have enough context to answer."},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})

	p := draft.NewPipeline(mock, sourceReg, accounts, v, slog.Default())

	draftText, err := p.GenerateDraft(context.Background(), "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should still work — just no tools available.
	if draftText == "" {
		t.Fatal("expected non-empty draft")
	}
	if len(mock.lastRequest.Tools) != 0 {
		t.Errorf("expected 0 tools without connected accounts, got %d", len(mock.lastRequest.Tools))
	}
}

func TestPipeline_GenerateDraft_LLMError(t *testing.T) {
	mock := &mockLLMClient{
		err: context.DeadlineExceeded,
	}

	p := draft.NewPipeline(mock, source.NewRegistry(), mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	_, err := p.GenerateDraft(context.Background(), "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})

	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
}
