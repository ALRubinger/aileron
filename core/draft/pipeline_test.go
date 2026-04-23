package draft_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/draft"
	"github.com/ALRubinger/aileron/core/llm"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/source"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
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

	// Fix the clock to a Thursday so "this week" spans Mon–Thu with date gaps.
	fixedNow := time.Date(2026, 4, 16, 14, 0, 0, 0, time.UTC) // Thursday Apr 16
	p := draft.NewPipeline(researchMock, researchMock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research\n\nToday is {{today}}.", Ghostwrite: "test ghostwrite"}).
		WithClock(func() time.Time { return fixedNow })

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

	// Fix clock to Thursday so "this week" spans Mon–Thu with date gaps,
	// ensuring we actually reach the replayable-tools check.
	fixedNow := time.Date(2026, 4, 16, 14, 0, 0, 0, time.UTC) // Thursday Apr 16
	p := draft.NewPipeline(researchMock, researchMock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research\n\nToday is {{today}}.", Ghostwrite: "test ghostwrite"}).
		WithClock(func() time.Time { return fixedNow })

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

	// Fix clock to Friday so "this week" spans Mon–Fri, and all days are mentioned.
	fixedNow := time.Date(2026, 4, 17, 14, 0, 0, 0, time.UTC) // Friday Apr 17
	p := draft.NewPipeline(mock, mock, sourceReg, accounts, mem.NewUserInstructionStore(), v, slog.Default(), draft.Prompts{Research: "test research\n\nToday is {{today}}.", Ghostwrite: "test ghostwrite"}).
		WithClock(func() time.Time { return fixedNow })

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

// mockStreamingLLMClient implements both llm.Client and llm.StreamingClient.
type mockStreamingLLMClient struct {
	mockLLMClient
	streamChunks []string
	streamErr    error
}

func (m *mockStreamingLLMClient) GenerateStream(_ context.Context, req llm.GenerateRequest) (<-chan string, <-chan error) {
	textCh := make(chan string, len(m.streamChunks))
	errCh := make(chan error, 1)

	go func() {
		defer close(textCh)
		defer close(errCh)
		for _, chunk := range m.streamChunks {
			textCh <- chunk
		}
		if m.streamErr != nil {
			errCh <- m.streamErr
		}
	}()

	return textCh, errCh
}

func TestPipeline_GenerateDraftStream_Streaming(t *testing.T) {
	researchMock := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "No tools available."},
	}
	streamMock := &mockStreamingLLMClient{
		streamChunks: []string{"Hello", " ", "world"},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()

	p := draft.NewPipeline(researchMock, streamMock, source.NewRegistry(), accounts,
		mem.NewUserInstructionStore(), v, slog.Default(),
		draft.Prompts{Research: "test", Ghostwrite: "test"})

	chunkCh, errCh := p.GenerateDraftStream(context.Background(), "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#test", Author: "Bob", Body: "test",
	})

	var chunks []draft.DraftChunk
	for chunk := range chunkCh {
		chunks = append(chunks, chunk)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have: researching phase, writing phase, then 3 text chunks.
	if len(chunks) < 5 {
		t.Fatalf("expected at least 5 chunks (2 phase + 3 text), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Phase != "researching" {
		t.Errorf("expected first chunk phase=researching, got %q", chunks[0].Phase)
	}

	// Find the writing phase marker.
	writingIdx := -1
	for i, c := range chunks {
		if c.Phase == "writing" && c.Text == "" {
			writingIdx = i
			break
		}
	}
	if writingIdx < 0 {
		t.Fatal("expected a writing phase marker chunk")
	}

	// Collect text after writing marker.
	var text strings.Builder
	for _, c := range chunks[writingIdx+1:] {
		text.WriteString(c.Text)
	}
	if text.String() != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", text.String())
	}
}

func TestPipeline_GenerateDraftStream_Fallback(t *testing.T) {
	// Non-streaming client — should fall back to buffered output.
	mock := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "Buffered response"},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()

	p := draft.NewPipeline(mock, mock, source.NewRegistry(), accounts,
		mem.NewUserInstructionStore(), v, slog.Default(),
		draft.Prompts{Research: "test", Ghostwrite: "test"})

	chunkCh, errCh := p.GenerateDraftStream(context.Background(), "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#test", Author: "Bob", Body: "test",
	})

	var chunks []draft.DraftChunk
	for chunk := range chunkCh {
		chunks = append(chunks, chunk)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have: researching, writing marker, then one buffered text chunk.
	var text string
	for _, c := range chunks {
		if c.Text != "" {
			text += c.Text
		}
	}
	if text != "Buffered response" {
		t.Errorf("expected 'Buffered response', got %q", text)
	}
}

