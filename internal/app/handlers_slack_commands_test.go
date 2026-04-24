package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/draft"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/source"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
)

func newCommandTestServer() *apiServer {
	return &apiServer{
		log:                slog.Default(),
		connectedAccounts:  mem.NewConnectedAccountStore(),
		systemVault:        vault.NewMemVault(),
		vault:              vault.NewMemVault(),
		slackSigningSecret: testSigningSecret,
		newID:              func() string { return "test-id" },
	}
}

func signedCommandRequest(params url.Values) *http.Request {
	body := params.Encode()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", ts, body)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/commands",
		strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", sig)
	return r
}

func TestSlackCommand_MethodNotAllowed(t *testing.T) {
	srv := newCommandTestServer()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/webhooks/slack/commands", nil)
	srv.handleSlackCommand(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestSlackCommand_InvalidSignature(t *testing.T) {
	srv := newCommandTestServer()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/webhooks/slack/commands",
		strings.NewReader("text=hello"))
	r.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set("X-Slack-Signature", "v0=invalid")
	srv.handleSlackCommand(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestSlackCommand_Question_Returns200(t *testing.T) {
	srv := newCommandTestServer()
	params := url.Values{
		"command":      {"/aileron"},
		"text":         {"How many hours on calls today?"},
		"team_id":      {"T001"},
		"channel_id":   {"C123"},
		"user_id":      {"U_ALICE"},
		"response_url": {"https://hooks.slack.com/commands/test"},
	}

	w := httptest.NewRecorder()
	srv.handleSlackCommand(w, signedCommandRequest(params))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["response_type"] != "ephemeral" {
		t.Errorf("expected ephemeral response, got %v", resp["response_type"])
	}
	text, _ := resp["text"].(string)
	if !strings.Contains(text, "How many hours on calls today?") {
		t.Errorf("expected ephemeral to echo user's question, got %q", text)
	}
	if !strings.Contains(text, "/aileron") {
		t.Errorf("expected ephemeral to include /aileron prefix, got %q", text)
	}
}

func TestSlackCommand_Draft_Returns200(t *testing.T) {
	srv := newCommandTestServer()
	params := url.Values{
		"command":    {"/aileron"},
		"text":       {"Draft me a weekly status update"},
		"team_id":    {"T001"},
		"channel_id": {"C123"},
		"user_id":    {"U_ALICE"},
		"trigger_id": {"trig_123"},
	}

	w := httptest.NewRecorder()
	srv.handleSlackCommand(w, signedCommandRequest(params))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// The draft flow opens a modal async — response is empty 200.
	time.Sleep(50 * time.Millisecond)
}

func TestSlackCommand_DraftNoTriggerID_FallsBackToEphemeral(t *testing.T) {
	srv := newCommandTestServer()
	params := url.Values{
		"command":      {"/aileron"},
		"text":         {"Draft me a status update"},
		"team_id":      {"T001"},
		"channel_id":   {"C123"},
		"user_id":      {"U_ALICE"},
		"response_url": {"https://hooks.slack.com/commands/test"},
		// no trigger_id
	}

	w := httptest.NewRecorder()
	srv.handleSlackCommand(w, signedCommandRequest(params))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["response_type"] != "ephemeral" {
		t.Errorf("expected ephemeral response when no trigger_id, got %v", resp["response_type"])
	}
	text, _ := resp["text"].(string)
	if !strings.Contains(text, "Draft me a status update") {
		t.Errorf("expected ephemeral to echo user's request, got %q", text)
	}
}

func TestSlashCommandQuestion_CredentialUnavailable_SendsVaultUnlockMessage(t *testing.T) {
	// When GenerateDraft returns ErrCredentialUnavailable (stale escrow or
	// locked vault), the handler should respond with the vault unlock message
	// instead of the generic "Something went wrong" error.
	var mu sync.Mutex
	var capturedBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		mu.Lock()
		capturedBody = string(b[:n])
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	srv := newCommandTestServer()
	srv.uiBaseURL = "https://app.withaileron.ai"
	ctx := context.Background()

	// Seed user mapping.
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})

	// Configure pipeline with a research LLM that fails with ErrCredentialUnavailable.
	accounts := mem.NewConnectedAccountStore()
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_a", UserID: "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	srv.draftPipeline = newVaultErrorPipeline(accounts)
	// Disable KEK check so resolvePipelineVault falls through to tier 3.
	srv.kekSessionCache = nil

	srv.processSlashCommand(ctx, "T001", "C_CHAN", "U_ALICE", "what's in my email?", "", responseServer.URL)

	mu.Lock()
	body := capturedBody
	mu.Unlock()

	if !strings.Contains(body, "Unlock your vault") {
		t.Errorf("expected vault unlock message, got: %s", body)
	}
	if !strings.Contains(body, "vault") {
		t.Errorf("expected vault URL in response, got: %s", body)
	}
	// Should NOT contain the generic error message.
	if strings.Contains(body, "Something went wrong") {
		t.Errorf("should not contain generic error when vault is locked, got: %s", body)
	}
}

// newVaultErrorPipeline returns a pipeline whose research LLM fails with
// ErrCredentialUnavailable, simulating a stale escrow or locked vault.
func newVaultErrorPipeline(accounts *mem.ConnectedAccountStore) *draft.Pipeline {
	sourceReg := source.NewRegistry()
	sourceReg.Register(&stubSourceConnector{})
	return draft.NewPipeline(
		&mockLLMClient{err: fmt.Errorf("vault: escrow retrieve: %w", vault.ErrCredentialUnavailable)},
		&mockLLMClient{response: "draft"},
		sourceReg,
		accounts,
		mem.NewUserInstructionStore(),
		vault.NewMemVault(),
		slog.Default(),
		draft.Prompts{Research: "research", Ghostwrite: "ghostwrite"},
	)
}

func TestSlashCommandDraft_CredentialUnavailable_SendsVaultUnlockMessage(t *testing.T) {
	// When GenerateDraft returns ErrCredentialUnavailable during draft
	// generation, the modal should show the vault unlock message instead
	// of "Draft generation failed."
	var mu sync.Mutex

	srv := newCommandTestServer()
	srv.uiBaseURL = "https://app.withaileron.ai"
	ctx := context.Background()

	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})

	accounts := mem.NewConnectedAccountStore()
	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_a", UserID: "usr_a",
		Provider: model.ConnectedAccountProviderSlack,
		Status:   model.ConnectedAccountStatusActive,
	})
	srv.draftPipeline = newVaultErrorPipeline(accounts)
	srv.kekSessionCache = nil

	// New flow: vault locked errors go via response_url, not modal.
	var responseBody string
	responseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		responseBody = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer responseServer.Close()

	srv.processSlashCommand(ctx, "T001", "C_CHAN", "U_ALICE", "draft a reply", "trig_123", responseServer.URL)

	mu.Lock()
	body := responseBody
	mu.Unlock()

	if !strings.Contains(body, "Unlock your vault") {
		t.Errorf("expected vault unlock message in response, got: %s", body)
	}
	if !strings.Contains(body, "vault") {
		t.Errorf("expected vault URL in response, got: %s", body)
	}
	if strings.Contains(body, "Draft generation failed") {
		t.Errorf("should not show generic error for vault issues, got: %s", body)
	}
}

