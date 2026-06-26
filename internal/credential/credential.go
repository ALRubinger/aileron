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

	// Region and AccessKeyID are the non-secret AWS Signature Version 4
	// signing inputs a resolver may carry alongside the secret access key
	// (Value) for an `aws_sigv4` binding. Both are empty for every other
	// kind. They are safe to log. When non-empty they win over the
	// connector manifest's region / access_key_id at signing time, which
	// is how a single connector install supports several region-scoped
	// bindings.
	Region      string
	AccessKeyID string
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

// RegionalResolver is an optional extension of [Resolver] for credential
// kinds whose binding selection depends on the outbound request's target
// region. The aws_sigv4 path implements it: a single connector install
// may hold several region-scoped bindings, and the host parses the AWS
// region from the outbound host at request time and calls
// [RegionalResolver.ResolveForRegion] so the correct region's binding
// (and its access key id) is chosen.
//
// Callers type-assert a [Resolver] to RegionalResolver; resolvers that do
// not implement it are resolved through the plain [Resolver.Resolve].
type RegionalResolver interface {
	Resolver

	// ResolveForRegion resolves the binding whose region matches the
	// supplied region. An empty region means the caller could not derive
	// a region from the request; the resolver falls back to the sole
	// binding when exactly one exists. When several bindings match and no
	// region disambiguates them the resolver returns an error rather than
	// guessing, per ADR-0006's no-silent-matching rule.
	ResolveForRegion(ctx context.Context, region string) (Credential, error)
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
