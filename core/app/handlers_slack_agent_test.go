package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
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
