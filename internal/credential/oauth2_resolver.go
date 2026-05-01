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
	"time"

	"github.com/ALRubinger/aileron/internal/vault"
)

// ErrOAuth2RefreshFailed means the runtime tried to refresh an
// expired access token using the stored refresh token but the
// provider rejected the request (revoked refresh token, expired
// credential, scope changed). Surfaces as `binding_failed` per
// ADR-0010 — the agent's tool result tells the user to run
// `aileron binding rebind <name>`.
var ErrOAuth2RefreshFailed = errors.New("credential: oauth2 refresh failed")

// OAuth2Token is the structured payload persisted in the vault for
// an oauth2-kind binding. Stored as JSON in the entry's Value; the
// vault encrypts it like any other secret.
//
// `ClientID` and `TokenURL` are duplicated from the connector's
// manifest so the resolver can refresh without re-reading the
// connector's `[capabilities.credential.oauth2]` block on every
// request. They're not secrets; they're persisted alongside the
// tokens for self-containment.
type OAuth2Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type,omitempty"` // "Bearer" if absent
	ClientID     string    `json:"client_id"`
	TokenURL     string    `json:"token_url"`
	Scopes       []string  `json:"scopes,omitempty"`
}

// OAuth2VaultResolver wraps the api_key VaultResolver pattern with
// transparent token refresh. On Resolve:
//
//  1. Fetches the JSON envelope from the vault.
//  2. If the access token is within RefreshLeeway of expiring,
//     POSTs a refresh request to the stored TokenURL using the
//     stored RefreshToken + ClientID.
//  3. Persists the new envelope (refresh-token rotation: Google
//     rotates, Slack doesn't — we always store whatever the response
//     gave back).
//  4. Returns the (refreshed or original) access token as the
//     Credential.Value.
//
// The host's injectCredential is unaware of refresh. It calls Resolve
// and gets back access-token bytes to set as `Authorization: Bearer`.
type OAuth2VaultResolver struct {
	// Vault is the user's unlocked credential vault.
	Vault vault.Vault

	// VaultPath is the binding name that doubles as the vault path
	// (`<kind>/<service>/<identity>`).
	VaultPath string

	// RefreshLeeway is how close to expiry the resolver triggers a
	// refresh, to absorb clock drift and prevent a token expiring
	// mid-request. 60s is a sensible default; tests inject smaller.
	// Zero value means use the package default (60s).
	RefreshLeeway time.Duration

	// HTTPClient drives the refresh POST. nil → http.DefaultClient.
	HTTPClient *http.Client

	// Now returns "now" for expiry comparisons. nil → time.Now.
	// Tests inject deterministic clocks.
	Now func() time.Time
}

// Resolve implements [Resolver].
func (r *OAuth2VaultResolver) Resolve(ctx context.Context) (Credential, error) {
	if r == nil || r.Vault == nil {
		return Credential{}, ErrNoBindingResolver
	}
	secret, err := r.Vault.Get(ctx, r.VaultPath)
	if err != nil {
		if vault.IsNotFound(err) {
			return Credential{}, FormatBindingMissing(r.VaultPath)
		}
		return Credential{}, fmt.Errorf("oauth2 vault get: %w", err)
	}
	if secret.Metadata.Type != "oauth2" {
		return Credential{}, FormatKindMismatch("oauth2", secret.Metadata.Type)
	}

	var tok OAuth2Token
	if err := json.Unmarshal(secret.Value, &tok); err != nil {
		return Credential{}, fmt.Errorf("oauth2 vault payload parse: %w", err)
	}

	if r.shouldRefresh(tok) {
		newTok, err := r.refresh(ctx, tok)
		if err != nil {
			return Credential{}, err
		}
		if perr := r.persist(ctx, newTok, secret.Metadata); perr != nil {
			// We refreshed but failed to persist; return the new
			// access token anyway so the current request succeeds.
			// Subsequent requests will re-refresh — appropriate
			// degraded behavior since the worst case is one extra
			// refresh round trip.
			_ = perr
		}
		tok = newTok
	}

	return Credential{Kind: "oauth2", Value: []byte(tok.AccessToken)}, nil
}

