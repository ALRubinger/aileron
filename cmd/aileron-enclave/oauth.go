package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/ALRubinger/aileron/internal/enclave"
)

// doOAuthExchange calls the provider's token endpoint to exchange an
// authorization code for tokens. Returns the full token JSON (for encrypted
// vault storage), the access token (for fetching user info), and the token type.
// oauthExchangeResult holds the results of an OAuth token exchange.
type oauthExchangeResult struct {
	tokenJSON      []byte
	accessToken    string
	tokenType      string
	externalUserID string // provider-specific user ID (e.g. Slack authed_user.id)
	externalTeamID string // provider-specific team ID (e.g. Slack team.id)
}

func doOAuthExchange(ctx context.Context, req enclave.OAuthExchangeRequest) (*oauthExchangeResult, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"redirect_uri":  {req.RedirectURI},
		"client_id":     {req.ClientID},
		"client_secret": {req.ClientSecret},
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.TokenEndpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Body = io.NopCloser(stringReader(form.Encode()))

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenData struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		// Slack-specific: user token is nested under authed_user.
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
	if err := json.Unmarshal(body, &tokenData); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	result := &oauthExchangeResult{
		accessToken: tokenData.AccessToken,
		tokenType:   tokenData.TokenType,
		tokenJSON:   body,
	}

	// Slack's oauth.v2.access nests the user token under authed_user.
	// Normalize to store only the user token fields, matching the direct
	// (non-TEE) code path in SlackService.HandleCallback.
	if req.Provider == "slack" && tokenData.AuthedUser.AccessToken != "" {
		result.accessToken = tokenData.AuthedUser.AccessToken
		result.tokenType = tokenData.AuthedUser.TokenType
		result.externalUserID = tokenData.AuthedUser.ID
		result.externalTeamID = tokenData.Team.ID
		normalized, err := json.Marshal(map[string]string{
			"access_token": tokenData.AuthedUser.AccessToken,
			"token_type":   tokenData.AuthedUser.TokenType,
			"scope":        tokenData.AuthedUser.Scope,
			"team_id":      tokenData.Team.ID,
			"team_name":    tokenData.Team.Name,
			"user_id":      tokenData.AuthedUser.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("marshalling normalized token: %w", err)
		}
		result.tokenJSON = normalized
	}

	if result.accessToken == "" {
		return nil, fmt.Errorf("no access token returned")
	}

	return result, nil
}

// doFetchEmail retrieves the user's email from the userinfo endpoint.
func doFetchEmail(ctx context.Context, userinfoEndpoint, accessToken string) (string, error) {
	if userinfoEndpoint == "" {
		return "", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
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

type stringReaderImpl struct {
	s string
	i int
}

func (r *stringReaderImpl) Read(b []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(b, r.s[r.i:])
	r.i += n
	return n, nil
}

func stringReader(s string) io.Reader {
	return &stringReaderImpl{s: s}
}
