// Package credential implements the host-side credential mediation
// layer ratified by [ADR-0005] and [ADR-0011].
//
// The architectural promise is that connectors never see raw
// credential bytes. The runtime resolves a binding (a vault entry
// pointed at by an action's [[bindings]] block) at outbound-request
// time, injects the credential into the actual request headers, and
// hands the connector only an opaque "capability handle" — for v1,
// the kind string the connector declares in its manifest's
// `[capabilities.credential]`.
//
// This package owns:
//
//   - The [Resolver] interface — what the sandbox host functions call
//     when a connector references a credential in an http_request
//     envelope.
//   - The [Credential] value the resolver returns (host-side only).
//   - Sentinel errors so callers can distinguish "no binding" from
//     "wrong kind" and surface them as the right ADR-0010 failure
//     class.
//   - The [VaultResolver] reference implementation that looks up a
//     vault.Vault entry at a given path and validates its metadata
//     Type matches the kind the connector declared.
//
// [ADR-0005]: https://docs.withaileron.ai/adr/0005-sandbox-choice
// [ADR-0011]: https://docs.withaileron.ai/adr/0011-local-credential-vault
package credential

import (
	"context"
	"errors"
	"fmt"
)

// Credential is the resolved binding the runtime injects into an
// outbound request. It is host-side only — never crossed into the
// sandbox guest's memory.
type Credential struct {
	// Kind is the credential's type ("oauth2" or "api_key" in v1).
	// Matched against the connector's manifest declaration before
	// the bytes are ever touched, so a mismatched binding fails
	// fast without leaking which kind was actually stored.
	Kind string

	// Value is the raw credential bytes (the bearer token). Only
	// crosses host-side code paths: the request signer reads it,
	// then it leaves scope. Never flows into the sandbox or into
	// any audit record.
	Value []byte
}

// Resolver is the contract between the sandbox host functions and
// the credential mediation layer. Each call to [Resolver.Resolve]
// returns the credential bound for "the capability the connector
// declared, in the context of the action this Invoke is part of".
// Resolvers are constructed per Invoke by the executor; the sandbox
// host call site calls Resolve at most once per outbound request.
type Resolver interface {
	// Resolve looks up the binding and returns the credential. The
	// returned Credential's Kind has been validated against the
	// caller's declared kind already; resolvers that detect a
	// mismatch return ErrCredentialKindMismatch instead of returning
	// a Credential with the wrong Kind.
	Resolve(ctx context.Context) (Credential, error)
}

// Sentinel errors. Resolvers wrap these via fmt.Errorf("...: %w", err)
// so callers can pattern-match with errors.Is.
var (
	// ErrBindingMissing means the action declared a binding but the
	// vault has no entry at the configured path. The right surface
	// is a `binding_required` failure to the LLM (per ADR-0010).
	ErrBindingMissing = errors.New("credential: no binding for this capability")

	// ErrCredentialKindMismatch means the vault entry exists but its
	// metadata Type doesn't match the kind the connector declared.
	// Surfaces as `capability_denied` — the connector asked for a
	// kind it isn't bound to.
	ErrCredentialKindMismatch = errors.New("credential: bound kind does not match capability")

	// ErrNoBindingResolver means the host-side request was attempted
	// without a Resolver wired up at all (e.g. a Call constructed
	// with a nil CredentialResolver). The runtime treats this as a
	// programming error, surfaced as a binding_required failure so
	// the user sees a recoverable message.
	ErrNoBindingResolver = errors.New("credential: no resolver is wired for this call")
)

// FormatBindingMissing returns a user-facing error wrapping
// [ErrBindingMissing] with the vault path the resolver was looking
// for. Helpful for debugging and audit context.
func FormatBindingMissing(vaultPath string) error {
	return fmt.Errorf("%w: vault entry %q not found", ErrBindingMissing, vaultPath)
}

// FormatKindMismatch returns a user-facing error wrapping
// [ErrCredentialKindMismatch].
func FormatKindMismatch(want, got string) error {
	return fmt.Errorf("%w: declared %q but binding stores %q", ErrCredentialKindMismatch, want, got)
}
