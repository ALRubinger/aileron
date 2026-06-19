package agents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// codexAuthContainerPath is where the in-container Codex CLI reads
// its ChatGPT-mode credentials. The launcher mounts auth.json as a
// single file (MountAsFile = true) so it sits beside the read-only
// config.toml mount that ConfigureMCP installs under the same
// /home/agent/.codex/ directory. ADR-0025.
const codexAuthContainerPath = "/home/agent/.codex/auth.json"

// codexVaultPath is the canonical vault namespace for Codex's
// credentials per ADR-0025's `agents/<name>/<purpose>` scheme.
const codexVaultPath = "agents/codex/oauth"

// codexTokenURL is OpenAI's OAuth token endpoint used by the
// ChatGPT-mode refresh exchange. Pinned here so the value lives
// next to the Codex-specific refresh logic and is one grep away
// when OpenAI ships a new auth backend.
const codexTokenURL = "https://auth.openai.com/oauth/token"

// codexClientID is the OAuth client ID Codex CLI uses for the
// ChatGPT-mode device flow. Mirrors the value the in-container
// codex binary embeds. Verify against the live Codex install on
// upgrade; ChatGPT-mode auth has historically rolled the client ID
// only on major auth backend migrations.
const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// codexIssuerURL is the OAuth issuer Codex CLI authenticates against
// for ChatGPT mode. It is the base for both the device-authorization
// endpoints (under /api/accounts/deviceauth) and the OAuth token
// endpoint (codexTokenURL, /oauth/token). Verified against the live
// Codex source: codex-rs/login/src/server.rs `DEFAULT_ISSUER =
// "https://auth.openai.com"`.
const codexIssuerURL = "https://auth.openai.com"

// Device-authorization endpoints. OpenAI's Codex CLI does NOT use the
// RFC 8628 standard device-authorization grant. It uses a custom flow
// that the live source spells out in codex-rs/login/src/device_code_auth.rs:
//
//  1. POST {issuer}/api/accounts/deviceauth/usercode with {client_id}
//     returns {device_auth_id, user_code, interval}. The user is shown
//     the verification URL {issuer}/codex/device and the user_code.
//  2. Poll POST {issuer}/api/accounts/deviceauth/token with
//     {device_auth_id, user_code}. While the user has not finished,
//     the server replies 403/404 (NOT the RFC 8628 authorization_pending
//     JSON error); on success it returns
//     {authorization_code, code_challenge, code_verifier}.
//  3. Exchange that authorization_code via a standard PKCE
//     authorization_code grant against codexTokenURL with
//     redirect_uri = {issuer}/deviceauth/callback, yielding
//     {id_token, access_token, refresh_token}.
//
// The scope set is pinned server-side (the server mints the PKCE pair),
// so the device-code request carries only the client_id. The refresh
// token and id_token come back directly from the step-3 token endpoint;
// account_id is parsed from the id_token's chatgpt_account_id claim
// (see codexAccountIDFromIDToken). All citations are to
// github.com/openai/codex codex-rs/login/src as of June 2026.
const (
	codexDeviceUserCodeURL = codexIssuerURL + "/api/accounts/deviceauth/usercode"
	codexDeviceTokenURL    = codexIssuerURL + "/api/accounts/deviceauth/token"
	// codexDeviceVerificationURL is the page the user opens to enter the
	// user_code. Codex builds it as {issuer}/codex/device.
	codexDeviceVerificationURL = codexIssuerURL + "/codex/device"
	// codexDeviceRedirectURI is the redirect_uri the step-3 PKCE token
	// exchange must echo. Codex builds it as {issuer}/deviceauth/callback.
	codexDeviceRedirectURI = codexIssuerURL + "/deviceauth/callback"
)

// codexDeviceMaxWait bounds the device-code poll loop. Mirrors the
// 15-minute ceiling Codex's own poll_for_token enforces, matching the
// "expires in 15 minutes" copy its prompt prints.
const codexDeviceMaxWait = 15 * time.Minute

