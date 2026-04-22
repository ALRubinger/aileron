package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/vault"
)

func newInstallTestServer(t *testing.T) *apiServer {
	t.Helper()
	return &apiServer{
		log:               slog.New(slog.NewJSONHandler(io.Discard, nil)),
		systemVault:       vault.NewMemVault(),
		slackClientID:     "test-client-id",
		slackClientSecret: "test-client-secret",
	}
}

func TestHandleSlackInstall_Success(t *testing.T) {
	srv := newInstallTestServer(t)
	srv.slackBotExchanger = func(clientID, clientSecret, code, redirectURI string) (*slackBotInstallResponse, error) {
		if clientID != "test-client-id" {
			t.Errorf("clientID = %q, want test-client-id", clientID)
		}
		if clientSecret != "test-client-secret" {
			t.Errorf("clientSecret = %q, want test-client-secret", clientSecret)
		}
		if code != "test-code-123" {
			t.Errorf("code = %q, want test-code-123", code)
		}
		return &slackBotInstallResponse{
			OK:          true,
			AccessToken: "xoxb-test-bot-token",
			TokenType:   "bot",
			BotUserID:   "U_BOT",
			Team:        struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{"T001", "test-workspace"},
		}, nil
	}

	req := httptest.NewRequest("GET", "/v1/slack/install/callback?code=test-code-123", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify HTML response.
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "installed in your Slack workspace") {
		t.Errorf("expected success HTML, got: %s", body)
	}

	// Verify bot token stored in system vault.
	secret, err := srv.systemVault.Get(context.Background(), "slack-workspaces/T001/bot-token")
	if err != nil {
		t.Fatalf("expected bot token in system vault: %v", err)
	}
	if string(secret.Value) != "xoxb-test-bot-token" {
		t.Errorf("stored token = %q, want xoxb-test-bot-token", secret.Value)
	}
	if secret.Metadata.Type != "slack_bot_token" {
		t.Errorf("metadata type = %q, want slack_bot_token", secret.Metadata.Type)
	}
	if secret.Metadata.Labels["team_id"] != "T001" {
		t.Errorf("metadata team_id = %q, want T001", secret.Metadata.Labels["team_id"])
	}
	if secret.Metadata.Labels["team_name"] != "test-workspace" {
		t.Errorf("metadata team_name = %q, want test-workspace", secret.Metadata.Labels["team_name"])
	}
	if secret.Metadata.Labels["bot_user_id"] != "U_BOT" {
		t.Errorf("metadata bot_user_id = %q, want U_BOT", secret.Metadata.Labels["bot_user_id"])
	}
}

