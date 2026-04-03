// Package auth defines the SPI for authentication providers.
//
// An AuthProvider bridges the control plane to an external identity provider —
// Google OAuth, Okta OIDC, a generic SAML 2.0 IdP, etc. When a user initiates
// login, the control plane selects the appropriate provider and delegates the
// authentication flow.
//
// Each provider implementation handles one external identity system. The SPI
// is intentionally narrow: providers generate an authorization URL and exchange
// a callback for an identity. Session management and JWT issuance happen in the
// control plane after the provider returns.
//
// The Enforcer interface checks enterprise-level SSO policies — whether a
// provider is allowed, whether SSO is required, and whether an email domain
// is permitted. These checks run after the provider returns an identity but
// before the user is granted a session.
package auth

import "context"

// AuthProvider authenticates users via an external identity provider.
type AuthProvider interface {
	// Provider returns the provider identifier (e.g. "google", "okta").
	Provider() string

	// AuthorizationURL returns the URL to redirect the user to for login.
	// The state parameter is an opaque CSRF token that must be echoed back
	// in the callback. redirectURL is the callback URL the provider should
	// redirect to after authentication.
	AuthorizationURL(ctx context.Context, state, redirectURL string) (string, error)

	// HandleCallback exchanges the authorization code from the IdP callback
	// for a resolved user identity.
	HandleCallback(ctx context.Context, req CallbackRequest) (*Identity, error)
}

// CallbackRequest contains the data from the OAuth/OIDC callback.
type CallbackRequest struct {
	Code        string
	State       string
	RedirectURL string // must match the redirectURL used in AuthorizationURL
}

// Identity is the authenticated user identity returned by a provider.
type Identity struct {
	// Subject is the provider-specific unique user identifier.
	Subject     string
	Email       string
	DisplayName string
	AvatarURL   string
	Provider    string
	// RawClaims carries the full set of claims from the IdP for extensibility.
	RawClaims map[string]any
}

// Enforcer checks enterprise-level SSO policies.
type Enforcer interface {
	// IsProviderAllowed reports whether the given auth provider is permitted
	// for the enterprise. Returns true if the enterprise has no provider
	// restrictions configured.
	IsProviderAllowed(ctx context.Context, enterpriseID string, provider string) (bool, error)

	// IsSSORequired reports whether the enterprise requires all users to
	// authenticate via a configured SSO provider.
	IsSSORequired(ctx context.Context, enterpriseID string) (bool, error)

	// IsEmailDomainAllowed reports whether the user's email domain is
	// permitted for the enterprise. Returns true if the enterprise has no
	// domain restrictions configured.
	IsEmailDomainAllowed(ctx context.Context, enterpriseID string, email string) (bool, error)
}

// Registry holds registered auth providers and resolves the correct one
// for a given provider name. It is safe for concurrent use.
type Registry struct {
	providers map[string]AuthProvider
}

// NewRegistry returns an empty auth provider registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]AuthProvider)}
}

// Register adds an auth provider to the registry.
func (r *Registry) Register(p AuthProvider) {
	r.providers[p.Provider()] = p
}

// Get returns the provider for the given name, or nil if not registered.
func (r *Registry) Get(name string) (AuthProvider, bool) {
	p, ok := r.providers[name]
	return p, ok
}
