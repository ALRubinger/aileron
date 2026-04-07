package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/ALRubinger/aileron/enclave"
)

// doOAuthExchange calls the provider's token endpoint to exchange an
// authorization code for tokens. Returns the full token JSON (for encrypted
// vault storage), the access token (for fetching user info), and the token type.
func doOAuthExchange(ctx context.Context, req enclave.OAuthExchangeRequest) (tokenJSON []byte, accessToken, tokenType string, err error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"redirect_uri":  {req.RedirectURI},
		"client_id":     {req.ClientID},
		"client_secret": {req.ClientSecret},
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.TokenEndpoint, nil)
	if err != nil {
		return nil, "", "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Body = io.NopCloser(stringReader(form.Encode()))

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenData struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tokenData); err != nil {
		return nil, "", "", fmt.Errorf("parsing token response: %w", err)
	}

	if tokenData.RefreshToken == "" {
		return nil, "", "", fmt.Errorf("no refresh token returned; user may need to re-consent")
	}

	return body, tokenData.AccessToken, tokenData.TokenType, nil
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
