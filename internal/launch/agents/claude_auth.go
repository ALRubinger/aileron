package agents

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ALRubinger/aileron/internal/vault"
)

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
// credentials per ADR-0025's `agents/<name>/<purpose>` scheme.
const claudeVaultPath = "agents/claude/oauth"

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
}

// errClaudeEnvelopeMalformed is returned by claudeCapture when the
// in-container file does not parse as the documented envelope. The
// launcher's CaptureFn surfaces this to stderr with the file path
// and the user's recovery options (inspect, re-login, etc.) per R13.
var errClaudeEnvelopeMalformed = errors.New("claude: credentials envelope is malformed")

// claudeOnboardingStub is the constant payload written to
// /home/agent/.claude.json on every launch. {"hasCompletedOnboarding":
// true, "installMethod": "native"} short-circuits Claude's first-run
// wizard so the user does not pay the theme-picker tax on every
// sandbox launch. Kept as a package-level constant so the schema is
// visible in one place when Anthropic adds new onboarding fields.
var claudeOnboardingStub = []byte(`{"hasCompletedOnboarding":true,"installMethod":"native"}`)

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
// not populate expiresAt. Dropping that write would lose the
// rotation, so we treat it as fresher. A parse failure on either side
// returns an error; the launcher then retains the prior entry rather
// than risk clobbering it with an envelope it cannot reason about.
func claudeFresher(captured, current vault.Secret) (bool, error) {
	var capEnv, curEnv claudeCredentialEnvelope
	if err := json.Unmarshal(captured.Value, &capEnv); err != nil {
		return false, fmt.Errorf("%w: parse captured: %v", errClaudeEnvelopeMalformed, err)
	}
	if err := json.Unmarshal(current.Value, &curEnv); err != nil {
		return false, fmt.Errorf("%w: parse current: %v", errClaudeEnvelopeMalformed, err)
	}
	capExp := capEnv.ClaudeAiOauth.ExpiresAt
	curExp := curEnv.ClaudeAiOauth.ExpiresAt
	if capExp > curExp {
		return true, nil
	}
	if capExp == curExp {
		// Both-zero (or genuinely equal) timestamps are not strictly
		// newer — except a real rotation that left expiresAt unset.
		if capExp == 0 &&
			capEnv.ClaudeAiOauth.AccessToken != curEnv.ClaudeAiOauth.AccessToken {
			return true, nil
		}
		return false, nil
	}
	return false, nil
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
