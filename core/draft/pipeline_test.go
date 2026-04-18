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

// mockLLMClient records requests and returns configured responses.
// Supports two-round pipeline: first call is research, second is ghostwrite.
type mockLLMClient struct {
	requests       []*llm.GenerateRequest
	lastRequest    *llm.GenerateRequest
	researchResp   *llm.GenerateResponse // response for round 1 (with tools)
	ghostwriteResp *llm.GenerateResponse // response for round 2 (no tools)
	response       *llm.GenerateResponse // fallback if specific round responses not set
	err            error
}

func (m *mockLLMClient) GenerateWithTools(_ context.Context, req llm.GenerateRequest) (*llm.GenerateResponse, error) {
	m.requests = append(m.requests, &req)
	m.lastRequest = &req
	if m.err != nil {
		return nil, m.err
	}
	// If specific round responses are set, use them based on whether tools are present.
	if len(req.Tools) > 0 && m.researchResp != nil {
		return m.researchResp, nil
	}
	if len(req.Tools) == 0 && m.ghostwriteResp != nil {
		return m.ghostwriteResp, nil
	}
	return m.response, nil
}

// mockSourceConnector for testing tool execution through the pipeline.
type mockSourceConnector struct{}

func (m *mockSourceConnector) Provider() string { return "slack" }
func (m *mockSourceConnector) Tools() []source.ToolDefinition {
	return []source.ToolDefinition{
		{
			Name:        "slack_channel_history",
			Description: "Get channel messages",
			Parameters: []source.ToolParam{
				{Name: "channel", Type: "string", Required: true},
				{Name: "after", Type: "string", Required: false},
				{Name: "before", Type: "string", Required: false},
			},
		},
		{
			Name:        "slack_search_messages",
			Description: "Search messages",
			Parameters: []source.ToolParam{
				{Name: "query", Type: "string", Required: true},
				{Name: "after", Type: "string", Required: false},
				{Name: "before", Type: "string", Required: false},
			},
		},
	}
}
func (m *mockSourceConnector) Execute(_ context.Context, tool string, params map[string]any, _ []byte) (map[string]any, error) {
	return map[string]any{"tool": tool, "params": params, "executed": true}, nil
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

	p := draft.NewPipeline(mock, mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

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
		researchResp: &llm.GenerateResponse{
			Text:      "Found PR #247 which refactors JWT validation.",
			ToolCalls: []llm.ToolCall{{Tool: "slack_channel_history"}},
		},
		ghostwriteResp: &llm.GenerateResponse{
			Text: "Based on the channel history, the answer is yes.",
		},
	}

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

	p := draft.NewPipeline(mock, mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

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

	// Two-round pipeline: should have made 2 LLM calls.
	if len(mock.requests) != 2 {
		t.Fatalf("expected 2 LLM calls (research + ghostwrite), got %d", len(mock.requests))
	}

	// Round 1 (research) should have tools.
	if len(mock.requests[0].Tools) != 2 {
		t.Fatalf("expected 2 tools in research round, got %d", len(mock.requests[0].Tools))
	}
	if mock.requests[0].ToolExecutor == nil {
		t.Error("expected ToolExecutor in research round")
	}

	// Round 2 (ghostwrite) should have NO tools.
	if len(mock.requests[1].Tools) != 0 {
		t.Errorf("expected 0 tools in ghostwrite round, got %d", len(mock.requests[1].Tools))
	}
}

func TestPipeline_GenerateDraft_SeparateClients(t *testing.T) {
	// Verify the pipeline dispatches research to one client and ghostwriting
	// to another — enabling a fast model for research and a capable model
	// for composition.
	researchMock := &mockLLMClient{
		response: &llm.GenerateResponse{
			Text: "Context gathered from Slack and GitHub.",
		},
	}
	ghostwriteMock := &mockLLMClient{
		response: &llm.GenerateResponse{
			Text: "Deployed the new auth middleware on Tuesday.",
		},
	}

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

	p := draft.NewPipeline(researchMock, ghostwriteMock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

	draftText, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah",
		Body: "What's the status of the auth refactor?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draftText != "Deployed the new auth middleware on Tuesday." {
		t.Errorf("unexpected draft: %q", draftText)
	}

	// Research client should have received the tool-bearing request.
	// (No time range in message, so no backward pass — just 1 research call.)
	if len(researchMock.requests) != 1 {
		t.Fatalf("expected 1 research call, got %d", len(researchMock.requests))
	}
	if len(researchMock.requests[0].Tools) == 0 {
		t.Error("expected tools in research request")
	}

	// Ghostwrite client should have received the tool-free request.
	if len(ghostwriteMock.requests) != 1 {
		t.Fatalf("expected 1 ghostwrite call, got %d", len(ghostwriteMock.requests))
	}
	if len(ghostwriteMock.requests[0].Tools) != 0 {
		t.Errorf("expected 0 tools in ghostwrite request, got %d", len(ghostwriteMock.requests[0].Tools))
	}
	// Ghostwrite should receive the gathered context.
	if !strings.Contains(ghostwriteMock.requests[0].UserMessage, "Context gathered from Slack and GitHub.") {
		t.Error("expected gathered context in ghostwrite user message")
	}
}

func TestPipeline_GenerateDraft_ToolExecutor(t *testing.T) {
	// Verify the research round's tool executor resolves credentials
	// and calls the source connector.
	mock := &mockLLMClient{
		researchResp:   &llm.GenerateResponse{Text: "context gathered"},
		ghostwriteResp: &llm.GenerateResponse{Text: "draft"},
	}

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

	p := draft.NewPipeline(mock, mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

	_, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The tool executor is on the research round (first request).
	if len(mock.requests) < 1 {
		t.Fatal("expected at least 1 LLM call")
	}
	researchReq := mock.requests[0]
	if researchReq.ToolExecutor == nil {
		t.Fatal("expected ToolExecutor in research round")
	}

	result, execErr := researchReq.ToolExecutor(ctx, "slack_channel_history", map[string]any{"channel": "C123"})
	if execErr != nil {
		t.Fatalf("tool executor error: %v", execErr)
	}
	if result["executed"] != true {
		t.Error("expected tool to be executed")
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

	p := draft.NewPipeline(mock, mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

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
	p := draft.NewPipeline(mock, mock, sourceReg, accounts, instructions, v, slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

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

	p := draft.NewPipeline(mock, mock, source.NewRegistry(), mem.NewConnectedAccountStore(), mem.NewUserInstructionStore(), vault.NewMemVault(), slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

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

func TestPipeline_GenerateDraft_BackwardPass_ReplaysWithDates(t *testing.T) {
	// Research returns context that only mentions Monday and Friday, plus
	// the tool calls it made. The backward pass should replay those same
	// tool calls with after/before date filters for the uncovered window.
	researchMock := &mockLLMClient{
		researchResp: &llm.GenerateResponse{
			Text: "On Monday we merged PR #1. On Friday we deployed v2.3.",
			ToolCalls: []llm.ToolCall{
				{Tool: "slack_search_messages", Params: map[string]any{"query": "deploy merge"}},
				{Tool: "slack_channel_history", Params: map[string]any{"channel": "C0BACKEND"}},
			},
		},
		ghostwriteResp: &llm.GenerateResponse{
			Text: "Busy week — merged PRs, fixed CI, deployed v2.3.",
		},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	v.Put(ctx, "connected-accounts/usr_1/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{})

	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})

	p := draft.NewPipeline(researchMock, researchMock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research\n\nToday is {{today}}.", Ghostwrite: "test ghostwrite"})

	draftText, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah",
		Body: "What happened this week?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draftText == "" {
		t.Fatal("expected non-empty draft")
	}

	// Research LLM should have been called exactly twice: research + ghostwrite.
	// The backward pass does NOT call the LLM — it replays tools directly.
	if len(researchMock.requests) != 2 {
		t.Fatalf("expected 2 LLM calls (research + ghostwrite), got %d", len(researchMock.requests))
	}

	// Ghostwrite should receive both the initial context AND replayed results.
	gwMsg := researchMock.requests[1].UserMessage
	if !strings.Contains(gwMsg, "On Monday we merged PR #1") {
		t.Error("expected initial context in ghostwrite")
	}
	if !strings.Contains(gwMsg, "date-filtered replay") {
		t.Error("expected backward pass replay marker in ghostwrite context")
	}
	// The replayed tool calls should appear in results.
	if !strings.Contains(gwMsg, "slack_search_messages") {
		t.Error("expected replayed slack_search_messages in ghostwrite context")
	}
	if !strings.Contains(gwMsg, "slack_channel_history") {
		t.Error("expected replayed slack_channel_history in ghostwrite context")
	}
}

func TestPipeline_GenerateDraft_BackwardPass_NoReplayableTools(t *testing.T) {
	// Research only used non-search tools (e.g., get_pr) — nothing to replay.
	researchMock := &mockLLMClient{
		researchResp: &llm.GenerateResponse{
			Text: "On Monday we reviewed PR #5.",
			ToolCalls: []llm.ToolCall{
				{Tool: "github_get_pr", Params: map[string]any{"repo": "org/repo", "number": 5}},
			},
		},
		ghostwriteResp: &llm.GenerateResponse{Text: "We reviewed PR #5."},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	ctx := context.Background()
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	v.Put(ctx, "connected-accounts/usr_1/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{})

	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})

	p := draft.NewPipeline(researchMock, researchMock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research\n\nToday is {{today}}.", Ghostwrite: "test ghostwrite"})

	_, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah",
		Body: "What happened this week?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No replayable search calls → no backward pass, just research + ghostwrite.
	if len(researchMock.requests) != 2 {
		t.Errorf("expected 2 LLM calls, got %d", len(researchMock.requests))
	}
	// Ghostwrite should NOT contain replay marker.
	gwMsg := researchMock.requests[1].UserMessage
	if strings.Contains(gwMsg, "date-filtered replay") {
		t.Error("expected no replay when no search tools used")
	}
}

func TestPipeline_GenerateDraft_BackwardPass_SkipsWhenFullCoverage(t *testing.T) {
	// Research mentions all days — no backward pass needed.
	mock := &mockLLMClient{
		researchResp: &llm.GenerateResponse{
			Text: "Monday: standup. Tuesday: PR review. Wednesday: deploy. Thursday: bug fix. Friday: retro.",
			ToolCalls: []llm.ToolCall{
				{Tool: "slack_search_messages", Params: map[string]any{"query": "standup deploy"}},
			},
		},
		ghostwriteResp: &llm.GenerateResponse{Text: "Full week summary."},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	ctx := context.Background()
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	v.Put(ctx, "connected-accounts/usr_1/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{})

	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})

	p := draft.NewPipeline(mock, mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research\n\nToday is {{today}}.", Ghostwrite: "test ghostwrite"})

	_, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah",
		Body: "What happened this week?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only 2 LLM calls (research + ghostwrite) — no backward pass.
	if len(mock.requests) != 2 {
		t.Errorf("expected 2 LLM calls, got %d", len(mock.requests))
	}

	// Ghostwrite context should NOT contain replay marker.
	gwMsg := mock.requests[1].UserMessage
	if strings.Contains(gwMsg, "date-filtered replay") {
		t.Error("expected no replay when full coverage")
	}
}

func TestPipeline_GenerateDraft_BackwardPass_SkipsNoTimeRange(t *testing.T) {
	// Message doesn't imply a time range — no backward pass.
	mock := &mockLLMClient{
		researchResp:   &llm.GenerateResponse{Text: "The auth refactor is in PR #247."},
		ghostwriteResp: &llm.GenerateResponse{Text: "PR #247."},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	ctx := context.Background()
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	v.Put(ctx, "connected-accounts/usr_1/slack", []byte(`{"access_token":"xoxp-test"}`), vault.Metadata{})

	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})

	p := draft.NewPipeline(mock, mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

	_, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah",
		Body: "What's the status of the auth refactor?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.requests) != 2 {
		t.Errorf("expected 2 LLM calls (research + ghostwrite), got %d", len(mock.requests))
	}
}

func TestPipeline_GenerateDraft_LLMError(t *testing.T) {
	mock := &mockLLMClient{
		err: context.DeadlineExceeded,
	}

	p := draft.NewPipeline(mock, mock, source.NewRegistry(), mem.NewConnectedAccountStore(), mem.NewUserInstructionStore(), vault.NewMemVault(), slog.Default(), draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

	_, err := p.GenerateDraft(context.Background(), "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})

	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
}