func TestSlackCommand_InvalidFormData(t *testing.T) {
	srv := newCommandTestServer()

	// Send raw bytes that aren't valid form data — but url.ParseQuery is lenient,
	// so this actually won't fail. Instead test with bad signature.
	body := []byte("%%%invalid")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	baseString := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(baseString))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/v1/webhooks/slack/commands",
		strings.NewReader(string(body)))
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", sig)

	w := httptest.NewRecorder()
	srv.handleSlackCommand(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid form data, got %d", w.Code)
	}
}

// --- formatIntentPreview tests ---

func TestFormatIntentPreview_Email(t *testing.T) {
	intent := draft.ResolvedIntent{
		Type:    draft.IntentTypeEmailSend,
		Summary: "Send email",
		Action: &model.ActionIntent{
			Type: "email.send",
			Domain: model.DomainAction{Email: &model.EmailAction{
				To:       []model.Recipient{{Name: "Alice", Email: "alice@example.com"}},
				Subject:  "Weekly Report",
				BodyText: "Here is the report.",
			}},
		},
	}
	preview := formatIntentPreview(intent)
	for _, want := range []string{"alice@example.com", "Weekly Report", "Here is the report."} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q: %s", want, preview)
		}
	}
}

func TestFormatIntentPreview_Calendar(t *testing.T) {
	start := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 25, 10, 30, 0, 0, time.UTC)
	intent := draft.ResolvedIntent{
		Type: draft.IntentTypeCalendarEventCreate,
		Action: &model.ActionIntent{
			Type: "calendar.event.create",
			Domain: model.DomainAction{Calendar: &model.CalendarAction{
				Title: "Standup", StartTime: &start, EndTime: &end,
				Location: "Room A", Description: "Daily sync",
			}},
		},
	}
	preview := formatIntentPreview(intent)
	for _, want := range []string{"Standup", "Room A", "Daily sync"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q: %s", want, preview)
		}
	}
}