// codexDeviceDefaultInterval is the poll interval used when the
// usercode response omits one (the field defaults to 0 server-side).
const codexDeviceDefaultInterval = 5 * time.Second

// codexDeviceSlowDownIncrement is the RFC 8628 §3.5 +5s floor the
// acquirer adds to the poll interval whenever the server signals it is
// being polled too fast. Codex's custom flow does not emit the
// standard `slow_down` JSON error, but the launcher honors the floor
// defensively so a tightened interval never melts the endpoint.
const codexDeviceSlowDownIncrement = 5 * time.Second

// codexRefreshLeeway is the window before access-token expiry at
// which the pre-launch hook proactively refreshes. The brainstorm
// settled on 5 minutes to absorb upstream clock drift plus the
// time it takes the user to type their first prompt inside the
// container.
const codexRefreshLeeway = 5 * time.Minute

// codexAuthEnvelope mirrors the on-disk shape of Codex CLI's
// `~/.codex/auth.json` for the ChatGPT auth mode. The launcher
// renders and captures these bytes verbatim; the schema lives here
// so we can validate before any vault write and so the refresh
// hook can splice in rotated tokens without breaking the envelope.
//
// "Tolerant deserialization": fields outside this struct are
// preserved on capture because we round-trip the raw bytes — the
// envelope is parsed for validation, but the bytes Capture returns
// are the original bytes, not a re-marshaled subset. This keeps
// us forward-compatible with OpenAI adding fields the launcher
// does not know about.
type codexAuthEnvelope struct {
	AuthMode    string           `json:"auth_mode"`
	Tokens      codexAuthTokens  `json:"tokens"`
	LastRefresh string           `json:"last_refresh,omitempty"`
	OpenAI      *json.RawMessage `json:"openai,omitempty"` // preserve unknown sub-tree
}

type codexAuthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	// ExpiresAt is the timestamp the launcher consults for the
	// pre-launch refresh decision. OpenAI's Codex CLI does not
	// emit it in the on-disk format today (it tracks expiry
	// elsewhere); we annotate when we do the refresh so future
	// launches can skip cheaply.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// errCodexEnvelopeMalformed is the sentinel the Codex Capture func
// returns when the in-container file does not parse as the
// documented envelope. The launcher's CaptureFn surfaces this to
// stderr with recovery instructions per R13.
var errCodexEnvelopeMalformed = errors.New("codex: auth envelope is malformed")

// codexRender writes the vault's stored bytes into the in-container
// auth.json verbatim. Validates the envelope on the way through so
// a vault that holds non-Codex bytes (operator wrote the wrong file)
// fails the launch with a clear error before the container starts.
func codexRender(s vault.Secret) ([]byte, error) {
	if len(s.Value) == 0 {
		return nil, fmt.Errorf("%w: vault entry has empty Value (re-login or `aileron vault put %s` with a real envelope)",
			errCodexEnvelopeMalformed, codexVaultPath)
	}
	if _, err := parseCodexEnvelope(s.Value); err != nil {
		return nil, err
	}
	return s.Value, nil
}

// codexCapture validates the post-run file bytes against the
// envelope schema and produces a vault Secret. Bytes are byte-
// identity over the in-container file; future fields OpenAI adds
// round-trip cleanly because we preserve the raw bytes.
func codexCapture(b []byte) (vault.Secret, error) {
	if _, err := parseCodexEnvelope(b); err != nil {
		return vault.Secret{}, err
	}
	return vault.Secret{
		Value:    b,
		Metadata: vault.Metadata{Type: "oauth_refresh_token"},
	}, nil
}

