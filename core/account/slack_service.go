package account

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/google/uuid"
)

// Slack OAuth v2 endpoints.
const (
	slackAuthorizeURL   = "https://slack.com/oauth/v2/authorize"
	slackTokenURL       = "https://slack.com/api/oauth.v2.access"
	slackUserInfoURL    = "" // Slack returns user info in the OAuth response itself
)

// slackUserScopes are the OAuth user scopes requested for Slack.
// These enable reading channel messages and sending as the user.
var slackUserScopes = []string{
	"channels:history",
	"channels:read",
	"chat:write",
	"users:read",
}

// slackOAuthResponse is the non-standard response from Slack's oauth.v2.access endpoint.
type slackOAuthResponse struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	AuthedUser struct {
		ID          string `json:"id"`
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		TokenType   string `json:"token_type"`
	} `json:"authed_user"`
	Team struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
}

// slackTokenExchanger exchanges an authorization code for a Slack OAuth response.
type slackTokenExchanger func(ctx context.Context, clientID, clientSecret, code, redirectURI string) (*slackOAuthResponse, error)

// SlackService manages connected accounts for Slack.
type SlackService struct {
	clientID     string
	clientSecret string
	accounts     store.ConnectedAccountStore
	vault        vault.Vault

	// Testing hooks.
	exchangeToken    slackTokenExchanger
	authorizeURL     string // overrides slackAuthorizeURL for testing
	tokenURLOverride string // overrides slackTokenURL for testing
}

// NewSlackService creates a service that handles Slack account connections.
func NewSlackService(clientID, clientSecret string, accounts store.ConnectedAccountStore, v vault.Vault) *SlackService {
	return &SlackService{
		clientID:     clientID,
		clientSecret: clientSecret,
		accounts:     accounts,
		vault:        v,
	}
}

// WithEndpoints returns a shallow copy with overridden OAuth endpoints for testing.
func (s *SlackService) WithEndpoints(authorizeURL, tokenURL string) *SlackService {
	cp := *s
	cp.authorizeURL = authorizeURL
	cp.tokenURLOverride = tokenURL
	return &cp
}

func (s *SlackService) getExchanger() slackTokenExchanger {
	if s.exchangeToken != nil {
		return s.exchangeToken
	}
	return s.defaultTokenExchange
}

func (s *SlackService) getAuthorizeURL() string {
	if s.authorizeURL != "" {
		return s.authorizeURL
	}
	return slackAuthorizeURL
}

func (s *SlackService) getTokenURL() string {
	if s.tokenURLOverride != "" {
		return s.tokenURLOverride
	}
	return slackTokenURL
}

func (s *SlackService) ClientID() string     { return s.clientID }
func (s *SlackService) ClientSecret() string { return s.clientSecret }

func (s *SlackService) ScopesFor(_ model.ConnectedAccountProvider) []string {
	return slackUserScopes
}

func (s *SlackService) TokenEndpointFor(_ model.ConnectedAccountProvider) string {
	return s.getTokenURL()
}

func (s *SlackService) UserInfoEndpoint() string {
	return slackUserInfoURL
}

func (s *SlackService) Providers() []model.ConnectedAccountProvider {
	return []model.ConnectedAccountProvider{
		model.ConnectedAccountProviderSlack,
	}
}

func (s *SlackService) AuthorizationURL(_ context.Context, _ model.ConnectedAccountProvider, state, redirectURL string) (*ConnectResult, error) {
	params := url.Values{
		"client_id":    {s.clientID},
		"user_scope":   {strings.Join(slackUserScopes, ",")},
		"redirect_uri": {redirectURL},
		"state":        {state},
	}
	return &ConnectResult{URL: s.getAuthorizeURL() + "?" + params.Encode()}, nil
}

func (s *SlackService) HandleCallback(ctx context.Context, _ model.ConnectedAccountProvider, req CallbackRequest) (*CallbackResult, error) {
	resp, err := s.getExchanger()(ctx, s.clientID, s.clientSecret, req.Code, req.RedirectURL)
	if err != nil {
		return nil, fmt.Errorf("exchanging code: %w", err)
	}

	if resp.AuthedUser.AccessToken == "" {
		return nil, fmt.Errorf("no user access token returned")
	}

	account := model.ConnectedAccount{
		ID:             "conn_" + uuid.New().String(),
		UserID:         req.UserID,
		Provider:       model.ConnectedAccountProviderSlack,
		Email:          "", // Slack OAuth does not return email by default
		Scopes:         slackUserScopes,
		Status:         model.ConnectedAccountStatusActive,
		ExternalUserID: resp.AuthedUser.ID,
		ExternalTeamID: resp.Team.ID,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	// Store the user token in the vault.
	tokenJSON, err := json.Marshal(map[string]string{
		"access_token": resp.AuthedUser.AccessToken,
		"token_type":   resp.AuthedUser.TokenType,
		"scope":        resp.AuthedUser.Scope,
		"team_id":      resp.Team.ID,
		"team_name":    resp.Team.Name,
		"user_id":      resp.AuthedUser.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling token: %w", err)
	}
	if err := s.vault.Put(ctx, account.VaultPath(), tokenJSON, vault.Metadata{
		Type: "oauth_user_token",
		Labels: map[string]string{
			"provider": string(model.ConnectedAccountProviderSlack),
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

func (s *SlackService) List(ctx context.Context, userID string) ([]model.ConnectedAccount, error) {
	return s.accounts.List(ctx, store.ConnectedAccountFilter{UserID: userID})
}

func (s *SlackService) Get(ctx context.Context, accountID string) (model.ConnectedAccount, error) {
	return s.accounts.Get(ctx, accountID)
}

func (s *SlackService) Disconnect(ctx context.Context, accountID string) error {
	account, err := s.accounts.Get(ctx, accountID)
	if err != nil {
		return err
	}
	if err := s.vault.Delete(ctx, account.VaultPath()); err != nil {
		return fmt.Errorf("removing token from vault: %w", err)
	}
	return s.accounts.Delete(ctx, accountID)
}

// defaultTokenExchange performs the Slack OAuth v2 token exchange.
// Slack's response format differs from standard OAuth2 — the user token
// is nested under authed_user rather than at the top level.
func (s *SlackService) defaultTokenExchange(_ context.Context, clientID, clientSecret, code, redirectURI string) (*slackOAuthResponse, error) {
	data := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	resp, err := http.PostForm(s.getTokenURL(), data)
	if err != nil {
		return nil, fmt.Errorf("posting to token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}

	var oauthResp slackOAuthResponse
	if err := json.Unmarshal(body, &oauthResp); err != nil {
		return nil, fmt.Errorf("decoding token response: %w", err)
	}

	if !oauthResp.OK {
		return nil, fmt.Errorf("slack oauth error: %s", oauthResp.Error)
	}

	return &oauthResp, nil
}
