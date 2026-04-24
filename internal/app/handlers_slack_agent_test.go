package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/llm"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/enclave"
)

// mockSlackAgentClient records calls for assertions.
type mockSlackAgentClient struct {
	mu              sync.Mutex
	statusCalls     []string
	promptsCalls    int
	titleCalls      []string
	startStreamCalls int
	appendCalls     []string
	stopStreamCalls  int
	messageCalls    []string
}

func (m *mockSlackAgentClient) SetStatus(_ context.Context, _, _, _, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusCalls = append(m.statusCalls, status)
	return nil
}

func (m *mockSlackAgentClient) SetSuggestedPrompts(_ context.Context, _, _, _ string, _ []comms.SlackAgentPrompt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.promptsCalls++
	return nil
}

func (m *mockSlackAgentClient) SetTitle(_ context.Context, _, _, _, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.titleCalls = append(m.titleCalls, title)
	return nil
}

func (m *mockSlackAgentClient) StartStream(_ context.Context, _, _, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startStreamCalls++
	return "stream_ts_001", nil
}

func (m *mockSlackAgentClient) AppendStream(_ context.Context, _, _, _, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendCalls = append(m.appendCalls, text)
	return nil
}

func (m *mockSlackAgentClient) StopStream(_ context.Context, _, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopStreamCalls++
	return nil
}

func (m *mockSlackAgentClient) PostMessage(_ context.Context, _, _, _, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messageCalls = append(m.messageCalls, text)
	return nil
}

func newAgentTestServer() (*apiServer, *mockSlackAgentClient) {
	agentClient := &mockSlackAgentClient{}
	srv := &apiServer{
		log:               slog.Default(),
		connectedAccounts: mem.NewConnectedAccountStore(),
		systemVault:       vault.NewMemVault(),
		vault:             vault.NewMemVault(),
		slackAgentClient:  agentClient,
		newID:             func() string { return "test-id" },
	}
	return srv, agentClient
}

func TestHandleAssistantThreadStarted_SetsPromptsAndTitle(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()

	// Seed bot token.
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})

	srv.handleAssistantThreadStarted(ctx, "T001", "D_CHANNEL", "999.001")

	if agent.promptsCalls != 1 {
		t.Errorf("expected 1 SetSuggestedPrompts call, got %d", agent.promptsCalls)
	}
	if len(agent.titleCalls) != 1 || agent.titleCalls[0] != "Aileron" {
		t.Errorf("expected title 'Aileron', got %v", agent.titleCalls)
	}
}

func TestHandleAssistantThreadStarted_NoBotToken(t *testing.T) {
	srv, agent := newAgentTestServer()
	// No bot token seeded — should not panic, just log error.
	srv.handleAssistantThreadStarted(context.Background(), "T_UNKNOWN", "D_CHAN", "999.001")

	if agent.promptsCalls != 0 {
		t.Error("expected no calls when bot token missing")
	}
}

func TestHandleAssistantMessage_NoPipeline(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: "usr_a", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})

	// No draftPipeline configured.
	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "Draft something")

	// Should clear status (empty string) when pipeline is nil.
	agent.mu.Lock()
	defer agent.mu.Unlock()
	found := false
	for _, s := range agent.statusCalls {
		if s == "" {
			found = true
		}
	}
	if !found {
		t.Error("expected status to be cleared when pipeline is nil")
	}
}

func TestHandleAssistantMessage_NoUser(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	// No connected accounts — user lookup will fail.

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_UNKNOWN", "Draft something")

	agent.mu.Lock()
	defer agent.mu.Unlock()
	// Should clear status after user resolution failure.
	found := false
	for _, s := range agent.statusCalls {
		if s == "" {
			found = true
		}
	}
	if !found {
		t.Error("expected status to be cleared on user resolution failure")
	}
}

func TestIsDraftRequest(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"Draft me a weekly status update", true},
		{"Write a message to the team", true},
		{"Compose an email", true},
		{"Reply to Sarah's question", true},
		{"How many hours on calls today?", false},
		{"What's the status of PR #42?", false},
		{"Summarize the meeting", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isDraftRequest(tt.text); got != tt.want {
			t.Errorf("isDraftRequest(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestResolveAileronUserBySlack(t *testing.T) {
	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: "usr_alice", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_ALICE", ExternalTeamID: "T001",
	})
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_2", UserID: "usr_bob", Provider: model.ConnectedAccountProviderSlack,
		Status: model.ConnectedAccountStatusActive, ExternalUserID: "U_BOB", ExternalTeamID: "T001",
	})

	userID, err := srv.resolveAileronUserBySlack(ctx, "U_ALICE", "T001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if userID != "usr_alice" {
		t.Errorf("expected usr_alice, got %q", userID)
	}

	_, err = srv.resolveAileronUserBySlack(ctx, "U_UNKNOWN", "T001")
	if err == nil {
		t.Fatal("expected error for unknown user")
	}
}

func TestResolveWorkspaceBotToken(t *testing.T) {
	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-bot"), vault.Metadata{})

	token, err := srv.resolveWorkspaceBotToken(ctx, "T001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "xoxb-bot" {
		t.Errorf("expected xoxb-bot, got %q", token)
	}

	_, err = srv.resolveWorkspaceBotToken(ctx, "T_UNKNOWN")
	if err == nil {
		t.Fatal("expected error for unknown team")
	}
}