// parseCodexEnvelope checks the JSON parses, the auth_mode is
// "chatgpt" (the only mode v1 supports via vault), and the
// refresh token is present. Returns the parsed struct so callers
// can read the refresh token / expires_at without re-parsing.
func parseCodexEnvelope(b []byte) (codexAuthEnvelope, error) {
	var env codexAuthEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return env, fmt.Errorf("%w: parse: %v", errCodexEnvelopeMalformed, err)
	}
	if env.AuthMode != "chatgpt" {
		return env, fmt.Errorf("%w: auth_mode = %q, want \"chatgpt\"",
			errCodexEnvelopeMalformed, env.AuthMode)
	}
	if env.Tokens.RefreshToken == "" {
		return env, fmt.Errorf("%w: tokens.refresh_token is empty",
			errCodexEnvelopeMalformed)
	}
	return env, nil
}

// codexFresher reports whether the captured Codex auth envelope is
// strictly newer than the one currently in the vault. The freshness
// timestamp is `tokens.expires_at` (RFC3339) when present, falling
// back to the top-level `last_refresh` when expires_at is empty —
// upstream Codex does not always populate expires_at, but it does
// stamp last_refresh after a rotation. Each side independently uses
// its own best-available timestamp; comparing a captured expires_at
// against a current last_refresh (or vice versa) is acceptable
// because both are monotonic proxies for "when this bundle was
// minted."
//
// captured strictly after current → fresher (true). Equal → false.
// If neither side yields a parseable timestamp but the access tokens
// differ, a real rotation happened without a usable freshness signal;
// we treat it as fresher rather than drop the write. A parse failure of
// the captured envelope returns a plain error so the launcher retains
// the prior entry; a parse failure of the current envelope wraps
// [launch.ErrCurrentEnvelopeMalformed] so the launcher overwrites the
// corrupt entry with the valid capture (ADR-0025).
func codexFresher(captured, current vault.Secret) (bool, error) {
	capEnv, err := parseCodexEnvelope(captured.Value)
	if err != nil {
		return false, fmt.Errorf("freshness parse captured: %w", err)
	}
	curEnv, err := parseCodexEnvelope(current.Value)
	if err != nil {
		return false, fmt.Errorf("%w: freshness parse current: %v", launch.ErrCurrentEnvelopeMalformed, err)
	}
	capTime, capOK := codexFreshnessTime(capEnv)
	curTime, curOK := codexFreshnessTime(curEnv)
	if capOK && curOK {
		return capTime.After(curTime), nil
	}
	// One or both sides lack a parseable timestamp. Don't drop a real
	// rotation: if the access tokens differ, treat captured as fresher.
	if capEnv.Tokens.AccessToken != curEnv.Tokens.AccessToken {
		return true, nil
	}
	return false, nil
}

