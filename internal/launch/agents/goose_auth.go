package agents

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ALRubinger/aileron/internal/vault"
)

// gooseVaultPath is the canonical vault namespace for Goose's
// credential per ADR-0025's `agents/<name>/oauth` scheme. The third
// segment is the literal `oauth` the daemon endpoint requires even
// though Goose authenticates with a provider API key, not OAuth —
// vaultPathConforms rejects any other shape (internal/launch/authspec.go).
const gooseVaultPath = "agents/goose/oauth"

// gooseAPIKeyEnv is the environment variable the in-container Goose
// reads its LLM-provider credential from. Goose resolves provider
// keys from `<PROVIDER>_API_KEY` env vars (or its keyring) rather
// than a standalone on-disk credential file, so the natural vault
// binding is an EnvBinding: a static API key rendered into the env.
//
// We bind ANTHROPIC_API_KEY because Goose's default provider in the
// Aileron launch path is Anthropic. An operator running Goose against
// a different provider seeds the matching key under the same vault
// path and the in-container Goose still picks it up — the env var name
// is the only provider-specific coupling, and it is documented here so
// a future multi-provider binding has one place to change.
const gooseAPIKeyEnv = "ANTHROPIC_API_KEY"

// errGooseSecretEmpty is returned by gooseRender when the resolved
// vault entry has no usable key. The launcher surfaces this to stderr
// with the recovery hint per R13.
var errGooseSecretEmpty = errors.New("goose: vault entry has empty API key")

// gooseRender maps the vault secret to Goose's provider-API-key env
// var. Goose's credential is a single opaque API key, so the vault
// Value is the raw key bytes (no JSON envelope). We trim surrounding
// whitespace (a key copied from another machine can carry a trailing
// newline) and reject an empty value with a recovery hint, mirroring
// claudeRender's empty-Value guard.
func gooseRender(s vault.Secret) (map[string]string, error) {
	key := strings.TrimSpace(string(s.Value))
	if key == "" {
		return nil, fmt.Errorf("%w (re-login or `aileron vault put %s` with the provider API key)",
			errGooseSecretEmpty, gooseVaultPath)
	}
	return map[string]string{gooseAPIKeyEnv: key}, nil
}
