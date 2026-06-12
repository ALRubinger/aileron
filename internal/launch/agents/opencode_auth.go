package agents

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ALRubinger/aileron/internal/vault"
)

// opencodeAuthContainerPath is where the in-container OpenCode CLI
// reads its provider credentials. OpenCode writes this file via
// `opencode auth login` and loads it on startup; it lives under the
// XDG data dir, not the config dir. ADR-0025.
const opencodeAuthContainerPath = "/home/agent/.local/share/opencode/auth.json"

// opencodeVaultPath is the canonical vault namespace for OpenCode's
// credential per ADR-0025's `agents/<name>/oauth` scheme. The third
// segment is the literal `oauth` the daemon endpoint requires even
// when the stored providers are API keys rather than OAuth tokens —
// vaultPathConforms rejects any other shape.
const opencodeVaultPath = "agents/opencode/oauth"

// errOpencodeEnvelopeMalformed is the sentinel returned when the
// in-container auth.json does not parse as the documented envelope.
// The launcher surfaces this to stderr with the file path and the
// recovery options per R13.
var errOpencodeEnvelopeMalformed = errors.New("opencode: auth envelope is malformed")

// opencodeRender writes the vault's stored bytes into the in-container
// auth.json verbatim. OpenCode's auth.json is a JSON object keyed by
// provider name, each value carrying that provider's credential (an
// API key or an OAuth bundle). We validate the envelope on the way
// through so a vault holding non-OpenCode bytes (operator wrote the
// wrong file) fails the launch with a clear error before the container
// starts, mirroring claudeRender.
func opencodeRender(s vault.Secret) ([]byte, error) {
	if len(s.Value) == 0 {
		return nil, fmt.Errorf("%w: vault entry has empty Value (re-login or `aileron vault put %s` with a real envelope)",
			errOpencodeEnvelopeMalformed, opencodeVaultPath)
	}
	if err := validateOpencodeEnvelope(s.Value); err != nil {
		return nil, err
	}
	// Byte-identity write: OpenCode reads the file directly and the
	// round-trip property is the load-bearing contract for in-container
	// rotation. Returning the bytes untouched makes that obvious.
	return s.Value, nil
}

// opencodeCapture validates the post-run file bytes against the
// envelope schema and produces a vault Secret. Bytes are byte-identity
// over the in-container file so any provider OpenCode adds round-trips
// cleanly. A partial-write or schema-drift session is skipped (error
// returned) rather than overwriting the vault entry.
func opencodeCapture(b []byte) (vault.Secret, error) {
	if err := validateOpencodeEnvelope(b); err != nil {
		return vault.Secret{}, err
	}
	return vault.Secret{
		Value:    b,
		Metadata: vault.Metadata{Type: "oauth_refresh_token"},
	}, nil
}

// validateOpencodeEnvelope checks the bytes parse as a non-empty JSON
// object. OpenCode's auth.json maps provider name to that provider's
// credential entry; an empty object carries no usable credential and a
// non-object (array, scalar) is not the documented shape. We tolerate
// any provider keys and any per-provider sub-shape so a `brew upgrade
// opencode` that adds a new provider or credential field does not break
// the launch.
func validateOpencodeEnvelope(b []byte) error {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("%w: parse: %v", errOpencodeEnvelopeMalformed, err)
	}
	if len(env) == 0 {
		return fmt.Errorf("%w: no provider entries", errOpencodeEnvelopeMalformed)
	}
	return nil
}
