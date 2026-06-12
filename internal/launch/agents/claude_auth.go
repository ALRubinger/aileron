package agents

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ALRubinger/aileron/internal/launch"
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

// claudeWorkspacePath is the container directory the launcher runs
// Claude in (the bind-mounted project workspace). It must match
// container.WorkspacePath and the runtime `--workdir`; it is hardcoded
// here to keep the agents package free of an import on the container
// package, mirroring how the credential/onboarding paths above are
// spelled out literally.
const claudeWorkspacePath = "/home/agent/workspace"

// claudeOnboardingStub is the payload written to /home/agent/.claude.json
// on every launch. It short-circuits three first-run interruptions so a
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
//
// Built from a typed value rather than a string literal so the nested
// shape stays valid as Anthropic adds onboarding fields.
var claudeOnboardingStub = mustMarshalOnboardingStub()

func mustMarshalOnboardingStub() []byte {
	type projectTrust struct {
		HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
	}
	stub := struct {
		HasCompletedOnboarding bool                    `json:"hasCompletedOnboarding"`
		InstallMethod          string                  `json:"installMethod"`
		Projects               map[string]projectTrust `json:"projects"`
	}{
		HasCompletedOnboarding: true,
		InstallMethod:          "global",
		Projects: map[string]projectTrust{
			claudeWorkspacePath: {HasTrustDialogAccepted: true},
		},
	}
	b, err := json.Marshal(stub)
	if err != nil {
		// The shape is a fixed literal; marshaling it cannot fail at
		// runtime. Panic so a refactor that breaks it surfaces in tests
		// rather than silently shipping an empty onboarding file.
		panic("claude: marshaling onboarding stub: " + err.Error())
	}
	return b
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
