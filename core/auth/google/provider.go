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
	cfg *oauth2.Config
}

// New creates a Google OAuth provider with the given credentials.
// redirectURL is the callback URL (e.g. "https://app.example.com/auth/google/callback").
func New(clientID, clientSecret, redirectURL string) *Provider {
	return &Provider{
		cfg: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     google.Endpoint,
			Scopes:       []string{"openid", "email", "profile"},
		},
	}
}

// Provider returns "google".
func (p *Provider) Provider() string { return "google" }

// AuthorizationURL returns the Google OAuth consent URL.
func (p *Provider) AuthorizationURL(_ context.Context, state string) (string, error) {
	url := p.cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)
	return url, nil
}

// HandleCallback exchanges the authorization code for user identity.
func (p *Provider) HandleCallback(ctx context.Context, req auth.CallbackRequest) (*auth.Identity, error) {
	token, err := p.cfg.Exchange(ctx, req.Code)
	if err != nil {
		return nil, fmt.Errorf("exchanging code: %w", err)
	}

	client := p.cfg.Client(ctx, token)
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