// codexFreshnessTime returns the envelope's best-available freshness
// timestamp: tokens.expires_at when parseable, else last_refresh. The
// bool reports whether any timestamp parsed.
func codexFreshnessTime(env codexAuthEnvelope) (time.Time, bool) {
	if env.Tokens.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, env.Tokens.ExpiresAt); err == nil {
			return t, true
		}
	}
	if env.LastRefresh != "" {
		if t, err := time.Parse(time.RFC3339, env.LastRefresh); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// codexPreLaunchRefresh checks whether the stored access token is
// within codexRefreshLeeway of expiry. If so, it exchanges the
// refresh token for a new bundle via [credential.DoRefresh],
// rewrites the auth.json envelope with the rotated tokens, and
// PUTs the new bundle to the vault via the daemon BEFORE returning
// the Secret Render will write into the container. The AE6
// invariant — "rotated bundle is in vault before container start"
// — is enforced by failing the launch if the vault PUT fails;
// silent-degrade is explicitly rejected.
//
// If the access token is still fresh, the hook returns the input
// secret unchanged. The OAuth2 refresh is not free, so we skip it
// when possible.
func codexPreLaunchRefresh(s vault.Secret, deps launch.RefreshDeps) (vault.Secret, error) {
	env, err := parseCodexEnvelope(s.Value)
	if err != nil {
		return vault.Secret{}, fmt.Errorf("codex pre-launch refresh: %w", err)
	}

	if !codexShouldRefresh(env, time.Now()) {
		return s, nil
	}

	ctx := deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := credential.DoRefresh(ctx, credential.RefreshTokenParams{
		HTTPClient:   deps.HTTPClient,
		RefreshToken: env.Tokens.RefreshToken,
		ClientID:     codexClientID,
		TokenURL:     codexTokenURL,
	})
	if err != nil {
		// DoRefresh already strips the raw provider body from the
		// error per R21; we add the agent identifier and the
		// recovery hint a user can act on.
		return vault.Secret{}, fmt.Errorf("codex pre-launch refresh: %w (try `aileron vault delete %s && aileron launch codex --sandbox=docker` to re-login)",
			err, codexVaultPath)
	}

	rotatedRefresh := resp.RefreshToken
	if rotatedRefresh == "" {
		// OpenAI does not always rotate the refresh token on every
		// exchange. Preserve the prior one so the next launch can
		// refresh again.
		rotatedRefresh = env.Tokens.RefreshToken
	}

	// Mutate the envelope through a generic map so any forward-
	// compatible fields OpenAI adds (under the existing keys or as
	// new top-level keys) survive the round trip. Re-marshaling the
	// typed `codexAuthEnvelope` would silently drop anything not
	// represented in the Go struct, which is a real risk because the
	// Capture path otherwise preserves raw bytes byte-for-byte.
	var raw map[string]any
	if err := json.Unmarshal(s.Value, &raw); err != nil {
		return vault.Secret{}, fmt.Errorf("codex pre-launch refresh: re-parse envelope: %w", err)
	}
	tokens, _ := raw["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = resp.AccessToken
	tokens["refresh_token"] = rotatedRefresh
	if resp.ExpiresIn > 0 {
		tokens["expires_at"] = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).
			UTC().Format(time.RFC3339)
	}
	raw["tokens"] = tokens
	raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339)

	newBytes, err := json.Marshal(raw)
	if err != nil {
		return vault.Secret{}, fmt.Errorf("codex pre-launch refresh: marshal envelope: %w", err)
	}
	newSecret := vault.Secret{
		Value:    newBytes,
		Metadata: vault.Metadata{Type: "oauth_refresh_token"},
	}

	// Persist BEFORE returning so the AE6 invariant holds. If the
	// daemon PUT fails, we abort the launch and the vault is now
	// stale relative to the vendor: the prior refresh token may have
	// been invalidated by the rotation, leaving the user's only
	// recovery path a fresh in-container login. We surface that
	// recovery path explicitly rather than implying retry will
	// succeed.
	if err := deps.PutAgentCredentials(newSecret); err != nil {
		return vault.Secret{}, fmt.Errorf("codex pre-launch refresh: persist rotated bundle to vault: %w (the vendor rotated the refresh token but the new bundle did not reach the vault, so the next launch cannot refresh again; recover with `aileron secret set agents/codex/oauth` and a fresh login, or delete the vault entry and re-run `aileron launch codex --sandbox=docker`)",
			err)
	}
	return newSecret, nil
}

// codexShouldRefresh reports whether the access token in the
// envelope is within codexRefreshLeeway of expiry. An empty
// expires_at field is treated as "always refresh" because we have
// no other freshness signal — preferring an unnecessary refresh
// over an expired token mid-session.
func codexShouldRefresh(env codexAuthEnvelope, now time.Time) bool {
	if env.Tokens.ExpiresAt == "" {
		// First launch after seeding (the brainstorm note that
		// upstream Codex doesn't populate expires_at): we don't
		// know how fresh the token is, so refresh defensively.
		return true
	}
	expiresAt, err := time.Parse(time.RFC3339, env.Tokens.ExpiresAt)
	if err != nil {
		// Malformed timestamp from a stored envelope — refresh to
		// recover; the rotated envelope will overwrite the bad value.
		return true
	}
	return now.Add(codexRefreshLeeway).After(expiresAt)
}