func TestParseDraftModalMeta(t *testing.T) {
	meta := DraftModalMeta{
		TargetChannel:  "C123",
		TargetThreadTS: "111.222",
		OriginalMessage: "Test message",
		UserID:         "U_ALICE",
	}

	raw, _ := json.Marshal(meta)
	parsed, err := parseDraftModalMeta(string(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.TargetChannel != "C123" {
		t.Errorf("expected C123, got %q", parsed.TargetChannel)
	}
	if parsed.UserID != "U_ALICE" {
		t.Errorf("expected U_ALICE, got %q", parsed.UserID)
	}
}

// --- Mock LLM clients for pipeline tests ---

// mockLLMClient implements llm.Client and returns a fixed response.
type mockLLMClient struct {
	response string
	err      error
}

func (m *mockLLMClient) GenerateWithTools(_ context.Context, _ llm.GenerateRequest) (*llm.GenerateResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &llm.GenerateResponse{Text: m.response}, nil
}

// mockStreamingLLMClient implements both llm.Client and llm.StreamingClient.
type mockStreamingLLMClient struct {
	response string
	chunks   []string // if set, GenerateStream emits these chunks
	err      error
}

func (m *mockStreamingLLMClient) GenerateWithTools(_ context.Context, _ llm.GenerateRequest) (*llm.GenerateResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &llm.GenerateResponse{Text: m.response}, nil
}

func (m *mockStreamingLLMClient) GenerateStream(_ context.Context, _ llm.GenerateRequest) (<-chan string, <-chan error) {
	textCh := make(chan string, len(m.chunks)+1)
	errCh := make(chan error, 1)
	go func() {
		defer close(textCh)
		defer close(errCh)
		if m.err != nil {
			errCh <- m.err
			return
		}
		if len(m.chunks) > 0 {
			for _, c := range m.chunks {
				textCh <- c
			}
		} else {
			textCh <- m.response
		}
	}()
	return textCh, errCh
}

// newTestPipeline creates a draft.Pipeline with mock LLM clients.
func newTestPipeline(researchResp, ghostwriteResp string) *draft.Pipeline {
	research := &mockLLMClient{response: researchResp}
	ghostwrite := &mockLLMClient{response: ghostwriteResp}
	return draft.NewPipeline(
		research,
		ghostwrite,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research prompt", Ghostwrite: "ghostwrite prompt"},
	)
}

// newStreamingTestPipeline creates a pipeline with a streaming ghostwrite client.
func newStreamingTestPipeline(researchResp string, chunks []string) *draft.Pipeline {
	research := &mockLLMClient{response: researchResp}
	ghostwrite := &mockStreamingLLMClient{chunks: chunks}
	return draft.NewPipeline(
		research,
		ghostwrite,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research prompt", Ghostwrite: "ghostwrite prompt"},
	)
}

// seedTestUser seeds a connected Slack account for the test server.
func seedTestUser(ctx context.Context, srv *apiServer, slackUserID, teamID, aileronUserID string) {
	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_" + aileronUserID, UserID: aileronUserID,
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
		ExternalUserID: slackUserID,
		ExternalTeamID: teamID,
	})
}

func TestHandleAssistantMessage_InformationalIntent(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()

	// Seed bot token and user.
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	// Configure pipeline with a ghostwrite client that returns text via
	// GenerateWithTools (used by ResolveIntents for informational intents).
	research := &mockLLMClient{response: "context gathered"}
	ghostwrite := &mockStreamingLLMClient{response: "Hello world!", chunks: []string{"Hello ", "world!"}}
	srv.draftPipeline = draft.NewPipeline(
		research, ghostwrite,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research prompt", Ghostwrite: "ghostwrite prompt"},
	)

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "What's on my calendar?")

	agent.mu.Lock()
	defer agent.mu.Unlock()

	// Informational intents are posted via PostMessage, not streamed.
	if len(agent.messageCalls) == 0 {
		t.Error("expected at least 1 PostMessage call for informational response")
	}

	// Should have set status then cleared.
	foundThinking := false
	foundClear := false
	for _, s := range agent.statusCalls {
		switch s {
		case "Thinking...":
			foundThinking = true
		case "":
			foundClear = true
		}
	}
	if !foundThinking {
		t.Error("expected status 'Thinking...'")
	}
	if !foundClear {
		t.Error("expected status cleared")
	}
}

func TestHandleAssistantMessage_NonStreamingFallback(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	// Non-streaming pipeline (ghostwrite is plain llm.Client, not StreamingClient).
	srv.draftPipeline = newTestPipeline("context", "Here is your draft.")

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "Draft something")

	agent.mu.Lock()
	defer agent.mu.Unlock()

	// Non-streaming: the pipeline emits a writing phase chunk then a single text chunk.
	// Since startStream returns a ts, the text chunk is appended to the stream.
	// The key assertion: no panic, status was set and cleared.
	foundClear := false
	for _, s := range agent.statusCalls {
		if s == "" {
			foundClear = true
		}
	}
	if !foundClear {
		t.Error("expected status to be cleared after non-streaming pipeline")
	}
}

func TestHandleAssistantMessage_NoBotToken(t *testing.T) {
	srv, agent := newAgentTestServer()
	// No bot token seeded — should bail early.
	srv.handleAssistantMessage(context.Background(), "T_UNKNOWN", "D_CHAN", "999.001", "U_ALICE", "hello")
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.statusCalls) != 0 {
		t.Errorf("expected no status calls when bot token missing, got %d", len(agent.statusCalls))
	}
}

func TestHandleMessageShortcut_HappyPath(t *testing.T) {
	// Set up a Slack API mock that handles views.open and views.update.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_123"},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	srv.draftPipeline = newTestPipeline("context", "Here is your draft reply.")

	payload := slackInteractionPayload{
		TriggerID: "trigger_123",
	}
	payload.User.ID = "U_ALICE"
	payload.Team = &struct {
		ID string `json:"id"`
	}{ID: "T001"}
	payload.Channel = &struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{ID: "C_GENERAL", Name: "general"}
	payload.Message = &slackActionMessage{
		Text:     "Can someone help with the deploy?",
		TS:       "111.222",
		ThreadTS: "111.000",
	}

	// Should not panic; opens modal + updates with draft.
	srv.handleMessageShortcut(ctx, payload)
}

func TestHandleMessageShortcut_NoPipeline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_123"},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	// No pipeline — should open modal then update with error.
	payload := slackInteractionPayload{TriggerID: "trig_1"}
	payload.User.ID = "U_ALICE"
	payload.Team = &struct {
		ID string `json:"id"`
	}{ID: "T001"}

	srv.handleMessageShortcut(ctx, payload)
	// No panic, modal updated with error.
}

func TestHandleMessageShortcut_NoBotToken(t *testing.T) {
	srv, _ := newAgentTestServer()
	// No bot token — should bail.
	payload := slackInteractionPayload{TriggerID: "trig_1"}
	payload.User.ID = "U_ALICE"
	payload.Team = &struct {
		ID string `json:"id"`
	}{ID: "T_MISSING"}

	srv.handleMessageShortcut(context.Background(), payload)
	// No panic.
}

