package agents

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

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/oauth"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Claude Code subscription-OAuth client + endpoints.
//
// These values are Anthropic's public Claude Code client; they were
// verified against the live `claude` install / public documentation
// before this code was written (see the PR body for #1270 for the
// citations). The client is a PUBLIC OAuth client — no client_secret —
// and is registered ONLY for the hosted callback below.
//
//   - claudeOAuthClientID is Claude Code's public client id.
//   - claudeAuthorizeURL is the consent page the host browser opens.
//   - claudeTokenURL is the token-exchange endpoint (PKCE, no secret).
//   - claudeHostedCallbackURL is the ONLY redirect_uri the token
//     endpoint accepts for this client. Loopback (127.0.0.1) redirect
//     URIs return `400 invalid_grant`, which is why the host flow uses
//     the hosted-callback "paste the code" mechanism rather than a
//     loopback listener or device-auth (see #1275 P0).
//
// The authorize page renders the authorization response as a
// `<code>#<state>` string for the user to copy; the acquirer splits on
// `#` before the token exchange.
const (
	claudeOAuthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeAuthorizeURL      = "https://claude.ai/oauth/authorize"
	claudeTokenURL          = "https://platform.claude.com/v1/oauth/token"
	claudeHostedCallbackURL = "https://platform.claude.com/oauth/code/callback"
	// claudeProfileURL is the OAuth profile endpoint Claude Code reads to
	// learn the signed-in organization's subscription tier. It is fetched
	// host-side with the freshly-exchanged access token (the
	// `user:profile` scope, requested in claudeOAuthScopes, authorizes
	// it) so the seeded envelope can carry a subscriptionType.
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	// claudeModelsURL is a cheap authenticated endpoint used to validate
	// a raw ANTHROPIC_API_KEY host-side before it is seeded into the
	// vault. Unlike the OAuth profile endpoint (Bearer token), API keys
	// authenticate with the `x-api-key` header, so this probe cannot
	// reuse claudeFetchProfile.
	claudeModelsURL = "https://api.anthropic.com/v1/models"
	// claudeAnthropicVersion is the stable Anthropic API version header
	// the api-key validation probe sends. It matches the version pinned
	// elsewhere in the codebase (internal/app handlers).
	claudeAnthropicVersion = "2023-06-01"
)

// claudeOAuthScopes are the scopes Claude Code requests for a
// subscription (Pro/Max) login. `user:inference` is what lets the
// seeded token drive model calls; the others mirror the upstream CLI's
// requested set so the consent screen matches what a native `claude`
// login shows.
var claudeOAuthScopes = []string{
	"user:inference",
	"user:profile",
	"user:sessions:claude_code",
	"user:mcp_servers",
}