// codexDeviceSleep is the sleep seam the poll loop uses between
// attempts. It is a package var so tests drive the loop deterministically
// (returning immediately and recording the requested durations) rather
// than sleeping real seconds. Production uses a context-aware sleep so a
// cancelled launch interrupts the wait. Returns the context error when
// the wait is cancelled, nil when it completes.
var codexDeviceSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// codexDeviceUserCodeResponse is the JSON the usercode endpoint returns.
// Field names mirror Codex's UserCodeResp (device_auth_id, user_code,
// interval) in codex-rs/login/src/device_code_auth.rs. The upstream
// struct accepts a string-encoded interval via a custom deserializer; we
// model interval as a number here and tolerate its absence (the launcher
// falls back to codexDeviceDefaultInterval).
type codexDeviceUserCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	Interval     int    `json:"interval"`
}

// codexDevicePollResponse is the success body the deviceauth/token
// endpoint returns once the user finishes the browser approval. It is
// NOT a token bundle: Codex's custom flow hands back an OAuth
// authorization code plus the server-minted PKCE pair, which the
// acquirer then exchanges for tokens against codexTokenURL. Field names
// mirror Codex's CodeSuccessResp.
type codexDevicePollResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

// codexDeviceTokenResponse is the body the OAuth token endpoint returns
// for the device-flow PKCE authorization_code exchange. Codex's
// TokenResponse (codex-rs/login/src/server.rs exchange_code_for_tokens)
// captures id_token, access_token, and refresh_token. We deliberately do
// NOT route this through credential.RefreshTokenResponse: that type has
// no id_token field and would silently drop the claim that carries the
// account_id the in-container Codex CLI needs for ChatGPT mode (P0).
//
// The error field is present so a non-2xx body's RFC 6749 `error` code
// can be surfaced without leaking error_description.
type codexDeviceTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

// codexHostAcquire is Codex's host-side credential acquirer, wired to the
// FileBinding's HostAcquire hook (see AuthSpec in codex.go). The launcher
// invokes it only when the vault GET misses, the binding is not Required,
// and host-login is enabled; it returns a vault.Secret the launcher PUTs
// to agents/codex/oauth and renders before the container starts.
//
// Unlike Claude (which has a setup-token CLI shortcut and a paste-back
// hosted-callback fallback), Codex has a single mechanism: the
// OpenAI device-authorization flow. It needs no localhost callback and
// no host CLI, so it is pure Go end-to-end:
//
//  1. Request a device code (codexRequestDeviceCode).
//  2. Surface the verification URL + user_code on deps.Out and open the
//     verification URL in the host browser (best-effort).
//  3. Poll to completion (codexPollDeviceToken), yielding an OAuth
//     authorization code + the server-minted PKCE pair.
//  4. Exchange that code for tokens (codexExchangeDeviceToken).
//  5. Assemble the chatgpt-mode auth.json envelope
//     (codexEnvelopeFromDeviceToken), parsing account_id from the
//     id_token.
//
// Contract (per #1268, enforced by the launcher):
//   - A returned Secret with a non-empty Value seeds the vault.
//   - An empty Secret (with a nil error) or a non-nil error is NON-FATAL:
//     the launcher logs and falls back to the legacy in-container
//     device-auth login. So a cancel / failed poll / failed exchange must
//     surface as one of those, never as a partial seed.
func codexHostAcquire(ctx context.Context, deps launch.HostAcquireDeps) (vault.Secret, error) {
	dc, err := codexRequestDeviceCode(ctx, deps.HTTPClient)
	if err != nil {
		return vault.Secret{}, err
	}

	// Surface the verification URL + user_code BEFORE opening the
	// browser, so a headless host (or a failed browser open) still gives
	// the user everything they need to finish the login by hand.
	if deps.Out != nil {
		fmt.Fprintf(deps.Out,
			"To authorize Codex, open %s in your browser and enter the code: %s\n",
			codexDeviceVerificationURL, dc.UserCode)
	}
	if deps.Browser != nil {
		if err := deps.Browser.Open(codexDeviceVerificationURL); err != nil {
			// A failed browser open is non-fatal for the device flow: the
			// user already has the verification URL + code on deps.Out, so
			// they can finish in any browser. Return non-fatal so the
			// launcher falls back to the in-container device-auth login if
			// the host cannot open a browser AND the user does not act.
			return vault.Secret{}, fmt.Errorf("codex: open verification URL: %w", err)
		}
	}

	poll, err := codexPollDeviceToken(ctx, deps.HTTPClient, dc)
	if err != nil {
		return vault.Secret{}, err
	}

	tok, err := codexExchangeDeviceToken(ctx, deps.HTTPClient, poll)
	if err != nil {
		return vault.Secret{}, err
	}

	envelope, err := codexEnvelopeFromDeviceToken(tok)
	if err != nil {
		return vault.Secret{}, err
	}
	return vault.Secret{
		Value:    envelope,
		Metadata: vault.Metadata{Type: "oauth_refresh_token"},
	}, nil
}

