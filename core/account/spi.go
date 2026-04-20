// Package account manages connected external accounts.
//
// Connected accounts link a user's external service (Gmail, Google Calendar,
// Outlook, payment rails) to Aileron so it can execute irreversible actions
// on the user's behalf. This is distinct from the auth package which handles
// Aileron login — connected accounts grant Aileron access to external services
// via OAuth with service-specific scopes.
//
// The Service interface is the primary entry point. It orchestrates the OAuth
// flow, stores refresh tokens in the vault, and manages ConnectedAccount
// lifecycle records in the store.
package account

import (
	"context"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/vault"
)

// ConnectResult is returned after initiating an account connection flow.
type ConnectResult struct {
	// URL is the OAuth authorization URL to redirect the user to.
	URL string
}

// CallbackRequest contains the data from the OAuth callback for a connected account.
type CallbackRequest struct {
	Code        string
	State       string
	RedirectURL string
	UserID      string // authenticated user initiating the connection
}

// CallbackResult is returned after successfully connecting an account.
type CallbackResult struct {
	Account model.ConnectedAccount
}

// Service manages connected external accounts.
type Service interface {
	// Providers returns the list of supported provider identifiers.
	Providers() []model.ConnectedAccountProvider

	// AuthorizationURL returns the OAuth URL for connecting an external account.
	// The provider determines which service to connect (gmail, google_calendar, etc.).
	AuthorizationURL(ctx context.Context, provider model.ConnectedAccountProvider, state, redirectURL string) (*ConnectResult, error)

	// HandleCallback exchanges the OAuth code for tokens and creates a connected account.
	HandleCallback(ctx context.Context, provider model.ConnectedAccountProvider, req CallbackRequest) (*CallbackResult, error)

	// List returns all connected accounts for a user.
	List(ctx context.Context, userID string) ([]model.ConnectedAccount, error)

	// Get returns a specific connected account.
	Get(ctx context.Context, accountID string) (model.ConnectedAccount, error)

	// Disconnect removes a connected account and revokes its tokens.
	Disconnect(ctx context.Context, accountID string) error
}

// ProviderService extends Service with OAuth introspection methods needed
// by the TEE callback path. Each provider (Google, Slack, etc.) implements
// this interface so the enclave can exchange OAuth codes on the user's behalf.
type ProviderService interface {
	Service

	// ClientID returns the OAuth client ID for this provider.
	ClientID() string

	// ClientSecret returns the OAuth client secret for this provider.
	ClientSecret() string

	// ScopesFor returns the OAuth scopes requested for a given provider.
	ScopesFor(provider model.ConnectedAccountProvider) []string

	// TokenEndpointFor returns the OAuth token exchange URL for a provider.
	TokenEndpointFor(provider model.ConnectedAccountProvider) string

	// UserInfoEndpoint returns the URL to fetch user identity after OAuth.
	UserInfoEndpoint() string

	// WithVault returns a shallow copy of the service that uses the given vault
	// for token storage. This enables per-request vault scoping (e.g. wrapping
	// the base vault with a UserScopedVault for encrypted token storage).
	WithVault(v vault.Vault) ProviderService
}
