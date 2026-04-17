package account

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// providerConfig holds the OAuth configuration for a connected account provider.
type providerConfig struct {
	provider model.ConnectedAccountProvider
	scopes   []string
	endpoint oauth2.Endpoint
}

// googleProviders maps provider identifiers to their OAuth scopes.
// Gmail and Google Calendar use the same Google OAuth endpoint but request
// different scopes.
var googleProviders = map[model.ConnectedAccountProvider]providerConfig{
	model.ConnectedAccountProviderGmail: {
		provider: model.ConnectedAccountProviderGmail,
		scopes: []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.send",
			"https://www.googleapis.com/auth/gmail.compose",
			"https://www.googleapis.com/auth/drive.readonly",
			"https://www.googleapis.com/auth/userinfo.email",
		},
		endpoint: google.Endpoint,
	},
	model.ConnectedAccountProviderGoogleCalendar: {
		provider: model.ConnectedAccountProviderGoogleCalendar,
		scopes: []string{
			"https://www.googleapis.com/auth/calendar",
			"https://www.googleapis.com/auth/calendar.events",
			"https://www.googleapis.com/auth/userinfo.email",
		},
		endpoint: google.Endpoint,
	},
}

const userinfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"

// tokenExchanger exchanges an authorization code for an OAuth token.
type tokenExchanger func(ctx context.Context, cfg *oauth2.Config, code string) (*oauth2.Token, error)

