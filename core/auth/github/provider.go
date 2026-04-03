// Package github implements the AuthProvider SPI for GitHub OAuth 2.0.
//
// It uses GitHub's OAuth flow to authenticate users and retrieve their
// profile information (email, name, avatar). When the user's email is
// set to private, the provider falls back to the /user/emails API to
// find the primary verified email. The provider is stateless; session
// management is handled by the caller.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/ALRubinger/aileron/core/auth"
	"golang.org/x/oauth2"
	oauthgithub "golang.org/x/oauth2/github"
)

const (
	userURL       = "https://api.github.com/user"
	userEmailsURL = "https://api.github.com/user/emails"
)

// Provider implements auth.AuthProvider for GitHub OAuth 2.0.
type Provider struct {
	clientID     string
	clientSecret string
	endpoint     oauth2.Endpoint
}

// New creates a GitHub OAuth provider with the given credentials.
// The redirect URL is supplied per-request via AuthorizationURL and
// HandleCallback, allowing it to be derived dynamically from the
// incoming request host.
func New(clientID, clientSecret string) *Provider {
	return &Provider{
		clientID:     clientID,
		clientSecret: clientSecret,
		endpoint:     oauthgithub.Endpoint,
	}
}

// newConfig builds an oauth2.Config for a single request using the given redirectURL.
func (p *Provider) newConfig(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     p.endpoint,
		Scopes:       []string{"user:email", "read:user"},
	}
}

// Provider returns "github".
func (p *Provider) Provider() string { return "github" }

// AuthorizationURL returns the GitHub OAuth authorization URL.
func (p *Provider) AuthorizationURL(_ context.Context, state, redirectURL string) (*auth.AuthorizationResult, error) {
	url := p.newConfig(redirectURL).AuthCodeURL(state)
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

	// Fetch user profile.
	user, err := fetchUser(client)
	if err != nil {
		return nil, err
	}

	// GitHub may return null email if the user's email is private.
	// Fall back to the /user/emails endpoint.
	email := user.Email
	if email == "" {
		email, err = fetchPrimaryEmail(client)
		if err != nil {
			return nil, err
		}
	}

	displayName := user.Name
	if displayName == "" {
		displayName = user.Login
	}

	rawClaims := map[string]any{
		"id":         user.ID,
		"login":      user.Login,
		"name":       user.Name,
		"email":      email,
		"avatar_url": user.AvatarURL,
	}

	return &auth.Identity{
		Subject:     strconv.Itoa(user.ID),
		Email:       email,
		DisplayName: displayName,
		AvatarURL:   user.AvatarURL,
		Provider:    "github",
		RawClaims:   rawClaims,
	}, nil
}

// githubUser is the subset of the GitHub /user response we need.
type githubUser struct {
	ID        int    `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// githubEmail is a single entry from the /user/emails endpoint.
type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func fetchUser(client *http.Client) (*githubUser, error) {
	resp, err := client.Get(userURL)
	if err != nil {
		return nil, fmt.Errorf("fetching user profile: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("user API returned %d: %s", resp.StatusCode, body)
	}

	var user githubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("decoding user profile: %w", err)
	}
	return &user, nil
}

func fetchPrimaryEmail(client *http.Client) (string, error) {
	resp, err := client.Get(userEmailsURL)
	if err != nil {
		return "", fmt.Errorf("fetching user emails: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("user emails API returned %d: %s", resp.StatusCode, body)
	}

	var emails []githubEmail
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", fmt.Errorf("decoding user emails: %w", err)
	}

	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, nil
		}
	}

	return "", fmt.Errorf("no primary verified email found on GitHub account")
}
