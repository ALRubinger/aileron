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

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/draft"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/source"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
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

	srv.processSlashCommandQuestion(ctx, "T001", "U_ALICE", "what's in my email?", responseServer.URL)

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
	var modalMessage string
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		modalMessage = string(body)
		mu.Unlock()
		slackViewOK(w)
	}))
	defer slackServer.Close()

	comms.SetAgentAPIURL(slackServer.URL + "/")
	defer comms.SetAgentAPIURL("")

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

	meta := DraftModalMeta{
		TargetChannel:   "C123",
		OriginalMessage: "Help with PR",
		UserID:          "U_ALICE",
		TeamID:          "T001",
	}

	srv.processSlashCommandDraft(ctx, "T001", "U_ALICE", "draft a reply", "trig_123", meta)

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
