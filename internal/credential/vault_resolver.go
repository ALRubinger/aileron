package credential

import (
	"context"

	"github.com/ALRubinger/aileron/internal/vault"
)

// VaultResolver is the v1 reference implementation of [Resolver].
// It looks up a single vault entry at a configured path and
// validates that its [vault.Metadata] Type matches the kind the
// connector declared.
//
// Per the v1 binding model (action manifest's `[[bindings]]` block),
// each (connector, capability) pair maps to exactly one vault path;
// VaultResolver carries that mapping for one specific Invoke. The
// executor builds a fresh VaultResolver per step before calling the
// sandbox.
type VaultResolver struct {
	// Vault is the user's local credential vault, already unlocked
	// (the launcher prompts for the passphrase before any agent
	// runs, per ADR-0011).
	Vault vault.Vault

	// VaultPath is the path the bound credential lives at. Format
	// is governed by ADR-0006's binding namespace
	// (`<kind>/<identity>`); this resolver doesn't enforce the
	// shape, only that the entry exists and has the right Type.
	VaultPath string

	// ExpectedKind is the connector's declared
	// `[capabilities.credential].kind`. The fetched secret's
	// Metadata.Type must equal this; otherwise the connector is
	// asking for a kind it isn't bound to and we fail with
	// [ErrCredentialKindMismatch].
	ExpectedKind string
}

// Resolve fetches the credential from the vault and validates kind.
//
//   - Vault returns no entry → [ErrBindingMissing] (wrapped with the
//     path so the audit log can name what was missing).
//   - Vault returns an entry whose Type doesn't match → [ErrCredentialKindMismatch].
//   - Vault returns an unrelated error → wrapped and propagated.
//   - Success → [Credential] with the exact bytes from the vault.
//
// The bytes never leave the host process; callers are responsible
// for using the returned Credential immediately and not retaining a
// reference beyond the request signing path.
func (r *VaultResolver) Resolve(ctx context.Context) (Credential, error) {
	if r == nil || r.Vault == nil {
		return Credential{}, ErrNoBindingResolver
	}
	secret, err := r.Vault.Get(ctx, r.VaultPath)
	if err != nil {
		if vault.IsNotFound(err) {
			return Credential{}, FormatBindingMissing(r.VaultPath)
		}
		return Credential{}, err
	}
	if secret.Metadata.Type != r.ExpectedKind {
		return Credential{}, FormatKindMismatch(r.ExpectedKind, secret.Metadata.Type)
	}
	return Credential{Kind: secret.Metadata.Type, Value: secret.Value}, nil
}
