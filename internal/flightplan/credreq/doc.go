// Package credreq derives a frozen Flight Plan's credential-binding
// requirements from its tool-step trust contracts. It answers one question,
// purely and deterministically: given a decoded plan, what set of credentials
// must an operator supply before the plan can run?
//
// The output is a deduplicated, deterministically ordered set of
// [RequiredBinding] values. Each names a credential kind, an injection scheme,
// the vault path (`credential_ref`) the operator's secret must live at, the
// declared upstream hosts, and whether the binding is selected by host or by
// credential identity. It is the machine-readable "what the operator must
// supply" that the `aileron skill bind` verb (#1998) consumes; nothing in this
// package writes a descriptor, reads a vault, or prompts.
//
// # Purity
//
// [Derive] is a pure function of a *runtime.Plan: no I/O, no clock, no
// randomness. Given the same plan it returns the same set, so it is the fully
// tested contract surface. [DeriveFromFrozen] is the only I/O path: a thin
// wrapper that reads and signature-verifies a frozen version through
// runtime.LoadVerified, then delegates to Derive. It adds no derivation logic.
//
// # Scope: tool-step trust contracts only
//
// Derive iterates plan.Steps and considers a step only when it is a
// runtime.KindTool step carrying a non-nil TrustContract. Action-level trust
// contracts (plan.Actions) are OUT OF SCOPE for this unit: the action boundary
// mediates credentials through the connector-spec binding layer, a different
// mechanism from the host-side egress injection a tool step's reach describes.
// A step whose contract declares credential kind `none` is unauthenticated and
// produces no requirement.
//
// # Scheme mapping (keyed on credential.kind, fail closed)
//
// runtime.TrustContract carries no placement (the DTO reads it but toContract
// drops it), so the scheme and host-shape are mapped from credential.kind
// alone, through the single closed table in scheme.go:
//
//	credential.kind | scheme (inject.*) | host-shape          | note
//	----------------+-------------------+---------------------+----------------------------------
//	aws-sigv4       | sigv4-resign      | host-less-identity  | selected by (kind, identityLabel)
//	oauth2          | bearer            | host-keyed          | schema pins placement=header
//	api-key         | bearer            | host-keyed          | documented header-bearer default
//	none            | (skip)            | (skip)              | unauthenticated, no credential
//	anything else   | ERROR             | ERROR               | fail closed, no guessed scheme
//
// A kind outside this set is a hard error: the derivation never guesses a
// scheme for an unknown kind. Placement-aware selection (a query-param api-key,
// a header-template vendor header) is deliberately unsupported here; adding it
// requires plumbing placement onto runtime.TrustContract first, a separate
// future change.
//
// # credential_ref rule (stable, do not change without a migration)
//
// The derived vault path targets the user-level namespace `user/<service>`,
// matching #1995's decision that plan-onboarded secrets live at `user/<name>`
// and that a second operator supplies their own secret at the same ref. The
// service segment is slug(identityLabel) when the contract declares a label,
// else slug(credentialKind). The slug lowercases, collapses each run of
// characters outside [a-z0-9] to a single '-', and trims leading/trailing '-',
// so the result always satisfies the user-ref grammar
// (`user/[a-z0-9][a-z0-9_-]*`, no dots). This is the exact, stable rule; a
// change to it would strand every already-bound secret and must ship with a
// migration.
//
// This package never reads or carries a credential value. It reads only the
// non-secret trust-contract declaration (kind, identity label, hosts) and emits
// non-secret binding requirements.
package credreq