func TestHandleSlackInstall_MissingCode(t *testing.T) {
	srv := newInstallTestServer(t)

	req := httptest.NewRequest("GET", "/v1/slack/install/callback", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleSlackInstall_NoSystemVault(t *testing.T) {
	srv := newInstallTestServer(t)
	srv.systemVault = nil

	req := httptest.NewRequest("GET", "/v1/slack/install/callback?code=test", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestHandleSlackInstall_ExchangeFails(t *testing.T) {
	srv := newInstallTestServer(t)
	srv.slackBotExchanger = func(_, _, _, _ string) (*slackBotInstallResponse, error) {
		return nil, io.ErrUnexpectedEOF
	}

	req := httptest.NewRequest("GET", "/v1/slack/install/callback?code=bad-code", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}

func TestHandleSlackInstall_NoBotToken(t *testing.T) {
	srv := newInstallTestServer(t)
	srv.slackBotExchanger = func(_, _, _, _ string) (*slackBotInstallResponse, error) {
		return &slackBotInstallResponse{
			OK:   true,
			Team: struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{"T001", "ws"},
		}, nil
	}

	req := httptest.NewRequest("GET", "/v1/slack/install/callback?code=test", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}

func TestHandleSlackInstall_NoTeamID(t *testing.T) {
	srv := newInstallTestServer(t)
	srv.slackBotExchanger = func(_, _, _, _ string) (*slackBotInstallResponse, error) {
		return &slackBotInstallResponse{
			OK:          true,
			AccessToken: "xoxb-token",
		}, nil
	}

	req := httptest.NewRequest("GET", "/v1/slack/install/callback?code=test", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}

func TestHandleSlackInstall_OverwritesExistingToken(t *testing.T) {
	srv := newInstallTestServer(t)
	ctx := context.Background()

	// Pre-existing bot token.
	srv.systemVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-old"), vault.Metadata{Type: "slack_bot_token"})

	srv.slackBotExchanger = func(_, _, _, _ string) (*slackBotInstallResponse, error) {
		return &slackBotInstallResponse{
			OK:          true,
			AccessToken: "xoxb-new",
			BotUserID:   "U_BOT",
			Team:        struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{"T001", "ws"},
		}, nil
	}

	req := httptest.NewRequest("GET", "/v1/slack/install/callback?code=reinstall", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	secret, err := srv.systemVault.Get(ctx, "slack-workspaces/T001/bot-token")
	if err != nil {
		t.Fatalf("get after overwrite: %v", err)
	}
	if string(secret.Value) != "xoxb-new" {
		t.Errorf("token = %q, want xoxb-new", secret.Value)
	}
}

func TestHandleSlackInstall_NoAuthRequired(t *testing.T) {
	srv := newInstallTestServer(t)
	srv.slackBotExchanger = func(_, _, _, _ string) (*slackBotInstallResponse, error) {
		return &slackBotInstallResponse{
			OK:          true,
			AccessToken: "xoxb-token",
			BotUserID:   "U_BOT",
			Team:        struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{"T002", "ws"},
		}, nil
	}

	// No Authorization header — this endpoint must work without auth.
	req := httptest.NewRequest("GET", "/v1/slack/install/callback?code=test", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 without auth, got %d", w.Code)
	}
}

func TestHandleSlackInstall_VaultPutFails(t *testing.T) {
	srv := newInstallTestServer(t)
	// Use a failing vault — Get works but Put always fails.
	srv.systemVault = &failPutVault{}
	srv.slackBotExchanger = func(_, _, _, _ string) (*slackBotInstallResponse, error) {
		return &slackBotInstallResponse{
			OK:          true,
			AccessToken: "xoxb-token",
			BotUserID:   "U_BOT",
			Team: struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{"T001", "ws"},
		}, nil
	}

	req := httptest.NewRequest("GET", "/v1/slack/install/callback?code=test", nil)
	w := httptest.NewRecorder()
	srv.handleSlackInstall(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// failPutVault is a vault that always fails on Put.
type failPutVault struct{}

func (v *failPutVault) Get(_ context.Context, _ string) (vault.Secret, error) {
	return vault.Secret{}, nil
}
func (v *failPutVault) Put(_ context.Context, _ string, _ []byte, _ vault.Metadata) error {
	return io.ErrUnexpectedEOF
}
func (v *failPutVault) Delete(_ context.Context, _ string) error {
	return nil
}

func TestDefaultSlackBotTokenExchange_Success(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		r.ParseForm()
		if r.FormValue("client_id") != "cid" {
			t.Errorf("client_id = %q", r.FormValue("client_id"))
		}
		if r.FormValue("code") != "test-code" {
			t.Errorf("code = %q", r.FormValue("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"access_token":"xoxb-bot","token_type":"bot","bot_user_id":"U_BOT","team":{"id":"T001","name":"ws"}}`))
	}))
	defer mockServer.Close()

	srv := newInstallTestServer(t)
	srv.slackTokenURL = mockServer.URL

	resp, err := srv.defaultSlackBotTokenExchange("cid", "csecret", "test-code", "http://localhost/callback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.AccessToken != "xoxb-bot" {
		t.Errorf("access_token = %q, want xoxb-bot", resp.AccessToken)
	}
	if resp.Team.ID != "T001" {
		t.Errorf("team_id = %q, want T001", resp.Team.ID)
	}
}

func TestDefaultSlackBotTokenExchange_SlackError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":false,"error":"invalid_code"}`))
	}))
	defer mockServer.Close()

	srv := newInstallTestServer(t)
	srv.slackTokenURL = mockServer.URL

	_, err := srv.defaultSlackBotTokenExchange("cid", "csecret", "bad", "http://localhost/callback")
	if err == nil {
		t.Fatal("expected error for Slack error response")
	}
	if !strings.Contains(err.Error(), "invalid_code") {
		t.Errorf("error = %q, want to contain invalid_code", err.Error())
	}
}

func TestDefaultSlackBotTokenExchange_InvalidJSON(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer mockServer.Close()

	srv := newInstallTestServer(t)
	srv.slackTokenURL = mockServer.URL

	_, err := srv.defaultSlackBotTokenExchange("cid", "csecret", "code", "http://localhost/callback")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestGetSlackTokenURL_Default(t *testing.T) {
	srv := newInstallTestServer(t)
	if got := srv.getSlackTokenURL(); got != slackTokenURL {
		t.Errorf("getSlackTokenURL = %q, want %q", got, slackTokenURL)
	}
}

func TestGetSlackTokenURL_Override(t *testing.T) {
	srv := newInstallTestServer(t)
	srv.slackTokenURL = "http://localhost:9999/token"
	if got := srv.getSlackTokenURL(); got != "http://localhost:9999/token" {
		t.Errorf("getSlackTokenURL = %q, want http://localhost:9999/token", got)
	}
}

func TestBuildRedirectURI(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		header   http.Header
		path     string
		expected string
	}{
		{
			name:     "plain http",
			url:      "http://localhost:8080/v1/slack/install/callback?code=test",
			path:     "/v1/slack/install/callback",
			expected: "http://localhost:8080/v1/slack/install/callback",
		},
		{
			name:     "forwarded https",
			url:      "http://aileron.example.com/v1/slack/install/callback?code=test",
			header:   http.Header{"X-Forwarded-Proto": {"https"}},
			path:     "/v1/slack/install/callback",
			expected: "https://aileron.example.com/v1/slack/install/callback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.url, nil)
			for k, vs := range tt.header {
				for _, v := range vs {
					req.Header.Set(k, v)
				}
			}
			got := buildRedirectURI(req, tt.path)
			if got != tt.expected {
				t.Errorf("buildRedirectURI = %q, want %q", got, tt.expected)
			}
		})
	}
}