func TestFormatIntentPreview_GitIssue(t *testing.T) {
	intent := draft.ResolvedIntent{
		Type: draft.IntentTypeGitIssueCreate,
		Action: &model.ActionIntent{
			Type: "git.issue.create",
			Domain: model.DomainAction{Git: &model.GitAction{
				Repository: "acme/app", IssueTitle: "Login bug", IssueBody: "Steps to reproduce.",
			}},
		},
	}
	preview := formatIntentPreview(intent)
	for _, want := range []string{"acme/app", "Login bug", "Steps to reproduce."} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q: %s", want, preview)
		}
	}
}

func TestFormatIntentPreview_NilAction(t *testing.T) {
	intent := draft.ResolvedIntent{Type: draft.IntentTypeEmailSend, Summary: "fallback text"}
	if got := formatIntentPreview(intent); got != "fallback text" {
		t.Errorf("expected summary fallback, got %q", got)
	}
}

// --- intentTypeLabel tests ---

func TestIntentTypeLabel(t *testing.T) {
	tests := []struct {
		t    draft.IntentType
		want string
	}{
		{draft.IntentTypeEmailSend, "Send Email"},
		{draft.IntentTypeEmailDraft, "Save Email Draft"},
		{draft.IntentTypeCalendarEventCreate, "Create Calendar Event"},
		{draft.IntentTypeGitIssueCreate, "Create GitHub Issue"},
		{draft.IntentTypeGitIssueComment, "Comment on GitHub Issue"},
		{draft.IntentTypeSlackSend, "Send Slack Message"},
		{draft.IntentType("custom.action"), "custom.action"},
	}
	for _, tt := range tests {
		if got := intentTypeLabel(tt.t); got != tt.want {
			t.Errorf("intentTypeLabel(%q) = %q, want %q", tt.t, got, tt.want)
		}
	}
}

// --- respondViaURL tests ---

func TestCommandRespondViaURL_EmptyURL(t *testing.T) {
	srv := newCommandTestServer()
	// Should not panic with empty URL.
	srv.respondViaURL("", "test")
}

func TestCommandRespondViaURL_SendsResponse(t *testing.T) {
	var mu sync.Mutex
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	srv := newCommandTestServer()
	srv.respondViaURL(server.URL, "hello world")

	mu.Lock()
	got := body
	mu.Unlock()

	if !strings.Contains(got, "hello world") {
		t.Errorf("expected 'hello world' in body, got %s", got)
	}
	if !strings.Contains(got, "ephemeral") {
		t.Errorf("expected ephemeral response type, got %s", got)
	}
}

// --- processSlashCommand write action path ---

func TestProcessSlashCommand_WriteAction_NoTriggerID_FallsBackToResponseURL(t *testing.T) {
	var mu sync.Mutex
	var body string
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer respServer.Close()

	srv := newCommandTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	// Pipeline that returns an email.send intent.
	research := &mockLLMClient{response: `{"type":"email.send","to":[{"email":"bob@example.com"}],"subject":"Hi","body_text":"Hello"}`}
	srv.draftPipeline = draft.NewPipeline(research, research, source.NewRegistry(),
		mem.NewConnectedAccountStore(), mem.NewUserInstructionStore(),
		vault.NewMemVault(), slog.Default(),
		draft.Prompts{Research: "r", IntentResolution: "i", Ghostwrite: "g"})

	// No trigger_id → should fall back to response_url.
	srv.processSlashCommand(ctx, "T001", "C_CHAN", "U_ALICE", "Send email to Bob", "", respServer.URL)

	mu.Lock()
	got := body
	mu.Unlock()
	// Should contain the email preview text.
	if got == "" {
		t.Error("expected response sent to response_url for write action without trigger_id")
	}
}