func TestHandleMessageShortcut_NoUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_123"},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	// No user seeded — resolveAileronUserBySlack will fail.

	payload := slackInteractionPayload{TriggerID: "trig_1"}
	payload.User.ID = "U_MISSING"
	payload.Team = &struct {
		ID string `json:"id"`
	}{ID: "T001"}

	srv.handleMessageShortcut(ctx, payload)
	// Should update modal with error, no panic.
}

func TestHandleSlackAgentSend(t *testing.T) {
	// handleSlackAgentSend delegates to sendDraftMessage, which needs a connected
	// account + vault credential + Slack API. We test that it returns an error
	// when the user has no connected account (the error propagation path).
	srv, _ := newAgentTestServer()
	ctx := context.Background()

	err := srv.handleSlackAgentSend(ctx, "usr_nobody", "C123", "111.222", "Hello!")
	if err == nil {
		t.Fatal("expected error when no connected account exists")
	}
}

func TestBuildStreamingPipeline_NilPipeline(t *testing.T) {
	srv, _ := newAgentTestServer()
	// No pipeline configured.
	result := srv.buildStreamingPipeline("usr_a")
	if result != nil {
		t.Error("expected nil when draftPipeline is not configured")
	}
}

func TestBuildStreamingPipeline_WithPipeline(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("ctx", "draft")

	result := srv.buildStreamingPipeline("usr_a")
	if result == nil {
		t.Error("expected non-nil pipeline")
	}
}

func TestResolvePipelineVault_EscrowTier(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("ctx", "draft")
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour) // auth enabled
	// No KEK cached → tier 1 fails.

	// Set up a mock enclave client that returns plaintext.
	srv.enclaveClient = &stubEscrowEnclaveClient{}
	// Populate escrow index with an entry.
	srv.escrowIndex.Store("connected-accounts/usr_a/slack", "esc_test_1")

	result := srv.resolvePipelineVault("usr_a")
	if result == nil {
		t.Fatal("expected non-nil pipeline from escrow tier")
	}
}

func TestResolvePipelineVault_EscrowTier_OtherUserOnly(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("ctx", "draft")
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)

	srv.enclaveClient = &stubEscrowEnclaveClient{}
	// Only user B has escrow entries — user A should NOT match tier 2.
	srv.escrowIndex.Store("connected-accounts/usr_b/slack", "esc_b_1")
	srv.escrowIndex.Store("connected-accounts/usr_b/gmail", "esc_b_2")

	result := srv.resolvePipelineVault("usr_a")
	if result != nil {
		t.Fatal("expected nil — user A has no escrow entries, should not match tier 2")
	}
}

func TestResolvePipelineVault_EscrowTier_MixedUsers(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("ctx", "draft")
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)

	srv.enclaveClient = &stubEscrowEnclaveClient{}
	// Both users have escrow entries.
	srv.escrowIndex.Store("connected-accounts/usr_a/slack", "esc_a_1")
	srv.escrowIndex.Store("connected-accounts/usr_b/slack", "esc_b_1")

	// User A should match tier 2.
	resultA := srv.resolvePipelineVault("usr_a")
	if resultA == nil {
		t.Fatal("expected non-nil pipeline for user A (has escrow entries)")
	}

	// User B should also match tier 2.
	resultB := srv.resolvePipelineVault("usr_b")
	if resultB == nil {
		t.Fatal("expected non-nil pipeline for user B (has escrow entries)")
	}
}

func TestResolvePipelineVault_VaultLocked_AfterReconciliation(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("ctx", "draft")
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour) // auth enabled
	srv.enclaveClient = &stubEscrowEnclaveClient{}

	// Simulate post-reconciliation state: escrow index is empty
	// (all stale entries pruned), KEK not in cache (server restarted).
	// resolvePipelineVault should return nil → caller shows "vault locked".
	result := srv.resolvePipelineVault("usr_a")
	if result != nil {
		t.Fatal("expected nil — no KEK, no escrow entries, auth enabled")
	}
}

func TestResolvePipelineVault_NilPipeline(t *testing.T) {
	srv, _ := newAgentTestServer()
	// No pipeline configured.
	result := srv.resolvePipelineVault("usr_a")
	if result != nil {
		t.Error("expected nil when draftPipeline is not configured")
	}
}

func TestResolvePipelineVault_AuthDisabled(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("ctx", "draft")
	// kekSessionCache is nil → auth disabled → should return base pipeline.
	result := srv.resolvePipelineVault("usr_a")
	if result == nil {
		t.Error("expected non-nil pipeline when auth is disabled")
	}
}

// sourceExecuteEnclaveClient is a configurable enclave.Client for testing
// newEnclaveSourceExecutor.
type sourceExecuteEnclaveClient struct {
	stubEscrowEnclaveClient
	resp enclave.SourceExecuteResponse
	err  error
}

func (c *sourceExecuteEnclaveClient) SourceExecute(_ context.Context, _ enclave.SourceExecuteRequest) (enclave.SourceExecuteResponse, error) {
	return c.resp, c.err
}

// stubEscrowEnclaveClient is a minimal enclave.Client for testing escrow resolution.
type stubEscrowEnclaveClient struct{}