func TestPipeline_GenerateDraftStream_WithTools(t *testing.T) {
	// Research client that returns tool calls; ghostwrite client that streams.
	research := &mockLLMClient{
		researchResp: &llm.GenerateResponse{
			Text:      "Found context via tools.",
			ToolCalls: []llm.ToolCall{{Tool: "slack_channel_history"}},
		},
	}
	ghostwrite := &mockStreamingLLMClient{
		streamChunks: []string{"Based ", "on research."},
	}

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	ctx := context.Background()

	// Seed a connected account + source connector so tools are available.
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_1", Provider: "slack",
		Status: model.ConnectedAccountStatusActive,
	})
	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})
	v.Put(ctx, "connected-accounts/usr_1/slack", []byte(`{"access_token":"test"}`), vault.Metadata{})

	p := draft.NewPipeline(research, ghostwrite, sourceReg, accounts,
		mem.NewUserInstructionStore(), v, slog.Default(),
		draft.Prompts{Research: "research {{today}}", Ghostwrite: "ghostwrite"})

	chunkCh, errCh := p.GenerateDraftStream(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Bob", Body: "What happened this week?",
	})

	var chunks []draft.DraftChunk
	for c := range chunkCh {
		chunks = append(chunks, c)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify we got researching phase, writing phase, and text chunks.
	var text strings.Builder
	for _, c := range chunks {
		text.WriteString(c.Text)
	}
	if !strings.Contains(text.String(), "Based on research.") {
		t.Errorf("expected streamed text, got %q", text.String())
	}
}

func TestPipeline_GenerateDraftStream_StreamError(t *testing.T) {
	research := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "ok"},
	}
	ghostwrite := &mockStreamingLLMClient{
		streamErr: fmt.Errorf("stream exploded"),
	}

	p := draft.NewPipeline(research, ghostwrite, source.NewRegistry(),
		mem.NewConnectedAccountStore(), mem.NewUserInstructionStore(),
		vault.NewMemVault(), slog.Default(),
		draft.Prompts{Research: "r", Ghostwrite: "g"})

	chunkCh, errCh := p.GenerateDraftStream(context.Background(), "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#test", Author: "Bob", Body: "test",
	})

	for range chunkCh {
	}
	err := <-errCh
	if err == nil {
		t.Fatal("expected error from streaming failure")
	}
	if !strings.Contains(err.Error(), "stream exploded") {
		t.Errorf("expected 'stream exploded' in error, got %q", err.Error())
	}
}

func TestPipeline_GenerateDraftStream_ResearchError(t *testing.T) {
	// Research client with tools that fails — use err field which fails all calls.
	research := &mockLLMClient{
		err: fmt.Errorf("research failed"),
	}
	ghostwrite := &mockStreamingLLMClient{
		streamChunks: []string{"draft output"},
	}

	accounts := mem.NewConnectedAccountStore()
	ctx := context.Background()
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_s1", UserID: "usr_1", Provider: "slack",
		Status: model.ConnectedAccountStatusActive,
	})
	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})
	v := vault.NewMemVault()
	v.Put(ctx, "connected-accounts/usr_1/slack", []byte(`{"access_token":"test"}`), vault.Metadata{})

	p := draft.NewPipeline(research, ghostwrite, sourceReg, accounts,
		mem.NewUserInstructionStore(), v, slog.Default(),
		draft.Prompts{Research: "r", Ghostwrite: "g"})

	chunkCh, errCh := p.GenerateDraftStream(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#test", Author: "Bob", Body: "test",
	})

	var chunks []draft.DraftChunk
	for c := range chunkCh {
		chunks = append(chunks, c)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v (research failure should be handled gracefully)", err)
	}
	// Research failure is logged but pipeline continues with fallback context.
	var text string
	for _, c := range chunks {
		text += c.Text
	}
	if text == "" {
		t.Error("expected draft output despite research failure")
	}
}

func TestRefineDraft_LLMError(t *testing.T) {
	mockClient := &mockLLMClient{
		err: fmt.Errorf("LLM unavailable"),
	}
	p := draft.NewPipeline(
		mockClient, mockClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		nil, vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)

	_, err := p.RefineDraft(context.Background(), "usr_1",
		"Original", "Draft", "Feedback",
	)
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "refine draft failed") {
		t.Errorf("expected 'refine draft failed' in error, got: %v", err)
	}
}