// buildClaudeAuthorizeURL composes Claude's consent URL with PKCE +
// state. The `code=true` query param tells the authorize page to render
// the authorization response as a copyable `<code>#<state>` string
// (the hosted-callback paste mechanism) rather than auto-redirecting.
func buildClaudeAuthorizeURL(pkce oauth.PKCEPair, state string) string {
	q := url.Values{
		"code":                  {"true"},
		"client_id":             {claudeOAuthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {claudeHostedCallbackURL},
		"scope":                 {strings.Join(claudeOAuthScopes, " ")},
		"state":                 {state},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {pkce.Method},
	}
	sep := "?"
	if strings.Contains(claudeAuthorizeURL, "?") {
		sep = "&"
	}
	return claudeAuthorizeURL + sep + q.Encode()
}

// claudeEnvelopeFromTokenResponse converts a successful token-exchange
// response into the on-disk `claudeAiOauth` envelope.
//
// expiresInSec is the provider's `expires_in` (SECONDS). Claude's
// `.credentials.json` stores `claudeAiOauth.expiresAt` in MILLISECONDS
// (the real file carries values like 1775212290694), so we convert to
// an absolute ms timestamp. A non-positive expiresInSec leaves
// expiresAt at 0 (omitted), matching envelopes that ship without an
// expiry.
func claudeEnvelopeFromTokenResponse(access, refresh string, expiresInSec int, scopes []string, subscriptionType, rateLimitTier string) ([]byte, error) {
	env := claudeCredentialEnvelope{
		ClaudeAiOauth: claudeAiOauth{
			AccessToken:      access,
			RefreshToken:     refresh,
			Scopes:           scopes,
			SubscriptionType: subscriptionType,
			RateLimitTier:    rateLimitTier,
		},
	}
	if expiresInSec > 0 {
		env.ClaudeAiOauth.ExpiresAt = time.Now().
			Add(time.Duration(expiresInSec) * time.Second).UnixMilli()
	}
	b, err := json.Marshal(env)
	if err != nil {
		// The shape is fixed; marshaling only fails on a programming
		// error. Wrap rather than panic so a caller in the acquire path
		// degrades to the in-container fallback instead of crashing.
		return nil, fmt.Errorf("claude: marshal acquired envelope: %w", err)
	}
	return b, nil
}

// claudeCredentialsContainerPath is where Claude Code reads its
// subscription-OAuth credentials inside the sandbox container. The
// launcher bind-mounts the parent directory writable; Claude rotates
// the file mid-session via a tmpfile + rename dance.
const claudeCredentialsContainerPath = "/home/agent/.claude/.credentials.json"

// claudeOnboardingContainerPath is the agent's first-launch state
// file. Writing {"hasCompletedOnboarding": true} suppresses the theme
// picker and other onboarding prompts so a vault-rendered launch is
// silent end-to-end.
const claudeOnboardingContainerPath = "/home/agent/.claude.json"

// claudeVaultPath is the canonical vault namespace for Claude's
// subscription-OAuth credentials per ADR-0025's `agents/<name>/<purpose>`
// scheme. Subscription mode (Pro/Max) reads/writes the envelope here.
const claudeVaultPath = "agents/claude/oauth"

// claudeAPIKeyVaultPath is the vault namespace for Claude's raw
// `ANTHROPIC_API_KEY` credential, used by api-key auth mode. It is a
// physically distinct slot from claudeVaultPath: the subscription
// envelope (oauth) and the raw key (apikey) never share a destination,
// so selecting api-key never touches the oauth slot and vice versa. The
// `apikey` purpose is one of the known purposes vaultPathConforms
// accepts (internal/launch/authspec.go) and the daemon routes via the
// `purpose` query parameter.
const claudeAPIKeyVaultPath = "agents/claude/apikey"

// claudeAPIKeyEnv is the environment variable Claude Code reads its raw
// API key from in api-key auth mode. The launcher renders the vault's
// stored key into this env var via the EnvBinding.
const claudeAPIKeyEnv = "ANTHROPIC_API_KEY"

// errClaudeAPIKeyEmpty is returned by claudeAPIKeyRender when the
// resolved vault entry has no usable key. The launcher surfaces this to
// stderr with the recovery hint naming claudeAPIKeyVaultPath per R13.
var errClaudeAPIKeyEmpty = errors.New("claude: vault entry has empty API key")

// claudeAPIKeyRender maps the vault secret to Claude's
// ANTHROPIC_API_KEY env var for api-key auth mode. The api-key
// credential is a single opaque key, so the vault Value is the raw key
// bytes (no JSON envelope). We trim surrounding whitespace (a key
// copied from another machine can carry a trailing newline) and reject
// an empty value with a recovery hint naming the apikey slot, mirroring
// gooseRender. Because the raw key lives at a path distinct from the
// subscription envelope (claudeVaultPath), it can never be mistaken for
// or overwrite the OAuth credential (#1304-class ambiguity fix).
func claudeAPIKeyRender(s vault.Secret) (map[string]string, error) {
	key := strings.TrimSpace(string(s.Value))
	if key == "" {
		return nil, fmt.Errorf("%w (re-login or `aileron vault put %s` with the Anthropic API key)",
			errClaudeAPIKeyEmpty, claudeAPIKeyVaultPath)
	}
	return map[string]string{claudeAPIKeyEnv: key}, nil
}

// claudeAPIKeyHostAcquire is Claude's host-side acquirer for api-key
// auth mode, wired to the EnvBinding's HostAcquire hook (see AuthSpec in
// claude.go). The launcher invokes it only when the vault GET for
// agents/claude/apikey misses, the binding is not Required, and
// host-login is enabled; it returns a vault.Secret the launcher PUTs to
// agents/claude/apikey and renders into ANTHROPIC_API_KEY before the
// container starts.
//
// Unlike claudeHostAcquire's PKCE OAuth flow, this is a static-key
// paste: there is no browser, no token exchange, no profile fetch. We
// print a prompt to deps.Out and read the key from the host terminal
// via deps.CodePrompter so the paste stays on the HOST rather than the
// container TTY.
//
// Contract (per the EnvBinding.HostAcquire contract, enforced by the
// launcher):
//   - A returned Secret with a non-empty Value seeds the vault.
//   - A nil CodePrompter, an empty/whitespace-only paste, or a
//     cancelled/errored read is NON-FATAL: return an empty Secret (with
//     or without a benign error) so the launcher logs and falls back to
//     the legacy in-container login. A cancel must never seed a partial
//     credential.
func claudeAPIKeyHostAcquire(ctx context.Context, deps launch.HostAcquireDeps) (vault.Secret, error) {
	if deps.CodePrompter == nil {
		// No way to read the pasted key. Non-fatal: let the launcher
		// fall back to the in-container login.
		return vault.Secret{}, errors.New("claude: no code prompter available for api-key paste flow")
	}
	if deps.Out != nil {
		fmt.Fprintln(deps.Out,
			"Paste your Anthropic API key to seed Claude's api-key auth, then press Enter.")
	}
	// This flow prints its own (api-key-specific) paste banner above, so
	// the prompter is invoked with a nil promptW to suppress the
	// prompter's generic "Paste the code from your browser..." banner and
	// avoid a second, wrong-domain prompt. Mirrors claudeHostedCallbackAcquire.
	pasted, err := deps.CodePrompter(ctx, nil)
	if err != nil {
		// Read failed or was cancelled. Non-fatal: empty Secret so the
		// launcher falls back to the in-container login.
		return vault.Secret{}, fmt.Errorf("claude: read pasted api key: %w", err)
	}
	key := strings.TrimSpace(pasted)
	if key == "" {
		// Empty / whitespace-only paste (user declined). Non-fatal:
		// empty Secret, no seed, no error.
		return vault.Secret{}, nil
	}
	// Validate the pasted key host-side before seeding the vault. A
	// mistyped or revoked key would otherwise be persisted and the
	// container would launch with a dead key. On failure return an empty
	// Secret (the existing non-fatal contract) so the launcher logs and
	// falls back to the in-container login rather than seeding a dead key.
	if err := claudeValidateAPIKey(ctx, deps.HTTPClient, key); err != nil {
		if deps.Out != nil {
			fmt.Fprintf(deps.Out,
				"The pasted Anthropic API key did not validate (%v); not seeding it. Falling back to the in-container login.\n", err)
		}
		return vault.Secret{}, fmt.Errorf("claude: pasted api key failed validation: %w", err)
	}
	return vault.Secret{
		Value:    []byte(key),
		Metadata: vault.Metadata{Type: "api_key"},
	}, nil
}

// claudeValidateAPIKey confirms a raw ANTHROPIC_API_KEY authenticates by
// issuing a cheap GET against Claude's models endpoint with the
// `x-api-key` header. A 2xx confirms the key; a 401/403 (or any other
// non-2xx) means the key is mistyped or revoked. Network errors are also
// surfaced as failures so the launcher falls back rather than seeding an
// unvalidated key.
//
// API keys use `x-api-key`, NOT a Bearer token, so this deliberately does
// NOT reuse claudeFetchProfile. The HTTP client is taken from the caller
// (HostAcquireDeps.HTTPClient) so an httptest server can stand in.
// Mirroring claudeFetchProfile's posture, a non-2xx surfaces only the
// HTTP status; the raw body is redacted so a verbose or hostile provider
// response cannot leak into the user-facing launch error.
func claudeValidateAPIKey(ctx context.Context, client *http.Client, key string) error {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeModelsURL, nil)
	if err != nil {
		return fmt.Errorf("claude: build api-key validation request: %w", err)
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", claudeAnthropicVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("claude: api-key validation request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		// Redact the raw body; surface only the status.
		return fmt.Errorf("claude: api-key validation failed: provider returned %d", resp.StatusCode)
	}
	return nil
}