func TestProcessSlashCommand_EnclaveNotReady(t *testing.T) {
	var mu sync.Mutex
	var body string
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer respServer.Close()

	srv := newCommandTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")
	srv.draftPipeline = newTestPipeline("ctx", "answer")
	srv.enclaveClient = &failingEnclaveClient{}
	// Set kekSessionCache so Tier 4 (no-auth) doesn't bypass enclave check.
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)

	srv.processSlashCommand(ctx, "T001", "C_CHAN", "U_ALICE", "question?", "", respServer.URL)

	mu.Lock()
	got := body
	mu.Unlock()
	if !strings.Contains(got, "starting up") {
		t.Errorf("expected starting up message, got: %s", got)
	}
}

func TestFormatIntentPreview_GitPR(t *testing.T) {
	// Git action without issue fields → falls back to summary.
	intent := draft.ResolvedIntent{
		Type:    draft.IntentType("git.pull_request.create"),
		Summary: "Create PR for feature",
		Action: &model.ActionIntent{
			Type: "git.pull_request.create",
			Domain: model.DomainAction{Git: &model.GitAction{
				Repository: "acme/app", Branch: "feat/x", BaseBranch: "main",
			}},
		},
	}
	got := formatIntentPreview(intent)
	if got != "Create PR for feature" {
		t.Errorf("expected summary fallback for PR, got %q", got)
	}
}

func TestFormatIntentPreview_DefaultAction(t *testing.T) {
	intent := draft.ResolvedIntent{
		Type:    draft.IntentType("custom.action"),
		Summary: "Do something custom",
		Action:  &model.ActionIntent{Type: "custom.action", Summary: "Do something custom"},
	}
	got := formatIntentPreview(intent)
	if got != "Do something custom" {
		t.Errorf("expected summary fallback, got %q", got)
	}
}

// --- openIntentModal test ---

func TestOpenIntentModal_CallsSlackAPI(t *testing.T) {
	var mu sync.Mutex
	var receivedBody string
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V_MODAL"},
		})
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

	srv := newCommandTestServer()
	intent := draft.ResolvedIntent{
		Type:    draft.IntentTypeEmailSend,
		Summary: "Send email",
		Action: &model.ActionIntent{
			Type: "email.send",
			Domain: model.DomainAction{Email: &model.EmailAction{
				To: []model.Recipient{{Email: "alice@example.com"}},
				Subject: "Test", BodyText: "Hello",
			}},
		},
	}

	srv.openIntentModal(context.Background(), "xoxb-test", "trig_123", "usr_a", "C_CHAN", intent)

	mu.Lock()
	body := receivedBody
	mu.Unlock()

	if body == "" {
		t.Fatal("expected Slack API call for modal")
	}
	if !strings.Contains(body, intentModalCallbackID) {
		t.Errorf("expected callback_id in modal request, got: %s", body)
	}
	if !strings.Contains(body, "Review") {
		t.Errorf("expected 'Review' title in modal, got: %s", body)
	}
}

func TestProcessSlashCommand_EmptyIntents(t *testing.T) {
	var mu sync.Mutex
	var body string
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer respServer.Close()

	srv := newCommandTestServer()
	ctx := context.Background()
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-test"), vault.Metadata{})
	seedTestUser(ctx, srv, "U_ALICE", "T001", "usr_a")

	// Pipeline returns empty intents.
	emptyClient := &mockLLMClient{response: ""}
	srv.draftPipeline = draft.NewPipeline(emptyClient, emptyClient, source.NewRegistry(),
		mem.NewConnectedAccountStore(), mem.NewUserInstructionStore(),
		vault.NewMemVault(), slog.Default(),
		draft.Prompts{Research: "r", Ghostwrite: "g"})

	srv.processSlashCommand(ctx, "T001", "C_CHAN", "U_ALICE", "something", "", respServer.URL)

	mu.Lock()
	got := body
	mu.Unlock()
	// Should get a response even with empty ghostwrite (informational with empty text).
	if got == "" {
		t.Error("expected response to response_url")
	}
}

func TestCommandRespondViaURL_HTTPError(t *testing.T) {
	srv := newCommandTestServer()
	// URL that immediately closes connection.
	srv.respondViaURL("http://127.0.0.1:1/bad", "test message")
	// Should not panic — just logs the error.
}
