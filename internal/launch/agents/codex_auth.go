package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