func (stubEscrowEnclaveClient) Attest(_ context.Context, _ enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	return enclave.AttestationResponse{}, nil
}
func (stubEscrowEnclaveClient) EstablishSession(_ context.Context, _ enclave.SessionRequest) (enclave.SessionResponse, error) {
	return enclave.SessionResponse{}, nil
}
func (stubEscrowEnclaveClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{}, nil
}
func (stubEscrowEnclaveClient) OAuthExchange(_ context.Context, _ enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	return enclave.OAuthExchangeResponse{}, nil
}
func (stubEscrowEnclaveClient) Execute(_ context.Context, _ enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	return enclave.ExecuteResponse{}, nil
}
func (stubEscrowEnclaveClient) EscrowStore(_ context.Context, _ enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	return enclave.EscrowStoreResponse{}, nil
}
func (stubEscrowEnclaveClient) EscrowRetrieve(_ context.Context, req enclave.EscrowRetrieveRequest) (enclave.EscrowRetrieveResponse, error) {
	return enclave.EscrowRetrieveResponse{Credential: []byte("escrow-plaintext")}, nil
}
func (stubEscrowEnclaveClient) EscrowList(_ context.Context) (enclave.EscrowListResponse, error) {
	return enclave.EscrowListResponse{}, nil
}
func (stubEscrowEnclaveClient) EscrowRevoke(_ context.Context, _ enclave.EscrowRevokeRequest) error {
	return nil
}
func (stubEscrowEnclaveClient) SourceExecute(_ context.Context, _ enclave.SourceExecuteRequest) (enclave.SourceExecuteResponse, error) {
	return enclave.SourceExecuteResponse{}, nil
}
func (stubEscrowEnclaveClient) Ready(_ context.Context) error { return nil }
func (stubEscrowEnclaveClient) Close() error                  { return nil }

func TestProcessSlashCommandDraft_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_456"},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("context", "Draft: this is your message.")

	meta := DraftModalMeta{TargetChannel: "C_GENERAL", UserID: "U_ALICE"}
	srv.processSlashCommandDraft(ctx, "T001", "U_ALICE", "draft a weekly update", "trig_1", meta)
	// No panic; modal opened and updated with draft.
}

func TestProcessSlashCommandDraft_NoBotToken(t *testing.T) {
	srv, _ := newAgentTestServer()
	meta := DraftModalMeta{TargetChannel: "C_GENERAL", UserID: "U_ALICE"}
	// No token — should bail early.
	srv.processSlashCommandDraft(context.Background(), "T_MISSING", "U_ALICE", "draft", "trig_1", meta)
}

func TestProcessSlashCommandDraft_NoUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_789"},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	// No user seeded.
	meta := DraftModalMeta{TargetChannel: "C_GENERAL", UserID: "U_NOBODY"}
	srv.processSlashCommandDraft(ctx, "T001", "U_NOBODY", "draft something", "trig_1", meta)
	// Should update modal with error.
}

func TestProcessSlashCommandDraft_NoPipeline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_789"},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	// No pipeline.
	meta := DraftModalMeta{TargetChannel: "C_GENERAL", UserID: "U_ALICE"}
	srv.processSlashCommandDraft(ctx, "T001", "U_ALICE", "draft something", "trig_1", meta)
}

func TestProcessSlashCommandDraft_PipelineError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_789"},
		})
	}))
	defer server.Close()

	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	// Pipeline with an error-returning ghostwrite client.
	errClient := &mockLLMClient{err: fmt.Errorf("LLM unavailable")}
	srv.draftPipeline = draft.NewPipeline(
		&mockLLMClient{response: "context"},
		errClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)

	meta := DraftModalMeta{TargetChannel: "C_GENERAL", UserID: "U_ALICE"}
	srv.processSlashCommandDraft(ctx, "T001", "U_ALICE", "draft something", "trig_1", meta)
	// Should update modal with error, no panic.
}

func TestProcessSlashCommandQuestion_HappyPath(t *testing.T) {
	// Set up a response_url server to capture the final response.
	var receivedBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	srv, _ := newAgentTestServer()
	ctx := context.Background()

	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("context", "The answer is 42.")

	srv.processSlashCommandQuestion(ctx, "T001", "U_ALICE", "How many hours on calls?", responseServer.URL)

	// Verify the response was sent to the response URL.
	if receivedBody == "" {
		t.Fatal("expected response sent to response_url")
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(receivedBody), &resp); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	text, _ := resp["text"].(string)
	if !strings.Contains(text, "The answer is 42.") {
		t.Errorf("expected answer text in response, got %v", text)
	}
	if !strings.Contains(text, "/aileron") {
		t.Errorf("expected /aileron prefix in response, got %v", text)
	}
	if !strings.Contains(text, "How many hours on calls?") {
		t.Errorf("expected question echoed in response, got %v", text)
	}
	if resp["response_type"] != "ephemeral" {
		t.Errorf("expected ephemeral response type, got %v", resp["response_type"])
	}
}

func TestProcessSlashCommandQuestion_NoUser(t *testing.T) {
	var receivedBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	srv, _ := newAgentTestServer()
	// No user seeded.
	srv.processSlashCommandQuestion(context.Background(), "T001", "U_UNKNOWN", "question?", responseServer.URL)

	if receivedBody == "" {
		t.Fatal("expected error response sent to response_url")
	}
	var resp map[string]any
	json.Unmarshal([]byte(receivedBody), &resp)
	if text, ok := resp["text"].(string); !ok || text != "Could not find your Aileron account." {
		t.Errorf("expected account error message, got %v", resp["text"])
	}
}

func TestProcessSlashCommandQuestion_NoPipeline(t *testing.T) {
	var receivedBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	// No pipeline.

	srv.processSlashCommandQuestion(ctx, "T001", "U_ALICE", "question?", responseServer.URL)

	var resp map[string]any
	json.Unmarshal([]byte(receivedBody), &resp)
	if text, ok := resp["text"].(string); !ok || text != "Draft generation is not configured." {
		t.Errorf("expected pipeline not configured message, got %v", resp["text"])
	}
}

func TestProcessSlashCommandQuestion_PipelineError(t *testing.T) {
	var receivedBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	errClient := &mockLLMClient{err: fmt.Errorf("LLM down")}
	srv.draftPipeline = draft.NewPipeline(
		&mockLLMClient{response: "context"},
		errClient,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "r", Ghostwrite: "g"},
	)

	srv.processSlashCommandQuestion(ctx, "T001", "U_ALICE", "question?", responseServer.URL)

	var resp map[string]any
	json.Unmarshal([]byte(receivedBody), &resp)
	if text, ok := resp["text"].(string); !ok || !strings.Contains(text, "Something went wrong. Please try again.") {
		t.Errorf("expected error message, got %v", resp["text"])
	}
}

func TestRespondViaURL_EmptyURL(t *testing.T) {
	srv, _ := newAgentTestServer()
	// Should not panic with empty URL.
	srv.respondViaURL("", "hello")
}