// claudeCredentialEnvelope mirrors the on-disk shape of Claude's
// `.credentials.json`. Anthropic's CLI writes a JSON document with
// a `claudeAiOauth` root key holding the access token, refresh
// token, expiry, and granted scopes. The launcher's vault payload
// is the same envelope byte-for-byte — Render/Capture are byte-
// identity functions over this shape — but we deserialize on the
// way in to enforce the schema invariant before any vault write.
//
// Field shapes intentionally use `json:",omitempty"` only where the
// upstream CLI leaves the field absent on a fresh login; we treat
// `claudeAiOauth.accessToken` as required because a credentials
// file missing it is not a usable Claude session.
type claudeCredentialEnvelope struct {
	ClaudeAiOauth claudeAiOauth `json:"claudeAiOauth"`
}

type claudeAiOauth struct {
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken,omitempty"`
	ExpiresAt    int64    `json:"expiresAt,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	// SubscriptionType is the Max/Pro tier Claude Code uses to decide
	// whether a seeded credential is a subscription session (Max/Pro/
	// Enterprise/Team) rather than a raw "Claude API" key. Without it the
	// CLI reports not-logged-in even with a valid access token
	// (anthropics/claude-code#34262). The host acquirer fetches the tier
	// from the profile endpoint after the token exchange and stamps it
	// here; "omitempty" keeps it absent on envelopes that never carried
	// a tier (e.g. those rendered verbatim from a vault entry).
	SubscriptionType string `json:"subscriptionType,omitempty"`
	// RateLimitTier mirrors the organization's rate-limit tier from the
	// profile endpoint. Claude Code surfaces it alongside the
	// subscription type; it is informational and absent when the profile
	// did not report one.
	RateLimitTier string `json:"rateLimitTier,omitempty"`
}

// errClaudeEnvelopeMalformed is returned by claudeCapture when the
// in-container file does not parse as the documented envelope. The
// launcher's CaptureFn surfaces this to stderr with the file path
// and the user's recovery options (inspect, re-login, etc.) per R13.
var errClaudeEnvelopeMalformed = errors.New("claude: credentials envelope is malformed")

// claudeWorkspacePath is the container directory the launcher runs
// Claude in (the bind-mounted project workspace). It must match
// container.WorkspacePath and the runtime `--workdir`; it is hardcoded
// here to keep the agents package free of an import on the container
// package, mirroring how the credential/onboarding paths above are
// spelled out literally.
const claudeWorkspacePath = "/home/agent/workspace"

// claudeOnboardingStub is the payload written to /home/agent/.claude.json
// on every launch. It short-circuits the first-run interruptions so a
// vault-rendered sandbox launch is silent end-to-end:
//
//   - hasCompletedOnboarding skips the theme picker / first-run wizard.
//   - installMethod "global" matches the devcontainer's
//     `npm install -g @anthropic-ai/claude-code`. Claude's `/doctor`
//     uses installMethod to decide where to verify the binary; "native"
//     made it look for ~/.local/bin/claude (the native-installer path,
//     which the npm install never creates) and report the CLI as
//     "missing or broken". "global" verifies the PATH-resolved global
//     binary the devcontainer actually installs.
//   - projects[workspace].hasTrustDialogAccepted pre-accepts the folder
//     trust dialog ("Is this a project you trust?") for the bind-mounted
//     workspace, so the agent does not block on it on every launch. The
//     sandbox container is the trust boundary, so pre-accepting inside
//     it is safe.
//   - bypassPermissionsModeAccepted pre-accepts the
//     "Bypassing Permissions" disclaimer that Claude Code shows the
//     first time it runs under --dangerously-skip-permissions. The
//     sandbox launch always passes that flag (Args(ModeSandbox)), so
//     without this the first launch blocks on an interactive
//     "do you want to dangerously skip permissions?" confirmation —
//     defeating the hands-off Cloud startup the flag exists to provide.
//     The opt-in is already expressed by Aileron passing the flag; the
//     container is the trust boundary (ADR-0015), so pre-accepting the
//     disclaimer inside it is consistent with the pre-accepted trust
//     dialog above. (#1379)
//
// claudeAPIKeySuffixLen is the number of trailing characters of the raw
// ANTHROPIC_API_KEY that Claude Code stores in
// `customApiKeyResponses.approved` to pre-approve an env-supplied key.
// Claude Code keys the approval on the LAST 20 characters of the raw key
// (the suffix, not a hash), so a .claude.json carrying that suffix
// suppresses the interactive "Detected a custom API key in your
// environment" confirmation that otherwise blocks a hands-off launch.
const claudeAPIKeySuffixLen = 20

// claudeProjectTrust is the per-project onboarding entry; the trust
// dialog is pre-accepted for the bind-mounted workspace.
type claudeProjectTrust struct {
	HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
}

// claudeCustomAPIKeyResponses pre-approves env-supplied API keys so
// Claude Code does not prompt "Detected a custom API key in your
// environment". The "approved" list carries the last
// claudeAPIKeySuffixLen characters of each approved key.
type claudeCustomAPIKeyResponses struct {
	Approved []string `json:"approved"`
}

// claudeOnboardingDoc is the typed shape of /home/agent/.claude.json.
// CustomApiKeyResponses uses ",omitempty" with a pointer so subscription
// mode (which sets it nil) emits a document byte-identical to the
// pre-#1695 stub — no customApiKeyResponses key at all.
type claudeOnboardingDoc struct {
	HasCompletedOnboarding        bool                          `json:"hasCompletedOnboarding"`
	BypassPermissionsModeAccepted bool                          `json:"bypassPermissionsModeAccepted"`
	InstallMethod                 string                        `json:"installMethod"`
	Projects                      map[string]claudeProjectTrust `json:"projects"`
	CustomApiKeyResponses         *claudeCustomAPIKeyResponses  `json:"customApiKeyResponses,omitempty"`
}

// newClaudeOnboardingDoc builds the onboarding document shared by both
// auth modes. The onboarding fields (hasCompletedOnboarding,
// bypassPermissionsModeAccepted, installMethod, projects trust) are
// constant; see the field comments in claudeOnboardingStub for why each
// is pre-accepted. apiKeyApprovals, when non-empty, is attached as
// customApiKeyResponses.approved.
func newClaudeOnboardingDoc(apiKeyApprovals []string) claudeOnboardingDoc {
	doc := claudeOnboardingDoc{
		HasCompletedOnboarding:        true,
		BypassPermissionsModeAccepted: true,
		InstallMethod:                 "global",
		Projects: map[string]claudeProjectTrust{
			claudeWorkspacePath: {HasTrustDialogAccepted: true},
		},
	}
	if len(apiKeyApprovals) > 0 {
		doc.CustomApiKeyResponses = &claudeCustomAPIKeyResponses{Approved: apiKeyApprovals}
	}
	return doc
}

// claudeOnboardingStub is the payload written to /home/agent/.claude.json
// in subscription mode (and the base for api-key mode). It short-circuits
// the first-run interruptions so a vault-rendered sandbox launch is
// silent end-to-end:
//
//   - hasCompletedOnboarding skips the theme picker / first-run wizard.
//   - installMethod "global" matches the devcontainer's
//     `npm install -g @anthropic-ai/claude-code`. Claude's `/doctor`
//     uses installMethod to decide where to verify the binary; "native"
//     made it look for ~/.local/bin/claude (the native-installer path,
//     which the npm install never creates) and report the CLI as
//     "missing or broken". "global" verifies the PATH-resolved global
//     binary the devcontainer actually installs.
//   - projects[workspace].hasTrustDialogAccepted pre-accepts the folder
//     trust dialog ("Is this a project you trust?") for the bind-mounted
//     workspace, so the agent does not block on it on every launch. The
//     sandbox container is the trust boundary, so pre-accepting inside
//     it is safe.
//   - bypassPermissionsModeAccepted pre-accepts the
//     "Bypassing Permissions" disclaimer that Claude Code shows the
//     first time it runs under --dangerously-skip-permissions. The
//     sandbox launch always passes that flag (Args(ModeSandbox)), so
//     without this the first launch blocks on an interactive
//     "do you want to dangerously skip permissions?" confirmation —
//     defeating the hands-off Cloud startup the flag exists to provide.
//     The opt-in is already expressed by Aileron passing the flag; the
//     container is the trust boundary (ADR-0015), so pre-accepting the
//     disclaimer inside it is consistent with the pre-accepted trust
//     dialog above. (#1379)
//
// In api-key mode the document gets one additional field,
// customApiKeyResponses.approved, derived per-launch from the rendered
// key; see claudeAPIKeyOnboardingContent. Subscription mode never carries
// it (no env key, no prompt). Built from a typed value rather than a
// string literal so the nested shape stays valid as Anthropic adds
// onboarding fields.
var claudeOnboardingStub = mustMarshalOnboardingStub()

func mustMarshalOnboardingStub() []byte {
	b, err := json.Marshal(newClaudeOnboardingDoc(nil))
	if err != nil {
		// The shape is a fixed literal; marshaling it cannot fail at
		// runtime. Panic so a refactor that breaks it surfaces in tests
		// rather than silently shipping an empty onboarding file.
		panic("claude: marshaling onboarding stub: " + err.Error())
	}
	return b
}

// claudeAPIKeySuffix returns the per-key approval token Claude Code
// stores in customApiKeyResponses.approved: the last
// claudeAPIKeySuffixLen characters of the key, or the whole key when it
// is shorter. The key must already be trimmed (claudeAPIKeyRender trims
// whitespace before rendering into ANTHROPIC_API_KEY) so the suffix
// matches the value Claude Code actually sees in its env. The suffix is
// measured in runes, not bytes, so a key with multi-byte characters
// yields the same trailing characters Claude Code's JS slice would.
func claudeAPIKeySuffix(key string) string {
	r := []rune(key)
	if len(r) <= claudeAPIKeySuffixLen {
		return key
	}
	return string(r[len(r)-claudeAPIKeySuffixLen:])
}

// claudeAPIKeyOnboardingContent is the StaticFile.RenderContent hook for
// api-key mode. The launcher invokes it after the EnvBinding renders the
// vault key into ANTHROPIC_API_KEY, passing the rendered env additions,
// so the .claude.json carries the suffix of the exact key Claude Code
// will see. When the env addition is absent (the api-key acquire fell
// back to the in-container login, so no key was rendered), it emits the
// plain onboarding stub — there is no env key for Claude Code to prompt
// about, so no approval is needed.
func claudeAPIKeyOnboardingContent(env map[string]string) ([]byte, error) {
	var approvals []string
	if key := strings.TrimSpace(env[claudeAPIKeyEnv]); key != "" {
		approvals = []string{claudeAPIKeySuffix(key)}
	}
	b, err := json.Marshal(newClaudeOnboardingDoc(approvals))
	if err != nil {
		return nil, fmt.Errorf("claude: marshal api-key onboarding doc: %w", err)
	}
	return b, nil
}

// claudeRender writes the vault's stored bytes into the in-container
// credentials file verbatim. We validate the envelope on the way
// through so a vault that holds non-Claude bytes (operator wrote the
// wrong file via `aileron vault put`) fails the launch with a clear
// error instead of letting Claude error out inside the container.
func claudeRender(s vault.Secret) ([]byte, error) {
	if len(s.Value) == 0 {
		return nil, fmt.Errorf("%w: vault entry has empty Value (re-login or `aileron vault put %s` with a real envelope)",
			errClaudeEnvelopeMalformed, claudeVaultPath)
	}
	if err := validateClaudeEnvelope(s.Value); err != nil {
		return nil, err
	}
	// Byte-identity write: Claude reads the file directly and
	// expects the original JSON shape, not a re-serialized form
	// (key order and whitespace are not significant but a
	// reformatted file would still parse). Returning the bytes
	// untouched also makes the round-trip property obvious.
	return s.Value, nil
}

// claudeCapture is the inverse: take the (possibly rotated) file
// bytes off the in-container filesystem and turn them into a vault
// Secret. The envelope is validated before the launcher PUTs the
// bytes back — a partial-write or schema-drift session is skipped
// rather than overwritten in vault.
func claudeCapture(b []byte) (vault.Secret, error) {
	if err := validateClaudeEnvelope(b); err != nil {
		return vault.Secret{}, err
	}
	return vault.Secret{
		Value:    b,
		Metadata: vault.Metadata{Type: "oauth_refresh_token"},
	}, nil
}

// claudeFresher reports whether the captured Claude credential
// envelope is strictly newer than the one currently in the vault,
// comparing `claudeAiOauth.expiresAt`. The launcher's CaptureFn
// calls this in the present@render/present@capture branch so a
// stale capture (e.g. a second concurrent launch that did not
// rotate) cannot clobber a fresher rotation.
//
// Tie-break hardening for the `expiresAt,omitempty` risk: Anthropic
// leaves expiresAt absent (0) on some envelopes. Equal timestamps —
// including both-zero — are NOT strictly newer, so we return false.
// The one carve-out: if the captured envelope's expiresAt is 0 while
// the access tokens differ, a real rotation happened that simply did
// not populate expiresAt. This is checked before the timestamp ordering
// so the carve-out also fires when the current entry carries a non-zero
// timestamp (an in-session rotation that dropped expiresAt must not lose
// to a stale-but-timestamped vault entry). Dropping that write would
// lose the rotation, so we treat it as fresher. A parse failure of the captured
// side returns a plain error and the launcher retains the prior entry; a
// parse failure of the current side wraps
// [launch.ErrCurrentEnvelopeMalformed] so the launcher overwrites the
// corrupt entry with the valid capture (ADR-0025).
func claudeFresher(captured, current vault.Secret) (bool, error) {
	var capEnv, curEnv claudeCredentialEnvelope
	if err := json.Unmarshal(captured.Value, &capEnv); err != nil {
		return false, fmt.Errorf("%w: parse captured: %v", errClaudeEnvelopeMalformed, err)
	}
	if err := json.Unmarshal(current.Value, &curEnv); err != nil {
		return false, fmt.Errorf("%w: parse current: %v", launch.ErrCurrentEnvelopeMalformed, err)
	}
	capExp := capEnv.ClaudeAiOauth.ExpiresAt
	curExp := curEnv.ClaudeAiOauth.ExpiresAt
	// A captured envelope with no expiresAt (0) but a different access
	// token is a real rotation that simply did not stamp expiresAt;
	// treat it as fresher before the timestamp ordering so it wins even
	// when the current entry carries a non-zero timestamp.
	if capExp == 0 &&
		capEnv.ClaudeAiOauth.AccessToken != curEnv.ClaudeAiOauth.AccessToken {
		return true, nil
	}
	if capExp > curExp {
		return true, nil
	}
	// Equal timestamps (including both-zero with identical tokens) are
	// not strictly newer.
	return false, nil
}

// claudeCaptureValidate is the FileBinding.CaptureValidate hook for the
// subscription (OAuth) binding. The launcher calls it on the host after
// the in-container capture path has read a rotated/first-login
// .credentials.json and validateClaudeEnvelope confirmed its structure,
// but before the vault PUT. It parses the captured envelope and probes
// Claude's profile endpoint with the access token; a non-2xx response or
// a token whose organization carries no recognizable subscription tier
// means the credential cannot authenticate, so it returns an error and
// the launcher skips the PUT (retaining any prior entry).
//
// Honest scope: this cannot pre-validate the *running* session — in the
// default in-container flow the credential does not exist until Claude
// Code logs in mid-session. The durable harm it prevents is a bad or
// expired token captured from a fallback/headless login (or an
// in-session rotation) poisoning the vault for future launches.
//
// The probe runs host-side because the sandbox's deny-by-default network
// policy (ADR-0005) blocks api.anthropic.com from inside the container;
// we deliberately do NOT widen [capabilities.network] to probe in-
// container.
func claudeCaptureValidate(ctx context.Context, client *http.Client, captured vault.Secret) error {
	var env claudeCredentialEnvelope
	if err := json.Unmarshal(captured.Value, &env); err != nil {
		return fmt.Errorf("%w: parse for validation: %v", errClaudeEnvelopeMalformed, err)
	}
	if env.ClaudeAiOauth.AccessToken == "" {
		return fmt.Errorf("%w: claudeAiOauth.accessToken is empty", errClaudeEnvelopeMalformed)
	}
	subscriptionType, _, err := claudeFetchProfile(ctx, client, env.ClaudeAiOauth.AccessToken)
	if err != nil {
		return fmt.Errorf("claude: captured token failed live validation: %w", err)
	}
	if subscriptionType == "" {
		// A 2xx with no recognizable organization_type: the token
		// authenticated but does not back a usable subscription session.
		return errors.New("claude: captured token authenticated but reports no subscription tier (unusable session)")
	}
	return nil
}

// validateClaudeEnvelope checks the JSON parses and carries a
// non-empty `claudeAiOauth.accessToken`. We intentionally tolerate
// extra fields and absent optional fields (Anthropic may add new
// keys on a CLI update; we don't want to break the user's launch
// the day after they `brew upgrade claude`).
func validateClaudeEnvelope(b []byte) error {
	var env claudeCredentialEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("%w: parse: %v", errClaudeEnvelopeMalformed, err)
	}
	if env.ClaudeAiOauth.AccessToken == "" {
		return fmt.Errorf("%w: claudeAiOauth.accessToken is empty",
			errClaudeEnvelopeMalformed)
	}
	return nil
}

// claudeHostAcquire is Claude's host-side credential acquirer, wired to
// the FileBinding's HostAcquire hook (see AuthSpec in claude.go). The
// launcher invokes it only when the vault GET misses, the binding is not
// Required, and host-login is enabled; it returns a vault.Secret that
// the launcher PUTs to agents/claude/oauth and renders before the
// container starts.
//
// Contract (per #1268, enforced by the launcher):
//   - A returned Secret with a non-empty Value seeds the vault.
//   - An empty Secret (with a nil error) or a non-nil error is
//     NON-FATAL: the launcher logs and falls back to the legacy
//     in-container login. So a cancel / unavailable-CLI / failed
//     exchange must surface as one of those, never as a partial seed.
//
// Acquisition mechanism: the hosted-callback PASTE flow (the real
// OAuth, with PKCE). Open the consent URL in the host browser, have the
// user paste the `<code>#<state>` string from the hosted callback page
// on the HOST terminal, verify state, exchange the code for tokens
// (PKCE, no client_secret), fetch the subscription tier from the
// profile endpoint, and build a ms-expiry envelope stamped with that
// tier.
//
// The `claude setup-token` shortcut was deliberately removed (#1304):
// setup-token returns a long-lived API-key-style token with no
// subscription metadata, so Claude Code treated the seeded credential
// as a raw "Claude API" key and reported not-logged-in for Max/Pro
// users (anthropics/claude-code#34262). Subscription launches must
// always run the PKCE flow so the envelope carries a subscriptionType;
// this also drops the implicit dependency on a host `claude` binary.
func claudeHostAcquire(ctx context.Context, deps launch.HostAcquireDeps) (vault.Secret, error) {
	return claudeHostedCallbackAcquire(ctx, deps)
}

// claudeHostedCallbackAcquire runs the PKCE authorization-code flow
// against Claude's hosted callback, reading the pasted code from the
// host terminal via deps.CodePrompter.
func claudeHostedCallbackAcquire(ctx context.Context, deps launch.HostAcquireDeps) (vault.Secret, error) {
	if deps.CodePrompter == nil {
		// No way to read the pasted code. Non-fatal: let the launcher
		// fall back to the in-container login.
		return vault.Secret{}, errors.New("claude: no code prompter available for host paste flow")
	}

	pkce, err := oauth.NewPKCE()
	if err != nil {
		return vault.Secret{}, fmt.Errorf("claude: %w", err)
	}
	state, err := oauth.NewState()
	if err != nil {
		return vault.Secret{}, fmt.Errorf("claude: %w", err)
	}

	authURL := buildClaudeAuthorizeURL(pkce, state)
	// Surface the authorize URL BEFORE opening the browser, so a headless
	// host (or a failed browser open) still gives the user a copyable
	// record of what was launched. Mirrors the Codex device flow.
	if deps.Out != nil {
		fmt.Fprintf(deps.Out,
			"To authorize Claude, open this link\n\n%s\n\nin your browser and paste back the code shown.\n",
			authURL)
	}
	if deps.Browser != nil {
		if err := deps.Browser.Open(authURL); err != nil {
			// Opening the browser failed (headless host, no opener).
			// Non-fatal: fall back to in-container login.
			return vault.Secret{}, fmt.Errorf("claude: open consent URL: %w", err)
		}
	}

	pasted, err := deps.CodePrompter(ctx, nil)
	if err != nil {
		return vault.Secret{}, fmt.Errorf("claude: read pasted code: %w", err)
	}
	code, gotState := splitClaudeCodeState(pasted)
	if code == "" {
		return vault.Secret{}, errors.New("claude: pasted code was empty")
	}
	// The hosted-callback page renders `<code>#<state>`. When the user
	// pasted the state too, verify it matches to bind the code to this
	// session. A paste of the bare code (no `#state`) is tolerated —
	// some users copy only the code — because PKCE already binds the
	// exchange to this client's verifier.
	if gotState != "" && gotState != state {
		return vault.Secret{}, errors.New("claude: pasted state does not match; aborting to avoid a cross-session code")
	}

	access, refresh, expiresIn, scopes, err := claudeExchangeCode(ctx, deps.HTTPClient, code, pkce.Verifier, state)
	if err != nil {
		return vault.Secret{}, err
	}
	// Fetch the subscription tier host-side. A token exchange that
	// succeeds but yields no subscriptionType reproduces #1304: the
	// rendered envelope would look like a raw Claude API key and Claude
	// Code reports not-logged-in. Treat a missing tier as a non-fatal
	// acquire failure (empty Secret / error) per the HostAcquire
	// contract — never seed a tier-less envelope silently.
	subscriptionType, rateLimitTier, err := claudeFetchProfile(ctx, deps.HTTPClient, access)
	if err != nil {
		return vault.Secret{}, err
	}
	if subscriptionType == "" {
		return vault.Secret{}, errors.New("claude: profile fetch returned no subscription tier; cannot seed a recognizable Max/Pro session (re-login)")
	}
	envelope, err := claudeEnvelopeFromTokenResponse(access, refresh, expiresIn, scopes, subscriptionType, rateLimitTier)
	if err != nil {
		return vault.Secret{}, err
	}
	return vault.Secret{
		Value:    envelope,
		Metadata: vault.Metadata{Type: "oauth_refresh_token"},
	}, nil
}

// splitClaudeCodeState splits a pasted `<code>#<state>` string on the
// first `#`. A paste with no `#` yields (code, "").
func splitClaudeCodeState(pasted string) (code, state string) {
	pasted = strings.TrimSpace(pasted)
	if i := strings.Index(pasted, "#"); i >= 0 {
		return strings.TrimSpace(pasted[:i]), strings.TrimSpace(pasted[i+1:])
	}
	return pasted, ""
}

// claudeExchangeCode POSTs the authorization code to Claude's token
// endpoint with PKCE (no client_secret — public client) and returns the
// access token, refresh token, expires_in (seconds), and granted
// scopes.
//
// Per RFC 6749 §5.2 a token-error body may carry token hints; mirroring
// credential.DoRefresh's posture, a non-2xx response surfaces only the
// HTTP status (and the standard `error` code when present), never the
// raw body, so a hostile or verbose provider response cannot leak into
// the user-facing launch error.
func claudeExchangeCode(ctx context.Context, client *http.Client, code, verifier, state string) (access, refresh string, expiresIn int, scopes []string, err error) {
	if client == nil {
		client = http.DefaultClient
	}
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {claudeHostedCallbackURL},
		"client_id":     {claudeOAuthClientID},
		"code_verifier": {verifier},
		"state":         {state},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return "", "", 0, nil, fmt.Errorf("claude: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, nil, fmt.Errorf("claude: token exchange: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		// Redact the raw body; surface only status + standard error code.
		if errCode := parseClaudeTokenErrorCode(respBody); errCode != "" {
			return "", "", 0, nil, fmt.Errorf("claude: token exchange failed: provider returned %d (%s)",
				resp.StatusCode, errCode)
		}
		return "", "", 0, nil, fmt.Errorf("claude: token exchange failed: provider returned %d", resp.StatusCode)
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", "", 0, nil, fmt.Errorf("claude: parse token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", "", 0, nil, errors.New("claude: token response missing access_token")
	}
	granted := strings.Fields(parsed.Scope)
	if len(granted) == 0 {
		// Provider omitted `scope` (RFC 6749 §5.1 permits this when the
		// granted set matches the requested set); record what we asked
		// for.
		granted = append([]string(nil), claudeOAuthScopes...)
	}
	return parsed.AccessToken, parsed.RefreshToken, parsed.ExpiresIn, granted, nil
}

// claudeFetchProfile GETs Claude's OAuth profile endpoint with the
// freshly-exchanged access token and maps the organization's
// `organization_type` to the subscriptionType Claude Code recognizes
// (claude_max -> max, claude_pro -> pro, claude_enterprise ->
// enterprise, claude_team -> team) and surfaces the organization's
// `rate_limit_tier` verbatim as rateLimitTier.
//
// Claude Code derives the tier from this profile endpoint, not from a
// token claim, so this host-side fetch is what lets the seeded envelope
// carry a non-null subscriptionType (#1304). An empty subscriptionType
// return (unknown/absent organization_type) is a non-fatal signal the
// caller turns into an acquire failure rather than seeding a tier-less
// envelope.
//
// Mirroring claudeExchangeCode's posture, a non-2xx response surfaces
// only the HTTP status; the raw body is redacted so a verbose or
// hostile provider response cannot leak into the user-facing launch
// error.
func claudeFetchProfile(ctx context.Context, client *http.Client, accessToken string) (subscriptionType, rateLimitTier string, err error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeProfileURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("claude: build profile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("claude: profile fetch: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		// Redact the raw body; surface only the status.
		return "", "", fmt.Errorf("claude: profile fetch failed: provider returned %d", resp.StatusCode)
	}

	var parsed struct {
		Organization struct {
			OrganizationType string `json:"organization_type"`
			RateLimitTier    string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", "", fmt.Errorf("claude: parse profile response: %w", err)
	}
	return mapClaudeOrganizationType(parsed.Organization.OrganizationType), parsed.Organization.RateLimitTier, nil
}

// mapClaudeOrganizationType maps the profile endpoint's
// `organization.organization_type` to the subscriptionType string
// Claude Code recognizes. An unknown or absent type maps to "" so the
// caller treats it as a missing tier (non-fatal acquire failure) rather
// than stamping an unrecognized value into the envelope.
func mapClaudeOrganizationType(orgType string) string {
	switch orgType {
	case "claude_max":
		return "max"
	case "claude_pro":
		return "pro"
	case "claude_enterprise":
		return "enterprise"
	case "claude_team":
		return "team"
	default:
		return ""
	}
}

// parseClaudeTokenErrorCode extracts the RFC 6749 §5.2 `error` field
// from a token-error body, if present. Returns "" when the body does
// not parse or carries no `error` — the caller then surfaces the bare
// HTTP status. Only the short standardized code is surfaced; the
// free-text `error_description` is intentionally not, since it may echo
// request material.
func parseClaudeTokenErrorCode(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	return e.Error
}
