package account_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/account"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"golang.org/x/oauth2"
)

func newGitHubTestService(t *testing.T, tokenServer, userinfoServer *httptest.Server) (*account.GitHubAccountService, store.ConnectedAccountStore, vault.Vault) {
	t.Helper()
	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	svc := account.NewGitHubAccountService("gh-client-id", "gh-client-secret", accounts, v).
		WithEndpoint(oauth2.Endpoint{
			AuthURL:  tokenServer.URL + "/authorize",
			TokenURL: tokenServer.URL + "/token",
		})
	return svc, accounts, v
}

func mockGitHubTokenServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ghp_test_token_123",
			"token_type":   "bearer",
			"scope":        "repo,read:org",
		})
	}))
}

func mockGitHubUserServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"login": "alrubinger",
		})
	}))
}

func TestGitHubService_Providers(t *testing.T) {
	svc := account.NewGitHubAccountService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	providers := svc.Providers()
	if len(providers) != 1 || providers[0] != model.ConnectedAccountProviderGitHub {
		t.Fatalf("expected [github_repos], got %v", providers)
	}
}

func TestGitHubService_ClientID(t *testing.T) {
	svc := account.NewGitHubAccountService("my-id", "my-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	if svc.ClientID() != "my-id" {
		t.Errorf("expected my-id, got %s", svc.ClientID())
	}
}

func TestGitHubService_ClientSecret(t *testing.T) {
	svc := account.NewGitHubAccountService("my-id", "my-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	if svc.ClientSecret() != "my-secret" {
		t.Errorf("expected my-secret, got %s", svc.ClientSecret())
	}
}

func TestGitHubService_ScopesFor(t *testing.T) {
	svc := account.NewGitHubAccountService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	scopes := svc.ScopesFor(model.ConnectedAccountProviderGitHub)
	if len(scopes) == 0 {
		t.Fatal("expected non-empty scopes")
	}
	found := false
	for _, s := range scopes {
		if s == "repo" {
			found = true
		}
	}
	if !found {
		t.Error("expected repo in scopes")
	}
}

func TestGitHubService_TokenEndpointFor(t *testing.T) {
	svc := account.NewGitHubAccountService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	ep := svc.TokenEndpointFor(model.ConnectedAccountProviderGitHub)
	if ep == "" {
		t.Fatal("expected non-empty token endpoint")
	}
}

func TestGitHubService_UserInfoEndpoint(t *testing.T) {
	svc := account.NewGitHubAccountService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	ep := svc.UserInfoEndpoint()
	if ep == "" {
		t.Fatal("expected non-empty userinfo endpoint")
	}
}

func TestGitHubService_AuthorizationURL(t *testing.T) {
	svc := account.NewGitHubAccountService("gh-client-id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	result, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderGitHub, "state123", "http://localhost/callback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.URL == "" {
		t.Fatal("expected non-empty URL")
	}
	if !containsSubstr(result.URL, "github.com") {
		t.Errorf("expected github.com in URL, got %s", result.URL)
	}
	if !containsSubstr(result.URL, "gh-client-id") {
		t.Errorf("expected client_id in URL, got %s", result.URL)
	}
}

func TestGitHubService_HandleCallback_Success(t *testing.T) {
	tokenServer := mockGitHubTokenServer()
	defer tokenServer.Close()

	// The GitHub user endpoint needs to be called during HandleCallback.
	// Since fetchGitHubUsername uses the token's client which calls the real GitHub API,
	// we need to override. We'll use the token server to also handle /user.
	combinedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "ghp_test_token_123",
				"token_type":   "bearer",
			})
		case "/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "alrubinger"})
		default:
			// The oauth2 client will call the user endpoint using the full URL
			// so we also handle /api/v3/user or just /user
			json.NewEncoder(w).Encode(map[string]any{"login": "alrubinger"})
		}
	}))
	defer combinedServer.Close()

	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	svc := account.NewGitHubAccountService("gh-client-id", "gh-secret", accounts, v).
		WithEndpoint(oauth2.Endpoint{
			AuthURL:  combinedServer.URL + "/authorize",
			TokenURL: combinedServer.URL + "/token",
		}).
		WithUserInfoURL(combinedServer.URL + "/user")

	result, err := svc.HandleCallback(context.Background(), model.ConnectedAccountProviderGitHub, account.CallbackRequest{
		Code:        "test-code",
		State:       "test-state",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Account.Provider != model.ConnectedAccountProviderGitHub {
		t.Errorf("expected github_repos, got %s", result.Account.Provider)
	}
	if result.Account.ExternalUserID != "alrubinger" {
		t.Errorf("expected alrubinger, got %s", result.Account.ExternalUserID)
	}

	// Verify stored in accounts.
	listed, _ := accounts.List(context.Background(), store.ConnectedAccountFilter{UserID: "usr_test"})
	if len(listed) != 1 {
		t.Fatalf("expected 1 account, got %d", len(listed))
	}

	// Verify token in vault.
	secret, err := v.Get(context.Background(), result.Account.VaultPath())
	if err != nil {
		t.Fatalf("token not in vault: %v", err)
	}
	if len(secret.Value) == 0 {
		t.Error("expected non-empty token in vault")
	}
}

func TestGitHubService_List(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()
	svc := account.NewGitHubAccountService("id", "secret", accounts, vault.NewMemVault())
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: "usr_1", Provider: model.ConnectedAccountProviderGitHub,
	})

	listed, err := svc.List(ctx, "usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1, got %d", len(listed))
	}
}

func TestGitHubService_Get(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()
	svc := account.NewGitHubAccountService("id", "secret", accounts, vault.NewMemVault())
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: "usr_1", Provider: model.ConnectedAccountProviderGitHub,
	})

	acct, err := svc.Get(ctx, "conn_1")
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != "conn_1" {
		t.Errorf("expected conn_1, got %s", acct.ID)
	}
}

func TestGitHubService_Disconnect(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	svc := account.NewGitHubAccountService("id", "secret", accounts, v)
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: "usr_1", Provider: model.ConnectedAccountProviderGitHub,
	})
	v.Put(ctx, "connected-accounts/usr_1/github_repos", []byte(`{"token":"x"}`), vault.Metadata{})

	if err := svc.Disconnect(ctx, "conn_1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	listed, _ := accounts.List(ctx, store.ConnectedAccountFilter{UserID: "usr_1"})
	if len(listed) != 0 {
		t.Fatalf("expected 0 after disconnect, got %d", len(listed))
	}
}
