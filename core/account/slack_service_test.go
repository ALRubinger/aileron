package account_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ALRubinger/aileron/core/account"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func newSlackTestService(t *testing.T, tokenServer *httptest.Server) (*account.SlackService, store.ConnectedAccountStore, vault.Vault) {
	t.Helper()
	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	svc := account.NewSlackService("slack-client-id", "slack-client-secret", accounts, v).
		WithEndpoints("https://slack.com/oauth/v2/authorize", tokenServer.URL)
	return svc, accounts, v
}

func TestSlackService_Providers(t *testing.T) {
	svc := account.NewSlackService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	providers := svc.Providers()
	if len(providers) != 1 || providers[0] != model.ConnectedAccountProviderSlack {
		t.Fatalf("expected [slack], got %v", providers)
	}
}

func TestSlackService_ClientID(t *testing.T) {
	svc := account.NewSlackService("my-id", "my-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	if svc.ClientID() != "my-id" {
		t.Errorf("expected my-id, got %s", svc.ClientID())
	}
}

func TestSlackService_ClientSecret(t *testing.T) {
	svc := account.NewSlackService("my-id", "my-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	if svc.ClientSecret() != "my-secret" {
		t.Errorf("expected my-secret, got %s", svc.ClientSecret())
	}
}

func TestSlackService_ScopesFor(t *testing.T) {
	svc := account.NewSlackService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	scopes := svc.ScopesFor(model.ConnectedAccountProviderSlack)
	if len(scopes) == 0 {
		t.Fatal("expected non-empty scopes")
	}
	found := false
	for _, s := range scopes {
		if s == "chat:write" {
			found = true
		}
	}
	if !found {
		t.Error("expected chat:write in scopes")
	}
}

func TestSlackService_ScopesIncludeSearchRead(t *testing.T) {
	// slack_search_messages requires the search:read OAuth user scope.
	// Without it, Slack returns a permissions error when the LLM tries
	// to search message history during draft generation.
	svc := account.NewSlackService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	scopes := svc.ScopesFor(model.ConnectedAccountProviderSlack)

	found := false
	for _, s := range scopes {
		if s == "search:read" {
			found = true
		}
	}
	if !found {
		t.Error("expected search:read in Slack user scopes — required by slack_search_messages tool")
	}
}

func TestSlackService_AuthorizationURL(t *testing.T) {
	svc := account.NewSlackService("test-client-id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	result, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderSlack, "state123", "http://localhost/callback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.URL == "" {
		t.Fatal("expected non-empty URL")
	}
	if !contains(result.URL, "slack.com/oauth/v2/authorize") {
		t.Errorf("expected Slack authorize URL, got %s", result.URL)
	}
	if !contains(result.URL, "test-client-id") {
		t.Errorf("expected client_id in URL, got %s", result.URL)
	}
	if !contains(result.URL, "state123") {
		t.Errorf("expected state in URL, got %s", result.URL)
	}
	if !contains(result.URL, "user_scope=") {
		t.Errorf("expected user_scope parameter in URL, got %s", result.URL)
	}
}

func TestSlackService_AuthorizationURL_NoBotScope(t *testing.T) {
	// The user OAuth flow must NOT include the scope parameter (bot scopes).
	// Omitting scope causes Slack to skip the bot install prompt and only
	// ask for user consent.
	svc := account.NewSlackService("test-client-id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	result, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderSlack, "state123", "http://localhost/callback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contains(result.URL, "scope=") && !contains(result.URL, "user_scope=") {
		t.Errorf("URL should not contain bare scope= parameter (bot scopes), got %s", result.URL)
	}
	// More precise: parse the URL and check there's no "scope" key
	u, err := url.Parse(result.URL)
	if err != nil {
		t.Fatalf("failed to parse URL: %v", err)
	}
	if u.Query().Get("scope") != "" {
		t.Errorf("expected no scope parameter (bot scopes), but found: %s", u.Query().Get("scope"))
	}
}

