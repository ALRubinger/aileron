package agents

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ALRubinger/aileron/internal/vault"
)

// piAuthContainerPath is where the in-container Pi CLI reads its
// provider credentials. Pi keeps its credential entries in a dedicated
// `auth.json` under `~/.pi/agent/`, separate from `settings.json`
// (which holds non-credential state). Binding the dedicated auth.json
// — not settings.json — avoids snapshotting non-credential session
// state into the vault. ADR-0025.
const piAuthContainerPath = "/home/agent/.pi/agent/auth.json"

// piVaultPath is the canonical vault namespace for Pi's credential per
// ADR-0025's `agents/<name>/oauth` scheme. The third segment is the
// literal `oauth` the daemon endpoint requires even when the stored
// providers are API keys rather than OAuth bundles — vaultPathConforms
// rejects any other shape.
const piVaultPath = "agents/pi/oauth"

// errPiEnvelopeMalformed is the sentinel returned when the in-container
// auth.json does not parse as the documented envelope. The launcher
// surfaces this to stderr with the file path and recovery options per
// R13.
var errPiEnvelopeMalformed = errors.New("pi: auth envelope is malformed")

// piRender writes the vault's stored bytes into the in-container
// auth.json verbatim. Pi's auth.json is a JSON object keyed by provider
// name, each value an entry like {"type":"api_key","key":"..."} (OAuth
// credentials land here too after `/login`). We validate the envelope
// on the way through so a vault holding non-Pi bytes (operator wrote
// the wrong file) fails the launch with a clear error before the
// container starts, mirroring claudeRender.
func piRender(s vault.Secret) ([]byte, error) {
	if len(s.Value) == 0 {
		return nil, fmt.Errorf("%w: vault entry has empty Value (re-login or `aileron vault put %s` with a real envelope)",
			errPiEnvelopeMalformed, piVaultPath)
	}
	if err := validatePiEnvelope(s.Value); err != nil {
		return nil, err
	}
	// Byte-identity write: Pi reads the file directly; round-trip
	// identity is the load-bearing contract for in-container rotation.
	return s.Value, nil
}

// piCapture validates the post-run file bytes against the envelope
// schema and produces a vault Secret. Bytes are byte-identity over the
// in-container file so any provider or credential field Pi adds
// round-trips cleanly. A partial-write or schema-drift session is
// skipped (error returned) rather than overwriting the vault entry.
func piCapture(b []byte) (vault.Secret, error) {
	if err := validatePiEnvelope(b); err != nil {
		return vault.Secret{}, err
	}
	return vault.Secret{
		Value:    b,
		Metadata: vault.Metadata{Type: "oauth_refresh_token"},
	}, nil
}

// validatePiEnvelope checks the bytes parse as a non-empty JSON object.
// Pi's auth.json maps provider name to a credential entry; an empty
// object carries no usable credential and a non-object is not the
// documented shape. We tolerate any provider keys and any per-provider
// sub-shape so a Pi upgrade that adds providers or fields does not
// break the launch.
func validatePiEnvelope(b []byte) error {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("%w: parse: %v", errPiEnvelopeMalformed, err)
	}
	if len(env) == 0 {
		return fmt.Errorf("%w: no provider entries", errPiEnvelopeMalformed)
	}
	return nil
}
