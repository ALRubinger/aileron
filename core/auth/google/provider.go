// Package google implements the AuthProvider SPI for Google OAuth 2.0.
//
// It uses Google's OpenID Connect flow to authenticate users and retrieve
// their profile information (email, name, avatar). The provider is stateless;
// session management is handled by the caller.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ALRubinger/aileron/core/auth"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const userinfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"

// Provider implements auth.AuthProvider for Google OAuth 2.0.
type Provider struct {
	clientID     string
	clientSecret string
	endpoint     oauth2.Endpoint
}

// New creates a Google OAuth provider with the given credentials.
// The redirect URL is supplied per-request via AuthorizationURL and HandleCallback,
// allowing it to be derived dynamically from the incoming request host.
func New(clientID, clientSecret string) *Provider {
	return &Provider{
		clientID:     clientID,
		clientSecret: clientSecret,
		endpoint:     google.Endpoint,
	}
}

// newConfig builds an oauth2.Config for a single request using the given redirectURL.
func (p *Provider) newConfig(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     p.endpoint,
		Scopes:       []string{"openid", "email", "profile"},
	}
}

// Provider returns "google".
func (p *Provider) Provider() string { return "google" }

// AuthorizationURL returns the Google OAuth consent URL.
// redirectURL must be the callback URL registered in the Google Cloud Console
// (or match the dynamic URL derived from the request host).
func (p *Provider) AuthorizationURL(_ context.Context, state, redirectURL string) (*auth.AuthorizationResult, error) {
	url := p.newConfig(redirectURL).AuthCodeURL(state, oauth2.AccessTypeOffline)
	return &auth.AuthorizationResult{URL: url}, nil
}

// HandleCallback exchanges the authorization code for user identity.
func (p *Provider) HandleCallback(ctx context.Context, req auth.CallbackRequest) (*auth.Identity, error) {
	cfg := p.newConfig(req.RedirectURL)
	token, err := cfg.Exchange(ctx, req.Code)
	if err != nil {
		return nil, fmt.Errorf("exchanging code: %w", err)
	}

	client := cfg.Client(ctx, token)
	resp, err := client.Get(userinfoURL)
	if err != nil {
		return nil, fmt.Errorf("fetching userinfo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("userinfo returned %d: %s", resp.StatusCode, body)
	}

	var claims struct {
		Sub     string `json:"sub"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return nil, fmt.Errorf("decoding userinfo: %w", err)
	}

	rawClaims := make(map[string]any)
	// Re-read is unnecessary; populate from typed fields.
	rawClaims["sub"] = claims.Sub
	rawClaims["email"] = claims.Email
	rawClaims["name"] = claims.Name
	rawClaims["picture"] = claims.Picture

	return &auth.Identity{
		Subject:     claims.Sub,
		Email:       claims.Email,
		DisplayName: claims.Name,
		AvatarURL:   claims.Picture,
		Provider:    "google",
		RawClaims:   rawClaims,
	}, nil
}
