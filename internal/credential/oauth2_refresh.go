package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// RefreshTokenResponse is the subset of RFC 6749 §5.1 token-response
// fields callers need from a refresh exchange. The shape is shared
// across OAuth2VaultResolver (binding refresh, used by connector
// execution) and per-agent launch-time refresh hooks (Codex today),
// per ADR-0025's "factor a generic doRefresh helper" decision.
type RefreshTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// RefreshTokenParams is the input bundle for DoRefresh. The caller
// owns the persistence shape that wraps the response; this helper
// owns only the HTTP exchange and parsing.
type RefreshTokenParams struct {
	HTTPClient   *http.Client // nil → http.DefaultClient
	RefreshToken string
	ClientID     string
	ClientSecret string // empty when the OAuth client has no secret (PKCE-only)
	TokenURL     string
	// ExtraForm adds vendor-specific fields to the POST body.
	// Codex's auth.openai.com refresh, for example, sends an extra
	// `scope` parameter on some flows. Empty map by default.
	ExtraForm url.Values
}

// ErrRefreshFailed is the typed sentinel callers wrap with
// errors.Is to discriminate "provider rejected our refresh" from
// transport errors or parse errors.
var ErrRefreshFailed = errors.New("credential: oauth2 refresh failed")

// DoRefresh POSTs an OAuth2 refresh-token grant to the provider's
// token endpoint and returns the parsed response. Errors are
// sanitized to "status N: <summary>" — the raw provider response
// body is NOT included in the returned error string (RFC 6749 §5.2
// allows providers to include token hints in error bodies, and we
// don't want those leaking into user-facing error surfaces per
// ADR-0025 / R21). Callers that need the body for debugging can
// log it at debug level after applying their own redaction policy.
//
// The helper does not persist the response. Codex's PreLaunchRefresh
// marshals the new tokens into its `auth.json` shape and persists
// via the daemon's PutAgentCredentials; OAuth2VaultResolver marshals
// into its OAuth2Token shape and persists via vault.Put.
func DoRefresh(ctx context.Context, p RefreshTokenParams) (RefreshTokenResponse, error) {
	if p.RefreshToken == "" {
		return RefreshTokenResponse{}, fmt.Errorf("%w: refresh_token is empty", ErrRefreshFailed)
	}
	if p.TokenURL == "" {
		return RefreshTokenResponse{}, fmt.Errorf("%w: token_url is empty", ErrRefreshFailed)
	}

	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {p.RefreshToken},
	}
	if p.ClientID != "" {
		body.Set("client_id", p.ClientID)
	}
	if p.ClientSecret != "" {
		body.Set("client_secret", p.ClientSecret)
	}
	for k, vs := range p.ExtraForm {
		for _, v := range vs {
			body.Add(k, v)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return RefreshTokenResponse{}, fmt.Errorf("%w: build request: %v", ErrRefreshFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return RefreshTokenResponse{}, fmt.Errorf("%w: %v", ErrRefreshFailed, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode/100 != 2 {
		// Strip the raw provider body from the user-facing error.
		// The body may include token hints or other sensitive
		// diagnostics per RFC 6749 §5.2. Surface only the HTTP
		// status; callers that want the body for debugging can
		// fetch it via the RFC 6749 error-response standard
		// fields the spec promises ("error", "error_description").
		errCode := parseStandardErrorCode(respBody)
		if errCode != "" {
			return RefreshTokenResponse{}, fmt.Errorf("%w: provider returned %d (%s)",
				ErrRefreshFailed, resp.StatusCode, errCode)
		}
		return RefreshTokenResponse{}, fmt.Errorf("%w: provider returned %d",
			ErrRefreshFailed, resp.StatusCode)
	}

	var parsed RefreshTokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return RefreshTokenResponse{}, fmt.Errorf("%w: parse response: %v", ErrRefreshFailed, err)
	}
	if parsed.AccessToken == "" {
		return RefreshTokenResponse{}, fmt.Errorf("%w: response missing access_token", ErrRefreshFailed)
	}
	return parsed, nil
}

// parseStandardErrorCode extracts the RFC 6749 §5.2 standard
// `error` field from a token-error response body. We surface this
// in user-facing errors because it is a small, well-typed set
// (invalid_grant, invalid_request, etc.) that does not include
// token hints or other sensitive material.
func parseStandardErrorCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error
}