func TestWithVault(t *testing.T) {
	mockClient := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "ok"},
	}
	p := draft.NewPipeline(
		mockClient, mockClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		nil, vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)

	v := vault.NewMemVault()
	p2 := p.WithVault(v)
	if p2 == nil {
		t.Fatal("WithVault returned nil")
	}
	// Original pipeline should not be affected.
	_ = p
}

func TestWithClientResolver(t *testing.T) {
	mockClient := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "ok"},
	}
	p := draft.NewPipeline(
		mockClient, mockClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		nil, vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)

	p2 := p.WithClientResolver(nil)
	if p2 == nil {
		t.Fatal("WithClientResolver returned nil")
	}
}

// failingInstructionStore always returns an error on List.
type failingInstructionStore struct{}

func (f *failingInstructionStore) Create(_ context.Context, _ model.UserInstruction) error {
	return nil
}
func (f *failingInstructionStore) Get(_ context.Context, _ string) (model.UserInstruction, error) {
	return model.UserInstruction{}, nil
}
func (f *failingInstructionStore) List(_ context.Context, _ store.UserInstructionFilter) ([]model.UserInstruction, error) {
	return nil, fmt.Errorf("instruction store unavailable")
}
func (f *failingInstructionStore) Update(_ context.Context, _ model.UserInstruction) error {
	return nil
}
func (f *failingInstructionStore) Delete(_ context.Context, _ string) error { return nil }

func TestRefineDraft_AssemblePromptError(t *testing.T) {
	mockClient := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "ok"},
	}
	p := draft.NewPipeline(
		mockClient, mockClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		&failingInstructionStore{}, vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)

	_, err := p.RefineDraft(context.Background(), "usr_1",
		"Original", "Draft", "Feedback",
	)
	if err == nil {
		t.Fatal("expected error from assembleSystemPrompt failure")
	}
	if !strings.Contains(err.Error(), "assembling system prompt") {
		t.Errorf("expected 'assembling system prompt' in error, got: %v", err)
	}
}

func TestRefineDraft_ClientResolverError(t *testing.T) {
	mockClient := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "ok"},
	}
	p := draft.NewPipeline(
		mockClient, mockClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		nil, vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)

	// Create a resolver that will fail (no default API key, no per-user config).
	resolver := llm.NewClientResolver(
		mem.NewLLMConfigStore(),
		&stubUserStoreForResolver{},
		vault.NewMemVault(),
		"", "", "", // empty defaults → will fail
		slog.Default(),
	)
	p2 := p.WithClientResolver(resolver)

	_, err := p2.RefineDraft(context.Background(), "usr_1",
		"Original", "Draft", "Feedback",
	)
	if err == nil {
		t.Fatal("expected error from client resolver")
	}
	if !strings.Contains(err.Error(), "resolving LLM config") {
		t.Errorf("expected 'resolving LLM config' in error, got: %v", err)
	}
}

// stubUserStoreForResolver satisfies store.UserStore for the resolver.
type stubUserStoreForResolver struct{}

func (s *stubUserStoreForResolver) Create(_ context.Context, _ model.User) error { return nil }
func (s *stubUserStoreForResolver) Get(_ context.Context, _ string) (model.User, error) {
	return model.User{}, nil
}
func (s *stubUserStoreForResolver) GetByEmail(_ context.Context, _ string) (model.User, error) {
	return model.User{}, nil
}
func (s *stubUserStoreForResolver) List(_ context.Context, _ store.UserFilter) ([]model.User, error) {
	return nil, nil
}
func (s *stubUserStoreForResolver) Update(_ context.Context, _ model.User) error { return nil }

func TestRefineDraft(t *testing.T) {
	mockClient := &mockLLMClient{
		response: &llm.GenerateResponse{Text: "Revised: shorter version"},
	}
	p := draft.NewPipeline(
		mockClient, mockClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		nil, vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)

	result, err := p.RefineDraft(context.Background(), "usr_1",
		"Original message text",
		"Here is my previous long draft...",
		"Make it more concise",
	)
	if err != nil {
		t.Fatalf("RefineDraft: %v", err)
	}
	if result != "Revised: shorter version" {
		t.Errorf("result = %q, want 'Revised: shorter version'", result)
	}

	// Verify the LLM received the right context.
	if mockClient.lastRequest == nil {
		t.Fatal("expected LLM call")
	}
	msg := mockClient.lastRequest.UserMessage
	if !strings.Contains(msg, "Previous draft") {
		t.Errorf("expected 'Previous draft' in prompt, got: %s", msg)
	}
	if !strings.Contains(msg, "Make it more concise") {
		t.Errorf("expected feedback in prompt, got: %s", msg)
	}
	if !strings.Contains(msg, "Original message text") {
		t.Errorf("expected original message in prompt, got: %s", msg)
	}
}

