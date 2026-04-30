// Package failure implements the structured failure envelope ratified
// by [ADR-0010]: a closed taxonomy of failure classes, a closed set of
// boundaries, and a single [Failure] type that carries enough context
// for the agent (and the audit log) to reason about what went wrong.
//
// The envelope schema is:
//
//	{
//	  "error": {
//	    "class":     "<failure-class>",
//	    "message":   "<user-facing description>",
//	    "retriable": true|false,
//	    "boundary":  "<which-layer>",
//	    "audit_id":  "audit-<id>",
//	    "details":   { ...class-specific... }
//	  }
//	}
//
// The closed taxonomy is enforced through the type system: Class and
// Boundary are unexported on [Failure] and only the package's
// constructors can produce a valid value. Code outside this package
// cannot synthesize a Failure with an arbitrary class string.
//
// [ADR-0010]: https://docs.withaileron.ai/adr/0010-failure-handling
package failure

import "fmt"

// FailureClass is the closed taxonomy of failure classes. Adding a
// class requires an ADR amendment per ADR-0010.
type FailureClass string

const (
	// CapabilityDenied — a capability check refused the operation.
	// Defense-in-depth check at sandbox, connector_manifest, or
	// action boundaries.
	CapabilityDenied FailureClass = "capability_denied"

	// BindingRequired — no credential is bound for a required
	// capability. The user must bind one and retry.
	BindingRequired FailureClass = "binding_required"

	// BindingFailed — an existing binding refused: token expired,
	// key revoked, scope changed.
	BindingFailed FailureClass = "binding_failed"

	// ResourceLimitExceeded — the connector instance hit a resource
	// limit (memory, wall time, fuel).
	ResourceLimitExceeded FailureClass = "resource_limit_exceeded"

	// ConnectorRuntimeError — the connector code itself failed:
	// panicked, returned an unexpected shape, threw an exception.
	ConnectorRuntimeError FailureClass = "connector_runtime_error"

	// NetworkError — outbound network call failed before reaching
	// a server (DNS, connection refused, TLS error).
	NetworkError FailureClass = "network_error"

	// ExternalAPIError — the upstream service returned an error
	// response. Retriable for 5xx and 429; not retriable for most
	// 4xx. Status code lives in details.status.
	ExternalAPIError FailureClass = "external_api_error"

	// HashMismatch — a connector or action's on-disk bytes don't
	// match the declared hash.
	HashMismatch FailureClass = "hash_mismatch"

	// SignatureFailure — a binary's signature doesn't verify.
	SignatureFailure FailureClass = "signature_failure"
)

// Boundary identifies which layer produced an error.
type Boundary string

const (
	// Sandbox is the connector sandbox (WASM runtime, capability
	// gates, resource limits).
	Sandbox Boundary = "sandbox"

	// ConnectorManifest is the connector's manifest-level capability
	// check: hosts, scope, runtime imports.
	ConnectorManifest Boundary = "connector_manifest"

	// Action is the action's declared capability subset check —
	// even when the connector permits the operation, the action's
	// declared subset must include it.
	Action Boundary = "action"

	// Runtime is the runtime layer outside any connector: hash
	// verification, signature verification, manifest parse,
	// orchestration.
	Runtime Boundary = "runtime"

	// External is the upstream service the connector talked to.
	External Boundary = "external"
)

// AllClasses returns every valid [FailureClass] in declaration order.
// Used by tests and for emitting documentation that enumerates the
// taxonomy.
func AllClasses() []FailureClass {
	return []FailureClass{
		CapabilityDenied,
		BindingRequired,
		BindingFailed,
		ResourceLimitExceeded,
		ConnectorRuntimeError,
		NetworkError,
		ExternalAPIError,
		HashMismatch,
		SignatureFailure,
	}
}

// AllBoundaries returns every valid [Boundary] in declaration order.
func AllBoundaries() []Boundary {
	return []Boundary{Sandbox, ConnectorManifest, Action, Runtime, External}
}

// Failure is the structured error returned across boundaries that
// follow ADR-0010's envelope contract.
//
// Class and boundary are unexported so the only path to a valid
// Failure is through this package's constructors (New, Network,
// External, CapabilityDenied, etc.). This is what makes the closed
// taxonomy actually closed — handlers cannot synthesize an arbitrary
// class string.
type Failure struct {
	class     FailureClass
	boundary  Boundary
	message   string
	retriable bool
	auditID   string
	details   map[string]any
	cause     error
}

// Class returns the canonical failure class.
func (f *Failure) Class() FailureClass { return f.class }

// Boundary returns the layer that produced the failure.
func (f *Failure) Boundary() Boundary { return f.boundary }

// Message is the user-facing prose. Safe to show to end users.
func (f *Failure) Message() string { return f.message }

// Retriable reports whether the failure is safe to retry.
func (f *Failure) Retriable() bool { return f.retriable }

// AuditID returns the audit-log entry identifier that records this
// failure. Empty until Recorder.RecordFailure stamps it.
func (f *Failure) AuditID() string { return f.auditID }

// SetAuditID stamps the audit-log entry identifier onto the failure.
// Called by the audit recorder right after appending the event.
func (f *Failure) SetAuditID(id string) { f.auditID = id }

// Details returns class-specific additional context. The map is
// owned by the Failure; callers must not mutate it.
func (f *Failure) Details() map[string]any { return f.details }

// SetDetail attaches or overwrites a single detail key. Used by the
// retry primitive to add `retried: N` after exhausting retries.
func (f *Failure) SetDetail(key string, value any) {
	if f.details == nil {
		f.details = map[string]any{}
	}
	f.details[key] = value
}

// Error implements the error interface.
func (f *Failure) Error() string {
	if f.cause != nil {
		return fmt.Sprintf("%s: %s: %s", f.class, f.message, f.cause.Error())
	}
	return fmt.Sprintf("%s: %s", f.class, f.message)
}

// Unwrap returns the wrapped cause, if any.
func (f *Failure) Unwrap() error { return f.cause }

// Option is a constructor option that customises a Failure beyond
// what the per-class constructor sets by default.
type Option func(*Failure)

// WithDetails attaches additional detail fields to the Failure.
// Provided pairs replace any existing keys with the same name.
func WithDetails(d map[string]any) Option {
	return func(f *Failure) {
		if f.details == nil {
			f.details = make(map[string]any, len(d))
		}
		for k, v := range d {
			f.details[k] = v
		}
	}
}

// WithCause wraps an underlying Go error so callers can use
// errors.Is / errors.As across the boundary.
func WithCause(err error) Option { return func(f *Failure) { f.cause = err } }

// WithBoundary overrides the default boundary for the class. Used
// for `capability_denied` which can fire at sandbox, connector_manifest,
// or action layers depending on which check tripped.
func WithBoundary(b Boundary) Option { return func(f *Failure) { f.boundary = b } }

// applyOpts mutates f with each option in order.
func applyOpts(f *Failure, opts []Option) {
	for _, opt := range opts {
		opt(f)
	}
}
