package composition

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file bridges from a booted image's devcontainer.metadata OCI label value
// to the placeholder environment the whole-plan boot injects. The declared
// tools' Feature customizations are merged into the composed image's
// devcontainer.metadata label (the mechanism internal/cli/unitloader already
// reads for CLI units), so the credential conventions are read from the exact
// signed pin being booted rather than a repo checkout. Each metadata array
// element carries the same customizations.aileron.credential path
// ParseCredentialConvention decodes, so element bytes feed it directly.

// ConventionsFromMetadata parses a devcontainer.metadata label value into zero
// or more credential conventions. The label value is the devcontainer CLI's
// merged metadata array: a JSON array of per-Feature objects. Each element is
// fed to ParseCredentialConvention (which decodes the
// customizations.aileron.credential path itself), so an element carrying a
// present, valid block contributes one convention in array order.
//
// An empty or whitespace-only label is a clean no-op returning (nil, nil),
// matching ImageMetadataLabel's "" sentinel for an unlabeled image. An array
// element with no customizations.aileron.credential block (e.g. an agent
// Feature) contributes nothing. Malformed JSON, or a present-but-invalid
// credential block, is an error: a present-but-broken convention fails loudly
// rather than silently shipping nothing (#1825's fail-closed posture).
func ConventionsFromMetadata(metadata []byte) ([]CredentialConvention, error) {
	if len(strings.TrimSpace(string(metadata))) == 0 {
		return nil, nil
	}

	var elements []json.RawMessage
	if err := json.Unmarshal(metadata, &elements); err != nil {
		return nil, fmt.Errorf("composition: parse devcontainer.metadata array: %w", err)
	}

	var convs []CredentialConvention
	for i := range elements {
		conv, ok, err := ParseCredentialConvention(elements[i])
		if err != nil {
			return nil, fmt.Errorf("composition: credential convention at element %d: %w", i, err)
		}
		if !ok {
			continue
		}
		convs = append(convs, conv)
	}
	return convs, nil
}

// PlaceholderEnv unions the non-secret placeholder pairs across the given
// credential conventions into the env map the whole-plan boot plants into the
// booted container. Each placeholder's Env becomes a key and its Value the
// value.
//
// The union is fail-closed on conflict: two placeholders sharing an env with
// the SAME value dedupe to one entry, but two sharing an env with DIFFERENT
// values are a loud error naming the env, never a silent last-wins. Empty input
// (no conventions, or conventions with no placeholders) returns (nil, nil).
func PlaceholderEnv(convs []CredentialConvention) (map[string]string, error) {
	var env map[string]string
	for _, conv := range convs {
		for _, p := range conv.Placeholders {
			if existing, seen := env[p.Env]; seen {
				if existing != p.Value {
					return nil, fmt.Errorf("composition: conflicting placeholder values for env %q: %q vs %q", p.Env, existing, p.Value)
				}
				continue
			}
			if env == nil {
				env = map[string]string{}
			}
			env[p.Env] = p.Value
		}
	}
	return env, nil
}