func TestRespondViaURL_Success(t *testing.T) {
	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	srv, _ := newAgentTestServer()
	srv.respondViaURL(server.URL, "test message")

	var resp map[string]any
	if err := json.Unmarshal([]byte(receivedBody), &resp); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	if resp["text"] != "test message" {
		t.Errorf("expected 'test message', got %v", resp["text"])
	}
	if resp["replace_original"] != true {
		t.Error("expected replace_original to be true")
	}
}

func TestHandleAssistantMessage_PipelineError(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	// Pipeline with an LLM that returns an error on the ghostwrite round.
	errMock := &mockStreamingLLMClient{err: fmt.Errorf("LLM exploded")}
	srv.draftPipeline = draft.NewPipeline(
		&mockLLMClient{response: "research ok"},
		errMock,
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "r", Ghostwrite: "g"},
	)

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "hello")

	agent.mu.Lock()
	defer agent.mu.Unlock()
	// Status should be cleared after error.
	foundClear := false
	for _, s := range agent.statusCalls {
		if s == "" {
			foundClear = true
		}
	}
	if !foundClear {
		t.Error("expected status cleared after pipeline error")
	}
}

func TestHandleAssistantMessage_VaultLocked(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("research", "draft")

	// Simulate KEK session cache present but no KEK for user → vault locked.
	srv.kekSessionCache = &auth.KEKSessionCache{}

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "hello")

	agent.mu.Lock()
	defer agent.mu.Unlock()
	foundClear := false
	for _, s := range agent.statusCalls {
		if s == "" {
			foundClear = true
		}
	}
	if !foundClear {
		t.Error("expected status cleared when vault locked")
	}
}

func TestHandleMessageShortcut_VaultLocked(t *testing.T) {
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "view": map[string]any{"id": "V_123"},
		})
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("research", "draft")
	srv.kekSessionCache = &auth.KEKSessionCache{}

	payload := buildMessageShortcutPayload(t, "U_ALICE", "T001", "C123", "general", "trig_123", "test", "111.222")

	// Should not panic; modal updated with vault locked error.
	srv.handleMessageShortcut(ctx, payload)
}

func TestHandleMessageShortcut_GenerationFails(t *testing.T) {
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "view": map[string]any{"id": "V_123"},
		})
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	// Pipeline that fails on ghostwrite.
	srv.draftPipeline = draft.NewPipeline(
		&mockLLMClient{response: "research"},
		&mockLLMClient{err: fmt.Errorf("ghostwrite failed")},
		source.NewRegistry(),
		mem.NewConnectedAccountStore(),
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "r", Ghostwrite: "g"},
	)

	payload := buildMessageShortcutPayload(t, "U_ALICE", "T001", "C123", "general", "trig_123", "test msg", "111.222")

	// Should not panic; modal updated with error.
	srv.handleMessageShortcut(ctx, payload)
}

func TestHandleMessageShortcut_LongMessageTruncated(t *testing.T) {
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "view": map[string]any{"id": "V_123"},
		})
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("research", "draft text")

	longMessage := ""
	for i := 0; i < 300; i++ {
		longMessage += "x"
	}

	payload := buildMessageShortcutPayload(t, "U_ALICE", "T001", "C123", "general", "trig_123", longMessage, "111.222")

	// Should not panic; message preview truncated to 200 chars.
	srv.handleMessageShortcut(ctx, payload)
}

func TestProcessSlackMessageEvent_DMRouting(t *testing.T) {
	srv := newSlackWebhookServer()
	srv.systemVault = vault.NewMemVault()
	srv.slackAgentClient = &mockSlackAgentClient{}
	// No pipeline — handleAssistantMessage will bail, but the routing is tested.

	srv.systemVault.Put(context.Background(), "slack-workspaces/T001TEAM/bot-token", []byte("xoxb-test"), vault.Metadata{})

	body, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"team_id":  "T001TEAM",
		"event_id": "ev_dm_msg",
		"event": map[string]any{
			"type":         "message",
			"user":         "U_ALICE",
			"text":         "Draft me something",
			"channel":      "D_DM_CHAN",
			"channel_type": "im",
			"ts":           "123.456",
		},
	})
	ts, sig := signSlackRequest(body, testSigningSecret)

	w := httptest.NewRecorder()
	srv.handleSlackEvent(w, slackWebhookRequest(body, ts, sig))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Give the async handler time to run.
	<-waitFor(50)
}

func TestBuildStreamingPipeline_VaultLocked(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("research", "draft")
	srv.kekSessionCache = &auth.KEKSessionCache{}
	// No KEK stored for user → vault locked → returns nil.
	result := srv.buildStreamingPipeline("usr_a")
	if result != nil {
		t.Error("expected nil when vault is locked")
	}
}

func TestResolvePipelineVault_ActiveTEESession_NoEscrow(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("research", "draft")
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)
	srv.teeState = newTeeState()

	// Simulate an active TEE session with no escrow entries.
	srv.teeState.userSessions["usr_a"] = time.Now().Add(1 * time.Hour)

	result := srv.resolvePipelineVault("usr_a")
	if result == nil {
		t.Error("expected non-nil pipeline when TEE session is active")
	}
}

func TestResolvePipelineVault_ExpiredTEESession_ReturnsNil(t *testing.T) {
	srv, _ := newAgentTestServer()
	srv.draftPipeline = newTestPipeline("research", "draft")
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)
	srv.teeState = newTeeState()

	// Simulate an expired TEE session.
	srv.teeState.userSessions["usr_a"] = time.Now().Add(-1 * time.Hour)

	result := srv.resolvePipelineVault("usr_a")
	if result != nil {
		t.Error("expected nil when TEE session is expired")
	}
}

func TestVaultUnlockURL_UsesConfiguredBase(t *testing.T) {
	srv := &apiServer{uiBaseURL: "https://custom.example.com"}
	url := srv.vaultUnlockURL()
	if url != "https://custom.example.com/vault" {
		t.Errorf("expected custom URL, got %q", url)
	}
}