func TestSlackService_HandleCallback_Success(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"authed_user": map[string]any{
				"id":           "U123ABC",
				"access_token": "xoxp-user-token-123",
				"scope":        "channels:history,channels:read,chat:write,users:read",
				"token_type":   "user",
			},
			"team": map[string]any{
				"id":   "T001TEAM",
				"name": "Test Workspace",
			},
		})
	}))
	defer tokenServer.Close()

	svc, accounts, v := newSlackTestService(t, tokenServer)
	ctx := context.Background()

	result, err := svc.HandleCallback(ctx, model.ConnectedAccountProviderSlack, account.CallbackRequest{
		Code:        "test-code",
		State:       "test-state",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the account was created.
	if result.Account.Provider != model.ConnectedAccountProviderSlack {
		t.Errorf("expected slack provider, got %s", result.Account.Provider)
	}
	if result.Account.ExternalUserID != "U123ABC" {
		t.Errorf("expected U123ABC, got %s", result.Account.ExternalUserID)
	}
	if result.Account.ExternalTeamID != "T001TEAM" {
		t.Errorf("expected T001TEAM, got %s", result.Account.ExternalTeamID)
	}
	if result.Account.UserID != "usr_test" {
		t.Errorf("expected usr_test, got %s", result.Account.UserID)
	}

	// Verify the account was stored.
	listed, err := accounts.List(ctx, store.ConnectedAccountFilter{UserID: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 account, got %d", len(listed))
	}

	// Verify the token was stored in the vault.
	secret, err := v.Get(ctx, result.Account.VaultPath())
	if err != nil {
		t.Fatalf("token not in vault: %v", err)
	}
	var tokenData map[string]string
	json.Unmarshal(secret.Value, &tokenData)
	if tokenData["access_token"] != "xoxp-user-token-123" {
		t.Errorf("expected xoxp-user-token-123, got %s", tokenData["access_token"])
	}
	if tokenData["team_id"] != "T001TEAM" {
		t.Errorf("expected T001TEAM in token data, got %s", tokenData["team_id"])
	}
	// Bot token must NOT be stored per-user — it lives at the workspace level.
	if tokenData["bot_access_token"] != "" {
		t.Errorf("expected no bot_access_token in per-user token data, got %s", tokenData["bot_access_token"])
	}
}

func TestSlackBotTokenVaultPath(t *testing.T) {
	path := account.SlackBotTokenVaultPath("T001TEAM")
	if path != "slack-workspaces/T001TEAM/bot-token" {
		t.Errorf("expected slack-workspaces/T001TEAM/bot-token, got %s", path)
	}
}

func TestSlackService_HandleCallback_OAuthError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "invalid_code",
		})
	}))
	defer tokenServer.Close()

	svc, _, _ := newSlackTestService(t, tokenServer)

	_, err := svc.HandleCallback(context.Background(), model.ConnectedAccountProviderSlack, account.CallbackRequest{
		Code:        "bad-code",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_test",
	})
	if err == nil {
		t.Fatal("expected error for OAuth failure")
	}
	if !contains(err.Error(), "invalid_code") {
		t.Errorf("expected error to mention invalid_code, got: %v", err)
	}
}

func TestSlackService_HandleCallback_NoAccessToken(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"authed_user": map[string]any{"id": "U123"},
			"team":        map[string]any{"id": "T001"},
		})
	}))
	defer tokenServer.Close()

	svc, _, _ := newSlackTestService(t, tokenServer)

	_, err := svc.HandleCallback(context.Background(), model.ConnectedAccountProviderSlack, account.CallbackRequest{
		Code:        "code",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_test",
	})
	if err == nil {
		t.Fatal("expected error for missing access token")
	}
}

func TestSlackService_Disconnect(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"authed_user": map[string]any{
				"id":           "U123",
				"access_token": "xoxp-token",
				"token_type":   "user",
			},
			"team": map[string]any{"id": "T001", "name": "Test"},
		})
	}))
	defer tokenServer.Close()

	svc, accounts, _ := newSlackTestService(t, tokenServer)
	ctx := context.Background()

	// Connect first.
	result, err := svc.HandleCallback(ctx, model.ConnectedAccountProviderSlack, account.CallbackRequest{
		Code:        "code",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_test",
	})
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Disconnect.
	if err := svc.Disconnect(ctx, result.Account.ID); err != nil {
		t.Fatalf("disconnect failed: %v", err)
	}

	// Verify deleted.
	listed, _ := accounts.List(ctx, store.ConnectedAccountFilter{UserID: "usr_test"})
	if len(listed) != 0 {
		t.Fatalf("expected 0 accounts after disconnect, got %d", len(listed))
	}
}

func TestSlackService_TokenEndpointFor(t *testing.T) {
	svc := account.NewSlackService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	ep := svc.TokenEndpointFor(model.ConnectedAccountProviderSlack)
	if ep == "" {
		t.Fatal("expected non-empty token endpoint")
	}
	if !containsSubstr(ep, "slack.com") {
		t.Errorf("expected slack.com in endpoint, got %s", ep)
	}
}

func TestSlackService_UserInfoEndpoint(t *testing.T) {
	svc := account.NewSlackService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	ep := svc.UserInfoEndpoint()
	// Slack returns user info in OAuth response, so this is empty.
	if ep != "" {
		t.Errorf("expected empty userinfo endpoint, got %s", ep)
	}
}

func TestSlackService_List(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()
	svc := account.NewSlackService("id", "secret", accounts, vault.NewMemVault())
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
	})

	listed, err := svc.List(ctx, "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 account, got %d", len(listed))
	}
}

func TestSlackService_Get(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()
	svc := account.NewSlackService("id", "secret", accounts, vault.NewMemVault())
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_s1",
		UserID:   "usr_1",
		Provider: model.ConnectedAccountProviderSlack,
	})

	acct, err := svc.Get(ctx, "conn_s1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.ID != "conn_s1" {
		t.Errorf("expected conn_s1, got %s", acct.ID)
	}
}

func TestSlackService_Get_NotFound(t *testing.T) {
	svc := account.NewSlackService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	_, err := svc.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing account")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
