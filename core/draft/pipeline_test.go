package draft_test

import (
	"context"
	"log/slog"
	"strings"
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

	p := draft.NewPipeline(mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), "test system prompt")

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

	p := draft.NewPipeline(mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), "test system prompt")

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

	p := draft.NewPipeline(mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), "test system prompt")

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

	p := draft.NewPipeline(mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), "test system prompt")

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

func TestPipeline_GenerateDraft_WithInstructions(t *testing.T) {
	mock := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "Draft with instructions"},
	}

	accounts := mem.NewConnectedAccountStore()
	instructions := mem.NewUserInstructionStore()
	v := vault.NewMemVault()
	ctx := context.Background()

	// Seed instructions.
	instructions.Create(ctx, model.UserInstruction{
		ID: "ins_1", UserID: "usr_1", Body: "Always reference PR numbers", Scope: "#backend", Active: true,
	})
	instructions.Create(ctx, model.UserInstruction{
		ID: "ins_2", UserID: "usr_1", Body: "Be brief in incidents", Active: true,
	})
	instructions.Create(ctx, model.UserInstruction{
		ID: "ins_3", UserID: "usr_1", Body: "Inactive rule", Active: false, // should NOT appear
	})

	sourceReg := source.NewRegistry()
	p := draft.NewPipeline(mock, sourceReg, accounts, instructions, v, slog.Default(), "test system prompt")

	_, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify instructions were included in the system prompt.
	prompt := mock.lastRequest.SystemPrompt
	if !strings.Contains(prompt, "Always reference PR numbers") {
		t.Error("expected instruction 1 in prompt")
	}
	if !strings.Contains(prompt, "[scope: #backend]") {
		t.Error("expected scope annotation in prompt")
	}
	if !strings.Contains(prompt, "Be brief in incidents") {
		t.Error("expected instruction 2 in prompt")
	}
	if strings.Contains(prompt, "Inactive rule") {
		t.Error("inactive instruction should not appear in prompt")
	}
	if !strings.Contains(prompt, "User Instructions") {
		t.Error("expected User Instructions section header")
	}
}

func TestPipeline_GenerateDraft_NoInstructions(t *testing.T) {
	mock := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "Draft without instructions"},
	}

	p := draft.NewPipeline(mock, source.NewRegistry(), mem.NewConnectedAccountStore(), mem.NewUserInstructionStore(), vault.NewMemVault(), slog.Default(), "test system prompt")

	_, err := p.GenerateDraft(context.Background(), "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// System prompt should NOT contain instructions section.
	if strings.Contains(mock.lastRequest.SystemPrompt, "User Instructions") {
		t.Error("expected no User Instructions section when none exist")
	}
}

func TestPipeline_GenerateDraft_LLMError(t *testing.T) {
	mock := &mockLLMClient{
		err: context.DeadlineExceeded,
	}

	p := draft.NewPipeline(mock, source.NewRegistry(), mem.NewConnectedAccountStore(), mem.NewUserInstructionStore(), vault.NewMemVault(), slog.Default(), "test system prompt")

	_, err := p.GenerateDraft(context.Background(), "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})

	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
}