func TestVaultUnlockURL_FallsBackToProduction(t *testing.T) {
	srv := &apiServer{}
	url := srv.vaultUnlockURL()
	if url != "https://app.withaileron.ai/vault" {
		t.Errorf("expected production fallback URL, got %q", url)
	}
}

func TestIsVaultError_MatchesCredentialUnavailable(t *testing.T) {
	wrapped := fmt.Errorf("escrow retrieve: %w", vault.ErrCredentialUnavailable)
	if !isVaultError(wrapped) {
		t.Error("expected isVaultError to return true for wrapped ErrCredentialUnavailable")
	}
}

func TestIsVaultError_FalseForOtherErrors(t *testing.T) {
	if isVaultError(fmt.Errorf("network timeout")) {
		t.Error("expected isVaultError to return false for unrelated error")
	}
}

func TestIsVaultError_MatchesAuthFailed(t *testing.T) {
	// Auth failures (e.g. expired OAuth tokens) should be treated as vault
	// errors so the handler sends the unlock message to the user.
	wrapped := fmt.Errorf("credentials expired for gmail: %w", source.ErrAuthFailed)
	if !isVaultError(wrapped) {
		t.Error("expected isVaultError to return true for wrapped ErrAuthFailed")
	}
}

func TestHandleAssistantMessage_VaultLocked_PostsUnlockMessage(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newStreamingTestPipeline("ctx", []string{"Hello"})
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour) // auth enabled, but no KEK cached
	srv.uiBaseURL = "https://app.withaileron.ai"

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "hi")

	agent.mu.Lock()
	defer agent.mu.Unlock()

	// Should have posted an unlock message.
	if len(agent.messageCalls) != 1 {
		t.Fatalf("expected 1 PostMessage call, got %d", len(agent.messageCalls))
	}
	msg := agent.messageCalls[0]
	if !strings.Contains(msg, "Unlock your vault") {
		t.Errorf("expected unlock link in message, got: %s", msg)
	}
	if !strings.Contains(msg, "app.withaileron.ai/vault") {
		t.Errorf("expected vault URL in message, got: %s", msg)
	}
}

func TestHandleAssistantMessage_VaultLocked_NoUIURL(t *testing.T) {
	srv, agent := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newStreamingTestPipeline("ctx", []string{"Hello"})
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)
	// No uiBaseURL set — should fall back to production URL.

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "hi")

	agent.mu.Lock()
	defer agent.mu.Unlock()

	if len(agent.messageCalls) != 1 {
		t.Fatalf("expected 1 PostMessage call, got %d", len(agent.messageCalls))
	}
	msg := agent.messageCalls[0]
	if !strings.Contains(msg, "Unlock your vault") {
		t.Errorf("expected unlock link in message, got: %s", msg)
	}
	if !strings.Contains(msg, "app.withaileron.ai/vault") {
		t.Errorf("expected production vault URL in message, got: %s", msg)
	}
}

func TestHandleAssistantMessage_CredentialUnavailable_PostsUnlockMessage(t *testing.T) {
	// When the pipeline hits ErrCredentialUnavailable mid-stream (vault can't
	// provide credentials), the handler should post the vault-locked message
	// directly to the user instead of letting the LLM paraphrase the error.
	srv, agent := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.uiBaseURL = "https://app.withaileron.ai"

	// Use a research LLM that fails with ErrCredentialUnavailable (simulates
	// ToolFatalError being unwrapped by the LLM client and propagated).
	accounts := mem.NewConnectedAccountStore()
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_a", UserID: "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	sourceReg := source.NewRegistry()
	sourceReg.Register(&stubSourceConnector{})

	srv.draftPipeline = draft.NewPipeline(
		&mockLLMClient{err: fmt.Errorf("vault: slack credentials unavailable: %w", vault.ErrCredentialUnavailable)},
		&mockLLMClient{response: "draft"},
		sourceReg,
		accounts,
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)
	// Ensure resolvePipelineVault returns the pipeline (not nil).
	// Disable KEK check so it falls through to "auth not enabled" tier.
	srv.kekSessionCache = nil

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "hi")

	agent.mu.Lock()
	defer agent.mu.Unlock()

	if len(agent.messageCalls) != 1 {
		t.Fatalf("expected 1 PostMessage call (vault unlock), got %d", len(agent.messageCalls))
	}
	msg := agent.messageCalls[0]
	if !strings.Contains(msg, "Unlock your vault") {
		t.Errorf("expected unlock link in message, got: %s", msg)
	}
	if !strings.Contains(msg, "vault") {
		t.Errorf("expected vault URL in message, got: %s", msg)
	}
}

func TestGenerateDraftFromInstructions_CredentialUnavailable_SendsVaultUnlockMessage(t *testing.T) {
	// When the pipeline returns ErrCredentialUnavailable during draft
	// generation from instructions, the modal should show the vault
	// unlock message instead of "Draft generation failed."
	var mu sync.Mutex
	var modalMessage string
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		modalMessage = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_MODAL_123"},
		})
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	srv.uiBaseURL = "https://app.withaileron.ai"
	ctx := context.Background()

	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	accounts := mem.NewConnectedAccountStore()
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_a", UserID: "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	sourceReg := source.NewRegistry()
	sourceReg.Register(&stubSourceConnector{})

	srv.draftPipeline = draft.NewPipeline(
		&mockLLMClient{err: fmt.Errorf("vault: escrow retrieve: %w", vault.ErrCredentialUnavailable)},
		&mockLLMClient{response: "draft"},
		sourceReg,
		accounts,
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)
	srv.kekSessionCache = nil

	meta := DraftModalMeta{
		TargetChannel:   "C123",
		OriginalMessage: "Can someone review PR #42?",
		Instructions:    "Be polite",
		UserID:          "U_ALICE",
		TeamID:          "T001",
	}

	srv.generateDraftFromInstructions(ctx, "V_MODAL_123", meta)

	mu.Lock()
	view := modalMessage
	mu.Unlock()

	if !strings.Contains(view, "Unlock your vault") {
		t.Errorf("expected vault unlock message in modal, got: %s", view)
	}
	if !strings.Contains(view, "vault") {
		t.Errorf("expected vault URL in modal, got: %s", view)
	}
	if strings.Contains(view, "Draft generation failed") {
		t.Errorf("should not show generic error for vault issues, got: %s", view)
	}
}