// emailFetcher retrieves the email from a Google userinfo response.
type emailFetcher func(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (string, error)

func defaultTokenExchange(ctx context.Context, cfg *oauth2.Config, code string) (*oauth2.Token, error) {
	return cfg.Exchange(ctx, code)
}

func defaultEmailFetch(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (string, error) {
	client := cfg.Client(ctx, token)
	return fetchGoogleEmail(client)
}

// GoogleService manages connected accounts for Google-based services.
type GoogleService struct {
	clientID     string
	clientSecret string
	accounts     store.ConnectedAccountStore
	vault        vault.Vault
	// Testing hooks — nil means use defaults.
	exchangeToken         tokenExchanger
	fetchEmail            emailFetcher
	tokenEndpointOverride string // overrides TokenEndpointFor (testing only)
	userinfoOverride      string // overrides UserInfoEndpoint (testing only)
}

// NewGoogleService creates a service that handles Gmail and Google Calendar connections.
func NewGoogleService(clientID, clientSecret string, accounts store.ConnectedAccountStore, v vault.Vault) *GoogleService {
	return &GoogleService{
		clientID:     clientID,
		clientSecret: clientSecret,
		accounts:     accounts,
		vault:        v,
	}
}

// WithVault returns a shallow copy of the service that uses the given vault
// for token storage. This enables per-request vault scoping (e.g. wrapping
// the base vault with a UserScopedVault to encrypt tokens with the user's KEK).
func (s *GoogleService) WithVault(v vault.Vault) *GoogleService {
	cp := *s
	cp.vault = v
	return &cp
}

func (s *GoogleService) getExchanger() tokenExchanger {
	if s.exchangeToken != nil {
		return s.exchangeToken
	}
	return defaultTokenExchange
}

func (s *GoogleService) getFetcher() emailFetcher {
	if s.fetchEmail != nil {
		return s.fetchEmail
	}
	return defaultEmailFetch
}

// ClientID returns Aileron's OAuth client ID (not a user secret).
func (s *GoogleService) ClientID() string { return s.clientID }

// ClientSecret returns Aileron's OAuth client secret (not a user secret).
func (s *GoogleService) ClientSecret() string { return s.clientSecret }

// ScopesFor returns the OAuth scopes for a given provider.
func (s *GoogleService) ScopesFor(provider model.ConnectedAccountProvider) []string {
	if pc, ok := googleProviders[provider]; ok {
		return pc.scopes
	}
	return nil
}

// TokenEndpointFor returns the OAuth token exchange URL for a provider.
func (s *GoogleService) TokenEndpointFor(provider model.ConnectedAccountProvider) string {
	if s.tokenEndpointOverride != "" {
		return s.tokenEndpointOverride
	}
	if pc, ok := googleProviders[provider]; ok {
		return pc.endpoint.TokenURL
	}
	return ""
}

// UserInfoEndpoint returns the Google userinfo URL.
func (s *GoogleService) UserInfoEndpoint() string {
	if s.userinfoOverride != "" {
		return s.userinfoOverride
	}
	return userinfoURL
}

// WithEndpoints returns a shallow copy with overridden token and userinfo
// endpoints. This is for testing only — production uses the hardcoded Google
// endpoints.
func (s *GoogleService) WithEndpoints(tokenEndpoint, userinfoEndpoint string) *GoogleService {
	cp := *s
	cp.tokenEndpointOverride = tokenEndpoint
	cp.userinfoOverride = userinfoEndpoint
	return &cp
}

func (s *GoogleService) Providers() []model.ConnectedAccountProvider {
	return []model.ConnectedAccountProvider{
		model.ConnectedAccountProviderGmail,
		model.ConnectedAccountProviderGoogleCalendar,
	}
}

func (s *GoogleService) oauthConfig(provider model.ConnectedAccountProvider, redirectURL string) (*oauth2.Config, error) {
	pc, ok := googleProviders[provider]
	if !ok {
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
	return &oauth2.Config{
		ClientID:     s.clientID,
		ClientSecret: s.clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     pc.endpoint,
		Scopes:       pc.scopes,
	}, nil
}

func (s *GoogleService) AuthorizationURL(_ context.Context, provider model.ConnectedAccountProvider, state, redirectURL string) (*ConnectResult, error) {
	cfg, err := s.oauthConfig(provider, redirectURL)
	if err != nil {
		return nil, err
	}
	url := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	return &ConnectResult{URL: url}, nil
}

func (s *GoogleService) HandleCallback(ctx context.Context, provider model.ConnectedAccountProvider, req CallbackRequest) (*CallbackResult, error) {
	cfg, err := s.oauthConfig(provider, req.RedirectURL)
	if err != nil {
		return nil, err
	}

	token, err := s.getExchanger()(ctx, cfg, req.Code)
	if err != nil {
		return nil, fmt.Errorf("exchanging code: %w", err)
	}

	if token.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token returned; user may need to re-consent")
	}

	email, err := s.getFetcher()(ctx, cfg, token)
	if err != nil {
		return nil, fmt.Errorf("fetching account email: %w", err)
	}

	pc := googleProviders[provider]

	account := model.ConnectedAccount{
		ID:             "conn_" + uuid.New().String(),
		UserID:         req.UserID,
		Provider:       provider,
		Scopes:         pc.scopes,
		Status:         model.ConnectedAccountStatusActive,
		ExternalUserID: email,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	// Store the refresh token in the vault.
	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return nil, fmt.Errorf("marshalling token: %w", err)
	}
	if err := s.vault.Put(ctx, account.VaultPath(), tokenJSON, vault.Metadata{
		Type: "oauth_refresh_token",
		Labels: map[string]string{
			"provider": string(provider),
			"user_id":  req.UserID,
		},
	}); err != nil {
		return nil, fmt.Errorf("storing token in vault: %w", err)
	}

	// Create the account record.
	if err := s.accounts.Create(ctx, account); err != nil {
		return nil, fmt.Errorf("creating account record: %w", err)
	}

	return &CallbackResult{Account: account}, nil
}

func (s *GoogleService) List(ctx context.Context, userID string) ([]model.ConnectedAccount, error) {
	return s.accounts.List(ctx, store.ConnectedAccountFilter{UserID: userID})
}

func (s *GoogleService) Get(ctx context.Context, accountID string) (model.ConnectedAccount, error) {
	return s.accounts.Get(ctx, accountID)
}

func (s *GoogleService) Disconnect(ctx context.Context, accountID string) error {
	account, err := s.accounts.Get(ctx, accountID)
	if err != nil {
		return err
	}

	// Remove token from vault.
	if err := s.vault.Delete(ctx, account.VaultPath()); err != nil {
		return fmt.Errorf("removing token from vault: %w", err)
	}

	// Delete the account record.
	return s.accounts.Delete(ctx, accountID)
}

// fetchGoogleEmail calls the Google userinfo endpoint to retrieve the email.
func fetchGoogleEmail(client *http.Client) (string, error) {
	resp, err := client.Get(userinfoURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("userinfo returned %d: %s", resp.StatusCode, body)
	}

	var claims struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return "", fmt.Errorf("decoding userinfo: %w", err)
	}
	return claims.Email, nil
}
