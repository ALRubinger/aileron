package sandbox

import "fmt"

// FailureClass is the closed-set classification of sandbox failures. The
// taxonomy is the subset of [ADR-0010]'s closed set produced at the
// sandbox boundary; new classes require an ADR amendment.
//
// [ADR-0010]: https://docs.withaileron.ai/adr/0010-failure-handling
type FailureClass string

const (
	// ClassCapabilityDenied is the canonical class for any operation
	// the connector attempted that exceeds its declared grants. The
	// `Boundary` field disambiguates whether the WASM sandbox itself,
	// the connector manifest, or the action manifest produced the
	// denial — see ADR-0010 §"capable_denied" for the full taxonomy.
	ClassCapabilityDenied FailureClass = "capability_denied"

	// ClassResourceLimitExceeded is the canonical class for a sandbox
	// instance that hit a memory, wall-time, or fuel limit. The
	// `Limit` detail names which one.
	ClassResourceLimitExceeded FailureClass = "resource_limit_exceeded"

	// ClassConnectorRuntimeError is the canonical class for a connector
	// that failed inside its own code path (panicked, returned a
	// non-zero status, produced a malformed output payload).
	ClassConnectorRuntimeError FailureClass = "connector_runtime_error"

	// ClassConnectorLoadFailed is a hard fail at compile or
	// instantiation time: the WASM module is malformed, refuses to
	// instantiate, or imports a host function the runtime did not
	// register. Per ADR-0005 §"WASI host imports are gated by the
	// connector's `[capabilities.runtime]` declaration", out-of-grant
	// imports refuse at instantiation.
	ClassConnectorLoadFailed FailureClass = "connector_load_failed"
)

// Boundary identifies the layer that produced a failure (per ADR-0010).
// The sandbox always emits "sandbox" — the kernel/WASM-engine boundary —
// regardless of which class is raised. Action-boundary and
// connector-manifest-boundary denials live in their respective packages
// (`internal/action` and `internal/cstore`) and emit their own values.
type Boundary string

const (
	// BoundarySandbox names the WASM sandbox layer.
	BoundarySandbox Boundary = "sandbox"
)

// LimitKind names which resource limit was exceeded. Used in the
// `Details["limit"]` field of a [Error] when [Error.Class] is
// [ClassResourceLimitExceeded] (per ADR-0010's "specific limit named"
// requirement).
type LimitKind string

const (
	LimitMemory   LimitKind = "memory"
	LimitWallTime LimitKind = "wall_time"
)

// Error is a structured sandbox-layer error per ADR-0010. The `Details`
// map carries class-specific structured context (e.g. denied host:port
// for capability_denied; named limit for resource_limit_exceeded).
type Error struct {
	Class     FailureClass
	Message   string
	Boundary  Boundary
	Retriable bool
	Details   map[string]any
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s (boundary=%s)", e.Class, e.Message, e.Boundary)
}

// newCapabilityDenied builds a capability_denied error per ADR-0005's
// example envelope (`boundary: "sandbox"`).
func newCapabilityDenied(message string, details map[string]any) *Error {
	return &Error{
		Class:    ClassCapabilityDenied,
		Message:  message,
		Boundary: BoundarySandbox,
		Details:  details,
	}
}

// newResourceLimit builds a resource_limit_exceeded error naming the
// specific limit (per ADR-0010 §"Resource-limit terminations produce
// `class: resource_limit_exceeded` errors with the specific limit
// named").
func newResourceLimit(limit LimitKind, configured string) *Error {
	return &Error{
		Class:    ClassResourceLimitExceeded,
		Message:  fmt.Sprintf("%s limit exceeded (configured: %s)", limit, configured),
		Boundary: BoundarySandbox,
		Details: map[string]any{
			"limit":      string(limit),
			"configured": configured,
		},
	}
}

// newConnectorRuntimeError builds a connector_runtime_error.
func newConnectorRuntimeError(message string) *Error {
	return &Error{
		Class:    ClassConnectorRuntimeError,
		Message:  message,
		Boundary: BoundarySandbox,
	}
}

// newConnectorLoadFailed builds a connector_load_failed.
func newConnectorLoadFailed(message string) *Error {
	return &Error{
		Class:    ClassConnectorLoadFailed,
		Message:  message,
		Boundary: BoundarySandbox,
	}
}