// failingSlackAgentClient returns errors for specific methods.
type failingSlackAgentClient struct {
	mockSlackAgentClient
	startStreamErr error
}

func (f *failingSlackAgentClient) StartStream(_ context.Context, _, _, _ string) (string, error) {
	return "", f.startStreamErr
}

func TestHandleAssistantMessage_StartStreamError(t *testing.T) {
	failClient := &failingSlackAgentClient{
		startStreamErr: fmt.Errorf("stream start failed"),
	}
	srv := &apiServer{
		log:               slog.Default(),
		connectedAccounts: mem.NewConnectedAccountStore(),
		systemVault:       vault.NewMemVault(),
		vault:             vault.NewMemVault(),
		slackAgentClient:  failClient,
		newID:             func() string { return "test-id" },
	}
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newStreamingTestPipeline("ctx", []string{"Hello"})

	srv.handleAssistantMessage(ctx, "T001", "D_CHAN", "999.001", "U_ALICE", "hi")

	// Should not panic — StartStream error is logged and returned early.
}

func TestHandleAssistantThreadStarted_SetPromptsError(t *testing.T) {
	// Use a mock that returns errors on SetSuggestedPrompts/SetTitle.
	errClient := &errorSlackAgentClient{}
	srv := &apiServer{
		log:               slog.Default(),
		connectedAccounts: mem.NewConnectedAccountStore(),
		systemVault:       vault.NewMemVault(),
		vault:             vault.NewMemVault(),
		slackAgentClient:  errClient,
		newID:             func() string { return "test-id" },
	}
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})

	// Should not panic — errors are logged.
	srv.handleAssistantThreadStarted(ctx, "T001", "D_CHAN", "999.001")
}

// errorSlackAgentClient returns errors for SetSuggestedPrompts and SetTitle.
type errorSlackAgentClient struct {
	mockSlackAgentClient
}

func (e *errorSlackAgentClient) SetSuggestedPrompts(_ context.Context, _, _, _ string, _ []comms.SlackAgentPrompt) error {
	return fmt.Errorf("prompts failed")
}

func (e *errorSlackAgentClient) SetTitle(_ context.Context, _, _, _, _ string) error {
	return fmt.Errorf("title failed")
}

func TestHandleMessageShortcut_OpenModalFails(t *testing.T) {
	// Mock server that returns an error for views.open.
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "trigger_expired"})
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})

	payload := buildMessageShortcutPayload(t, "U_ALICE", "T001", "C123", "general", "trig_expired", "test", "111.222")

	// Should not panic — open modal error is logged.
	srv.handleMessageShortcut(ctx, payload)
}

func TestHandleMessageShortcut_UpdateModalError(t *testing.T) {
	// First call (views.open) succeeds, second call (views.update) fails.
	callCount := 0
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "view": map[string]any{"id": "V_123"}})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "view_not_found"})
		}
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("research", "draft output")

	payload := buildMessageShortcutPayload(t, "U_ALICE", "T001", "C123", "general", "trig_123", "test", "111.222")

	// Should not panic — update modal error logged.
	srv.handleMessageShortcut(ctx, payload)
}

func TestProcessSlashCommandDraft_VaultLocked(t *testing.T) {
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "view": map[string]any{"id": "V_123"}})
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("research", "draft")
	srv.kekSessionCache = &auth.KEKSessionCache{}

	meta := DraftModalMeta{TargetChannel: "C123", UserID: "U_ALICE"}
	srv.processSlashCommandDraft(ctx, "T001", "U_ALICE", "Draft something", "trig_123", meta)
	// Should not panic — vault locked error shown in modal.
}

func TestProcessSlashCommandQuestion_VaultLocked(t *testing.T) {
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer respServer.Close()

	srv, _ := newAgentTestServer()
	ctx := context.Background()
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("research", "answer")
	srv.kekSessionCache = &auth.KEKSessionCache{}

	srv.processSlashCommandQuestion(ctx, "T001", "U_ALICE", "How many calls?", respServer.URL)
	// Should not panic — vault locked error sent via response_url.
}

// buildMessageShortcutPayload constructs a slackInteractionPayload via JSON
// round-trip to avoid anonymous struct type mismatches with JSON tags.
func buildMessageShortcutPayload(t *testing.T, userID, teamID, channelID, channelName, triggerID, messageText, messageTS string) slackInteractionPayload {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"type":       "message_action",
		"trigger_id": triggerID,
		"user":       map[string]any{"id": userID},
		"team":       map[string]any{"id": teamID},
		"channel":    map[string]any{"id": channelID, "name": channelName},
		"message":    map[string]any{"text": messageText, "ts": messageTS},
	})
	var payload slackInteractionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("failed to build payload: %v", err)
	}
	return payload
}

// waitFor returns a channel that closes after d milliseconds.
func waitFor(ms int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		<-time.After(time.Duration(ms) * time.Millisecond)
		close(ch)
	}()
	return ch
}

func TestNewEnclaveSourceExecutor_HappyPath(t *testing.T) {
	resultJSON, _ := json.Marshal(map[string]any{"messages": []string{"hello"}})
	client := &sourceExecuteEnclaveClient{
		resp: enclave.SourceExecuteResponse{Result: resultJSON},
	}
	srv := &apiServer{
		enclaveClient: client,
	}
	srv.escrowIndex.Store("connected-accounts/usr_1/gmail", "esc_gmail_123")

	executor := srv.newEnclaveSourceExecutor()
	result, err := executor(context.Background(), "gmail_search", map[string]any{"query": "test"}, "connected-accounts/usr_1/gmail")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	msgs, ok := result["messages"]
	if !ok {
		t.Fatal("expected 'messages' key in result")
	}
	arr, ok := msgs.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("expected 1 message, got %v", msgs)
	}
}

func TestNewEnclaveSourceExecutor_NoEscrowEntry(t *testing.T) {
	srv := &apiServer{
		enclaveClient: &sourceExecuteEnclaveClient{},
	}
	// escrowIndex is empty — no entry for this path.

	executor := srv.newEnclaveSourceExecutor()
	_, err := executor(context.Background(), "gmail_search", nil, "connected-accounts/usr_1/gmail")
	if err == nil {
		t.Fatal("expected error for missing escrow entry")
	}
	if !errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Errorf("expected ErrCredentialUnavailable, got: %v", err)
	}
}