// defaultRefreshLeeway is the window before expiry at which the
// resolver pre-emptively refreshes. Picked high enough to absorb
// upstream clock drift (typically <30s) and the cost of one HTTP
// request on slow links.
const defaultRefreshLeeway = 60 * time.Second

// shouldRefresh reports whether the access token is within
// RefreshLeeway of expiry.
func (r *OAuth2VaultResolver) shouldRefresh(tok OAuth2Token) bool {
	if tok.ExpiresAt.IsZero() {
		// No expiry recorded; treat as long-lived (some providers
		// issue tokens without a documented lifetime). Don't
		// gratuitously refresh.
		return false
	}
	if tok.RefreshToken == "" {
		// No refresh token — nothing we can do; let the upstream
		// surface a 401 if it's truly expired and let the user
		// rebind manually.
		return false
	}
	leeway := r.RefreshLeeway
	if leeway == 0 {
		leeway = defaultRefreshLeeway
	}
	return r.now().Add(leeway).After(tok.ExpiresAt)
}

// now returns the current time, allowing tests to inject a fake clock.
func (r *OAuth2VaultResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// refresh POSTs a refresh request to the provider's token endpoint
// per RFC 6749 §6 and parses the response.
func (r *OAuth2VaultResolver) refresh(ctx context.Context, tok OAuth2Token) (OAuth2Token, error) {
	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
		"client_id":     {tok.ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tok.TokenURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return OAuth2Token{}, fmt.Errorf("%w: build request: %v", ErrOAuth2RefreshFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := r.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return OAuth2Token{}, fmt.Errorf("%w: %v", ErrOAuth2RefreshFailed, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode/100 != 2 {
		return OAuth2Token{}, fmt.Errorf("%w: provider returned %d: %s",
			ErrOAuth2RefreshFailed, resp.StatusCode, string(respBody))
	}

	var parsed refreshResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return OAuth2Token{}, fmt.Errorf("%w: parse response: %v", ErrOAuth2RefreshFailed, err)
	}
	if parsed.AccessToken == "" {
		return OAuth2Token{}, fmt.Errorf("%w: response missing access_token", ErrOAuth2RefreshFailed)
	}

	// Refresh-token rotation: Google issues a new refresh_token on
	// every refresh; Slack/Notion etc. don't. Use whatever the
	// response gave us; fall back to the existing one when the
	// response omits it.
	newRefresh := parsed.RefreshToken
	if newRefresh == "" {
		newRefresh = tok.RefreshToken
	}
	expires := r.now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	if parsed.ExpiresIn == 0 {
		// Provider didn't tell us; preserve previous expiry to avoid
		// claiming the token is fresh forever.
		expires = tok.ExpiresAt
	}
	out := OAuth2Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: newRefresh,
		ExpiresAt:    expires,
		TokenType:    parsed.TokenType,
		ClientID:     tok.ClientID,
		TokenURL:     tok.TokenURL,
		Scopes:       tok.Scopes, // scope reductions are rare; preserve what was bound
	}
	return out, nil
}

// refreshResponse is the subset of RFC 6749 §5.1 token-response
// fields we consume.
type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// persist writes the (possibly refreshed) token back to the vault.
func (r *OAuth2VaultResolver) persist(ctx context.Context, tok OAuth2Token, meta vault.Metadata) error {
	bytes, err := json.Marshal(tok)
	if err != nil {
		return fmt.Errorf("oauth2 vault persist marshal: %w", err)
	}
	if err := r.Vault.Put(ctx, r.VaultPath, bytes, meta); err != nil {
		return fmt.Errorf("oauth2 vault persist put: %w", err)
	}
	return nil
}
