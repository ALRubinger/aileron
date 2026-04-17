package account

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	oauth2github "golang.org/x/oauth2/github"
)

// GitHub OAuth scopes for repository access.
var githubRepoScopes = []string{
	"repo",     // full access to private repos (includes read)
	"read:org", // read org membership
}

// GitHubAccountService manages connected accounts for GitHub repository access.
// This is separate from the GitHub auth provider (core/auth/github/) which
// handles Aileron login. This service connects a user's GitHub repos so the
// source connector can read code, PRs, and issues.
type GitHubAccountService struct {
	clientID     string
	clientSecret string
	accounts     store.ConnectedAccountStore
	vault        vault.Vault

	// Testing hooks.
	exchangeToken    tokenExchanger
	endpointOverride oauth2.Endpoint
	userInfoURL      string // overrides https://api.github.com/user for testing
}

// NewGitHubAccountService creates a service for GitHub connected accounts.
func NewGitHubAccountService(clientID, clientSecret string, accounts store.ConnectedAccountStore, v vault.Vault) *GitHubAccountService {
	return &GitHubAccountService{
		clientID:     clientID,
		clientSecret: clientSecret,
		accounts:     accounts,
		vault:        v,
	}
}

// WithEndpoint returns a shallow copy with an overridden OAuth endpoint (for testing).
func (s *GitHubAccountService) WithEndpoint(endpoint oauth2.Endpoint) *GitHubAccountService {
	cp := *s
	cp.endpointOverride = endpoint
	return &cp
}

// WithUserInfoURL returns a shallow copy with an overridden user info URL (for testing).
func (s *GitHubAccountService) WithUserInfoURL(url string) *GitHubAccountService {
	cp := *s
	cp.userInfoURL = url
	return &cp
}

func (s *GitHubAccountService) getEndpoint() oauth2.Endpoint {
	if s.endpointOverride != (oauth2.Endpoint{}) {
		return s.endpointOverride
	}
	return oauth2github.Endpoint
}

func (s *GitHubAccountService) getExchanger() tokenExchanger {
	if s.exchangeToken != nil {
		return s.exchangeToken
	}
	return defaultTokenExchange
}

func (s *GitHubAccountService) ClientID() string     { return s.clientID }
func (s *GitHubAccountService) ClientSecret() string { return s.clientSecret }

func (s *GitHubAccountService) ScopesFor(_ model.ConnectedAccountProvider) []string {
	return githubRepoScopes
}

func (s *GitHubAccountService) TokenEndpointFor(_ model.ConnectedAccountProvider) string {
	return s.getEndpoint().TokenURL
}

func (s *GitHubAccountService) UserInfoEndpoint() string {
	return "https://api.github.com/user"
}

func (s *GitHubAccountService) Providers() []model.ConnectedAccountProvider {
	return []model.ConnectedAccountProvider{
		model.ConnectedAccountProviderGitHub,
	}
}

func (s *GitHubAccountService) oauthConfig(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.clientID,
		ClientSecret: s.clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     s.getEndpoint(),
		Scopes:       githubRepoScopes,
	}
}

func (s *GitHubAccountService) AuthorizationURL(_ context.Context, _ model.ConnectedAccountProvider, state, redirectURL string) (*ConnectResult, error) {
	cfg := s.oauthConfig(redirectURL)
	url := cfg.AuthCodeURL(state)
	return &ConnectResult{URL: url}, nil
}

func (s *GitHubAccountService) HandleCallback(ctx context.Context, _ model.ConnectedAccountProvider, req CallbackRequest) (*CallbackResult, error) {
	cfg := s.oauthConfig(req.RedirectURL)

	token, err := s.getExchanger()(ctx, cfg, req.Code)
	if err != nil {
		return nil, fmt.Errorf("exchanging code: %w", err)
	}

	// Fetch GitHub username.
	userInfoURL := "https://api.github.com/user"
	if s.userInfoURL != "" {
		userInfoURL = s.userInfoURL
	}
	username, err := fetchGitHubUsername(ctx, cfg, token, userInfoURL)
	if err != nil {
		return nil, fmt.Errorf("fetching GitHub user: %w", err)
	}

	account := model.ConnectedAccount{
		ID:             "conn_" + uuid.New().String(),
		UserID:         req.UserID,
		Provider:       model.ConnectedAccountProviderGitHub,
		Scopes:         githubRepoScopes,
		Status:         model.ConnectedAccountStatusActive,
		ExternalUserID: username,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return nil, fmt.Errorf("marshalling token: %w", err)
	}
	if err := s.vault.Put(ctx, account.VaultPath(), tokenJSON, vault.Metadata{
		Type: "oauth_token",
		Labels: map[string]string{
			"provider": string(model.ConnectedAccountProviderGitHub),
			"user_id":  req.UserID,
		},
	}); err != nil {
		return nil, fmt.Errorf("storing token in vault: %w", err)
	}

	if err := s.accounts.Create(ctx, account); err != nil {
		return nil, fmt.Errorf("creating account record: %w", err)
	}

	return &CallbackResult{Account: account}, nil
}

func (s *GitHubAccountService) List(ctx context.Context, userID string) ([]model.ConnectedAccount, error) {
	return s.accounts.List(ctx, store.ConnectedAccountFilter{UserID: userID})
}

func (s *GitHubAccountService) Get(ctx context.Context, accountID string) (model.ConnectedAccount, error) {
	return s.accounts.Get(ctx, accountID)
}

func (s *GitHubAccountService) Disconnect(ctx context.Context, accountID string) error {
	account, err := s.accounts.Get(ctx, accountID)
	if err != nil {
		return err
	}
	if err := s.vault.Delete(ctx, account.VaultPath()); err != nil {
		return fmt.Errorf("removing token from vault: %w", err)
	}
	return s.accounts.Delete(ctx, accountID)
}

// fetchGitHubUsername calls the GitHub user API to get the username.
func fetchGitHubUsername(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token, userInfoURL string) (string, error) {
	client := cfg.Client(ctx, token)
	resp, err := client.Get(userInfoURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", fmt.Errorf("decoding user response: %w", err)
	}
	return user.Login, nil
}