func TestNewEnclaveSourceExecutor_EnclaveError(t *testing.T) {
	client := &sourceExecuteEnclaveClient{
		err: fmt.Errorf("enclave unreachable"),
	}
	srv := &apiServer{
		enclaveClient: client,
	}
	srv.escrowIndex.Store("connected-accounts/usr_1/gmail", "esc_gmail_123")

	executor := srv.newEnclaveSourceExecutor()
	_, err := executor(context.Background(), "gmail_search", nil, "connected-accounts/usr_1/gmail")
	if err == nil {
		t.Fatal("expected error when enclave fails")
	}
}

func TestNewEnclaveSourceExecutor_AuthError(t *testing.T) {
	client := &sourceExecuteEnclaveClient{
		resp: enclave.SourceExecuteResponse{
			Error: "googleapi: Error 401: Invalid Credentials: source: authentication failed",
		},
	}
	srv := &apiServer{
		enclaveClient: client,
	}
	srv.escrowIndex.Store("connected-accounts/usr_1/gmail", "esc_gmail_123")

	executor := srv.newEnclaveSourceExecutor()
	_, err := executor(context.Background(), "gmail_search", nil, "connected-accounts/usr_1/gmail")
	if err == nil {
		t.Fatal("expected error for auth failure")
	}
	if !errors.Is(err, source.ErrAuthFailed) {
		t.Errorf("expected ErrAuthFailed, got: %v", err)
	}
}

func TestNewEnclaveSourceExecutor_ToolError(t *testing.T) {
	client := &sourceExecuteEnclaveClient{
		resp: enclave.SourceExecuteResponse{
			Error: "some tool error",
		},
	}
	srv := &apiServer{
		enclaveClient: client,
	}
	srv.escrowIndex.Store("connected-accounts/usr_1/slack", "esc_slack_123")

	executor := srv.newEnclaveSourceExecutor()
	_, err := executor(context.Background(), "slack_search", nil, "connected-accounts/usr_1/slack")
	if err == nil {
		t.Fatal("expected error for tool failure")
	}
	// Should NOT be ErrAuthFailed for non-auth errors.
	if errors.Is(err, source.ErrAuthFailed) {
		t.Error("non-auth tool error should not wrap ErrAuthFailed")
	}
}

func TestNewEnclaveSourceExecutor_NilResult(t *testing.T) {
	client := &sourceExecuteEnclaveClient{
		resp: enclave.SourceExecuteResponse{}, // no result, no error
	}
	srv := &apiServer{
		enclaveClient: client,
	}
	srv.escrowIndex.Store("connected-accounts/usr_1/slack", "esc_slack_123")

	executor := srv.newEnclaveSourceExecutor()
	result, err := executor(context.Background(), "slack_search", nil, "connected-accounts/usr_1/slack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// --- presentWriteAction tests ---

func TestPresentWriteAction_EmailIntent(t *testing.T) {
	srv, agent := newAgentTestServer()
	intent := draft.ResolvedIntent{
		Type:    draft.IntentTypeEmailSend,
		Summary: "Send email to Alice",
		Action: &model.ActionIntent{
			Type: "email.send", Summary: "Send email to Alice",
			Domain: model.DomainAction{Email: &model.EmailAction{
				To: []model.Recipient{{Name: "Alice", Email: "alice@example.com"}},
				Subject: "Weekly Report", BodyText: "Here is the weekly summary.",
			}},
		},
	}
	srv.presentWriteAction(context.Background(), "xoxb-test", "D_CHAN", "999.001", "usr_a", intent)
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.messageCalls) < 1 {
		t.Fatal("expected PostMessage call for preview")
	}
	preview := agent.messageCalls[0]
	for _, want := range []string{"Send Email", "alice@example.com", "Weekly Report", "Here is the weekly summary."} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q: %s", want, preview)
		}
	}
}

func TestPresentWriteAction_CalendarIntent(t *testing.T) {
	srv, agent := newAgentTestServer()
	start := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 25, 10, 30, 0, 0, time.UTC)
	intent := draft.ResolvedIntent{
		Type: draft.IntentTypeCalendarEventCreate, Summary: "Create meeting",
		Action: &model.ActionIntent{Type: "calendar.event.create", Summary: "Create meeting",
			Domain: model.DomainAction{Calendar: &model.CalendarAction{
				Title: "Team Standup", StartTime: &start, EndTime: &end,
				Location: "Room A", Attendees: []model.CalendarAttendee{{Name: "Bob", Email: "bob@example.com"}},
			}},
		},
	}
	srv.presentWriteAction(context.Background(), "xoxb-test", "D_CHAN", "999.001", "usr_a", intent)
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.messageCalls) < 1 {
		t.Fatal("expected PostMessage call")
	}
	preview := agent.messageCalls[0]
	for _, want := range []string{"Create Calendar Event", "Team Standup", "Room A", "Bob"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q: %s", want, preview)
		}
	}
}

func TestPresentWriteAction_GitIssueIntent(t *testing.T) {
	srv, agent := newAgentTestServer()
	intent := draft.ResolvedIntent{
		Type: draft.IntentTypeGitIssueCreate, Summary: "Create issue",
		Action: &model.ActionIntent{Type: "git.issue.create", Summary: "Create issue",
			Domain: model.DomainAction{Git: &model.GitAction{
				Repository: "acme/app", IssueTitle: "Login bug",
				IssueBody: "Steps to reproduce.", IssueLabels: []string{"bug", "high-priority"},
			}},
		},
	}
	srv.presentWriteAction(context.Background(), "xoxb-test", "D_CHAN", "999.001", "usr_a", intent)
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.messageCalls) < 1 {
		t.Fatal("expected PostMessage call")
	}
	preview := agent.messageCalls[0]
	for _, want := range []string{"Create GitHub Issue", "acme/app", "Login bug", "bug, high-priority"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q: %s", want, preview)
		}
	}
}