// stubEnclaveClient implements enclave.Client for testing stale escrow in the pipeline.
type stubEnclaveClient struct {
	retrieveErr error
}

func (s *stubEnclaveClient) Attest(_ context.Context, _ enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	return enclave.AttestationResponse{}, nil
}
func (s *stubEnclaveClient) EstablishSession(_ context.Context, _ enclave.SessionRequest) (enclave.SessionResponse, error) {
	return enclave.SessionResponse{}, nil
}
func (s *stubEnclaveClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{}, nil
}
func (s *stubEnclaveClient) OAuthExchange(_ context.Context, _ enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	return enclave.OAuthExchangeResponse{}, nil
}
func (s *stubEnclaveClient) Execute(_ context.Context, _ enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	return enclave.ExecuteResponse{}, nil
}
func (s *stubEnclaveClient) EscrowStore(_ context.Context, _ enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	return enclave.EscrowStoreResponse{}, nil
}
func (s *stubEnclaveClient) EscrowRetrieve(_ context.Context, _ enclave.EscrowRetrieveRequest) (enclave.EscrowRetrieveResponse, error) {
	return enclave.EscrowRetrieveResponse{}, s.retrieveErr
}
func (s *stubEnclaveClient) EscrowList(_ context.Context) (enclave.EscrowListResponse, error) {
	return enclave.EscrowListResponse{}, nil
}
func (s *stubEnclaveClient) EscrowRevoke(_ context.Context, _ enclave.EscrowRevokeRequest) error {
	return nil
}
func (s *stubEnclaveClient) Close() error { return nil }

func TestPipeline_GenerateDraft_ToolExecutor_StaleEscrow(t *testing.T) {
	// When the escrow vault returns ErrEscrowStale, the tool executor should
	// return a user-actionable message telling Claude to ask the user to
	// unlock their vault, rather than a raw error stack trace.
	mock := &mockLLMClient{
		researchResp:   &llm.GenerateResponse{Text: "context gathered"},
		ghostwriteResp: &llm.GenerateResponse{Text: "draft"},
	}

	accounts := mem.NewConnectedAccountStore()
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})

	// Build an EscrowVault with a failing enclave client and a stale index entry.
	idx := &sync.Map{}
	idx.Store("connected-accounts/usr_1/slack", "esc_stale_123")
	escrowVault := vault.NewEscrowVault(
		&stubEnclaveClient{retrieveErr: enclave.ErrEscrowNotFound},
		idx,
		vault.NewMemVault(),
	)

	sourceReg := source.NewRegistry()
	sourceReg.Register(&mockSourceConnector{})

	p := draft.NewPipeline(mock, mock, sourceReg, accounts, mem.NewUserInstructionStore(),
		escrowVault, slog.Default(),
		draft.Prompts{Research: "test research", Ghostwrite: "test ghostwrite"})

	_, err := p.GenerateDraft(ctx, "usr_1", comms.IncomingMessage{
		ID: "msg_1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Extract the tool executor from the research round request.
	if len(mock.requests) < 1 {
		t.Fatal("expected at least 1 LLM call")
	}
	researchReq := mock.requests[0]
	if researchReq.ToolExecutor == nil {
		t.Fatal("expected ToolExecutor in research round")
	}

	// Execute a tool — should fail with the user-actionable message.
	_, execErr := researchReq.ToolExecutor(ctx, "slack_channel_history", map[string]any{"channel": "C123"})
	if execErr == nil {
		t.Fatal("expected error from stale escrow")
	}

	errMsg := execErr.Error()
	if !strings.Contains(errMsg, "vault session has expired") {
		t.Errorf("expected user-actionable 'vault session has expired' message, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "unlock their vault") {
		t.Errorf("expected 'unlock their vault' guidance, got: %s", errMsg)
	}
	// Should NOT contain raw error wrapping that would confuse the LLM.
	if strings.Contains(errMsg, "ErrEscrowStale") {
		t.Error("error message should not expose internal sentinel error name")
	}
}