// codexRequestDeviceCode POSTs the client_id to the usercode endpoint and
// returns the device-code response. Mirrors Codex's request_user_code:
// only the client_id is sent (the scope set is pinned server-side because
// the server mints the PKCE pair). On a non-2xx response it surfaces only
// the HTTP status, never the raw body, per R21.
func codexRequestDeviceCode(ctx context.Context, client *http.Client) (codexDeviceUserCodeResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	reqBody, err := json.Marshal(struct {
		ClientID string `json:"client_id"`
	}{ClientID: codexClientID})
	if err != nil {
		return codexDeviceUserCodeResponse{}, fmt.Errorf("codex: marshal device-code request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexDeviceUserCodeURL,
		strings.NewReader(string(reqBody)))
	if err != nil {
		return codexDeviceUserCodeResponse{}, fmt.Errorf("codex: build device-code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return codexDeviceUserCodeResponse{}, fmt.Errorf("codex: device-code request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		// Redact the raw body; surface only the HTTP status.
		return codexDeviceUserCodeResponse{}, fmt.Errorf("codex: device-code request failed: provider returned %d", resp.StatusCode)
	}
	var dc codexDeviceUserCodeResponse
	if err := json.Unmarshal(body, &dc); err != nil {
		return codexDeviceUserCodeResponse{}, fmt.Errorf("codex: parse device-code response: %w", err)
	}
	if dc.DeviceAuthID == "" || dc.UserCode == "" {
		return codexDeviceUserCodeResponse{}, errors.New("codex: device-code response missing device_auth_id or user_code")
	}
	return dc, nil
}

// codexPollDeviceToken polls the deviceauth/token endpoint until the user
// completes the browser approval, mirroring Codex's poll_for_token state
// machine:
//
//   - 2xx → the user finished; parse {authorization_code, code_challenge,
//     code_verifier} and return.
//   - 403 / 404 → still pending; keep polling at the current interval.
//   - any other status → clean terminal error (the device-auth grant was
//     denied or the device code expired). The raw body is never surfaced.
//
// The interval starts at the server-provided value (falling back to
// codexDeviceDefaultInterval) and is increased by codexDeviceSlowDownIncrement
// (RFC 8628 §3.5's +5s floor) whenever the server signals it is being
// polled too quickly (429 Too Many Requests). The loop is bounded by
// codexDeviceMaxWait and respects ctx cancellation throughout.
func codexPollDeviceToken(ctx context.Context, client *http.Client, dc codexDeviceUserCodeResponse) (codexDevicePollResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = codexDeviceDefaultInterval
	}
	deadline := time.Now().Add(codexDeviceMaxWait)

	reqBody, err := json.Marshal(struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
	}{DeviceAuthID: dc.DeviceAuthID, UserCode: dc.UserCode})
	if err != nil {
		return codexDevicePollResponse{}, fmt.Errorf("codex: marshal poll request: %w", err)
	}

	for {
		if err := ctx.Err(); err != nil {
			return codexDevicePollResponse{}, fmt.Errorf("codex: device poll cancelled: %w", err)
		}
		if time.Now().After(deadline) {
			return codexDevicePollResponse{}, errors.New("codex: device authorization timed out after 15 minutes")
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexDeviceTokenURL,
			strings.NewReader(string(reqBody)))
		if err != nil {
			return codexDevicePollResponse{}, fmt.Errorf("codex: build poll request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return codexDevicePollResponse{}, fmt.Errorf("codex: device poll: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		status := resp.StatusCode
		resp.Body.Close()

		switch {
		case status/100 == 2:
			var pr codexDevicePollResponse
			if err := json.Unmarshal(body, &pr); err != nil {
				return codexDevicePollResponse{}, fmt.Errorf("codex: parse poll success response: %w", err)
			}
			if pr.AuthorizationCode == "" || pr.CodeVerifier == "" {
				return codexDevicePollResponse{}, errors.New("codex: poll success response missing authorization_code or code_verifier")
			}
			return pr, nil
		case status == http.StatusForbidden || status == http.StatusNotFound:
			// Still pending — keep polling at the current interval.
		case status == http.StatusTooManyRequests:
			// Polling too fast: apply the RFC 8628 §3.5 +5s floor before
			// the next attempt.
			interval += codexDeviceSlowDownIncrement
		default:
			// Terminal error (denied / expired). Redact the raw body;
			// surface only the HTTP status.
			return codexDevicePollResponse{}, fmt.Errorf("codex: device authorization failed: provider returned %d", status)
		}

		if err := codexDeviceSleep(ctx, interval); err != nil {
			return codexDevicePollResponse{}, fmt.Errorf("codex: device poll cancelled: %w", err)
		}
	}
}

// codexExchangeDeviceToken performs the step-3 PKCE authorization_code
// exchange against codexTokenURL, mirroring Codex's
// exchange_code_for_tokens: the device flow's poll response carries the
// server-minted PKCE verifier, which this exchange echoes alongside the
// authorization_code and the device-flow redirect_uri. Returns the token
// bundle (id_token / access_token / refresh_token). On a non-2xx response
// it surfaces only the HTTP status plus the RFC 6749 `error` code, never
// the raw body or error_description (R21).
func codexExchangeDeviceToken(ctx context.Context, client *http.Client, poll codexDevicePollResponse) (codexDeviceTokenResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {poll.AuthorizationCode},
		"redirect_uri":  {codexDeviceRedirectURI},
		"client_id":     {codexClientID},
		"code_verifier": {poll.CodeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return codexDeviceTokenResponse{}, fmt.Errorf("codex: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return codexDeviceTokenResponse{}, fmt.Errorf("codex: token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		if code := codexParseTokenErrorCode(body); code != "" {
			return codexDeviceTokenResponse{}, fmt.Errorf("codex: token exchange failed: provider returned %d (%s)", resp.StatusCode, code)
		}
		return codexDeviceTokenResponse{}, fmt.Errorf("codex: token exchange failed: provider returned %d", resp.StatusCode)
	}
	var tok codexDeviceTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return codexDeviceTokenResponse{}, fmt.Errorf("codex: parse token response: %w", err)
	}
	if tok.AccessToken == "" {
		return codexDeviceTokenResponse{}, errors.New("codex: token response missing access_token")
	}
	if tok.RefreshToken == "" {
		// parseCodexEnvelope hard-requires a refresh token (ChatGPT mode
		// rotates it via codexPreLaunchRefresh). A response without one is
		// dead-on-arrival for the seeded envelope, so fail rather than
		// seed a non-refreshable credential.
		return codexDeviceTokenResponse{}, errors.New("codex: token response missing refresh_token (the seeded credential could not be refreshed; the device-flow scope must include offline_access)")
	}
	return tok, nil
}

// codexParseTokenErrorCode extracts the RFC 6749 §5.2 `error` field from a
// token-error body, if present. Returns "" when the body does not parse or
// carries no `error` so the caller surfaces the bare HTTP status. Only the
// short standardized code is surfaced; the free-text error_description is
// intentionally not, since it may echo request material (mirrors
// parseClaudeTokenErrorCode).
func codexParseTokenErrorCode(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	return e.Error
}

// codexEnvelopeFromDeviceToken assembles the on-disk chatgpt-mode auth.json
// envelope from a successful device-flow token exchange and marshals it.
// It populates BOTH tokens.id_token AND tokens.account_id so the seeded
// credential is FUNCTIONAL for the in-container Codex CLI's ChatGPT mode,
// not merely parseable (P0). account_id is parsed from the id_token's
// chatgpt_account_id claim; a missing claim is non-fatal (account_id stays
// empty) because Codex itself treats it as optional when no workspace is
// forced — but the id_token is always preserved verbatim.
//
// expires_at is stamped from the token response's expires_in when present
// (OpenAI does not always populate it; codexPreLaunchRefresh defensively
// refreshes when it is absent), and last_refresh is stamped to now in the
// RFC3339 format codexPreLaunchRefresh writes, so freshness comparisons
// line up across a seed and a later refresh.
func codexEnvelopeFromDeviceToken(resp codexDeviceTokenResponse) ([]byte, error) {
	accountID, err := codexAccountIDFromIDToken(resp.IDToken)
	if err != nil {
		// A malformed id_token is a non-fatal signal for account_id: leave
		// it empty rather than failing the whole acquire. The id_token
		// itself is still preserved so the in-container CLI can re-derive
		// what it needs.
		accountID = ""
	}
	env := codexAuthEnvelope{
		AuthMode: "chatgpt",
		Tokens: codexAuthTokens{
			AccessToken:  resp.AccessToken,
			RefreshToken: resp.RefreshToken,
			IDToken:      resp.IDToken,
			AccountID:    accountID,
		},
		LastRefresh: time.Now().UTC().Format(time.RFC3339),
	}
	if resp.ExpiresIn > 0 {
		env.Tokens.ExpiresAt = time.Now().
			Add(time.Duration(resp.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal acquired envelope: %w", err)
	}
	return b, nil
}

// codexAccountIDFromIDToken extracts the ChatGPT account id from an
// OAuth id_token. OpenAI nests its claims under a namespaced
// "https://api.openai.com/auth" object whose chatgpt_account_id field is
// the account id the Codex CLI mints into auth.json. This mirrors the
// live Codex source exactly (codex-rs/login/src/server.rs jwt_auth_claims
// + persist_tokens_async, June 2026):
//
//	jwt_auth_claims(id_token)["https://api.openai.com/auth"]["chatgpt_account_id"]
//
// Only the JWT payload segment (the base64url-encoded middle segment) is
// decoded; no signature verification is needed for claim extraction
// because the id_token came directly from the provider's token endpoint
// over TLS. Returns an error for a malformed JWT or a missing claim; the
// caller treats that as non-fatal (account_id stays empty).
func codexAccountIDFromIDToken(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", errors.New("codex: id_token is not a well-formed JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("codex: base64url-decode id_token payload: %w", err)
	}
	var claims struct {
		OpenAIAuth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("codex: parse id_token claims: %w", err)
	}
	if claims.OpenAIAuth.ChatGPTAccountID == "" {
		return "", errors.New("codex: id_token missing chatgpt_account_id claim")
	}
	return claims.OpenAIAuth.ChatGPTAccountID, nil
}
