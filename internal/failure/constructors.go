package failure

// Per-class constructors. Each one hard-codes the class, default
// boundary, and default retriable value per ADR-0010's taxonomy.
// Options override boundary or attach details/cause.

// CapabilityDeniedAt builds a CapabilityDenied failure attributed to
// the given boundary (sandbox, connector_manifest, or action). The
// caller must pass one of those three boundaries — Runtime/External
// don't make sense for capability denial.
func CapabilityDeniedAt(b Boundary, message string, opts ...Option) *Failure {
	f := &Failure{
		class:     CapabilityDenied,
		boundary:  b,
		message:   message,
		retriable: false,
	}
	applyOpts(f, opts)
	return f
}

// BindingRequiredFailure builds a BindingRequired failure. The user
// needs to bind a credential and retry; not retriable until they do.
func BindingRequiredFailure(message string, opts ...Option) *Failure {
	f := &Failure{
		class:     BindingRequired,
		boundary:  Runtime,
		message:   message,
		retriable: false,
	}
	applyOpts(f, opts)
	return f
}

// BindingFailedFailure builds a BindingFailed failure. The retriable
// flag is caller-determined — token refresh might succeed, while a
// revoked key won't.
func BindingFailedFailure(message string, retriable bool, opts ...Option) *Failure {
	f := &Failure{
		class:     BindingFailed,
		boundary:  Runtime,
		message:   message,
		retriable: retriable,
	}
	applyOpts(f, opts)
	return f
}

// ResourceLimitFailure builds a ResourceLimitExceeded failure. The
// connector instance hit a memory/wall-time/fuel limit; not retriable
// because the same call would hit the same limit.
func ResourceLimitFailure(message string, opts ...Option) *Failure {
	f := &Failure{
		class:     ResourceLimitExceeded,
		boundary:  Sandbox,
		message:   message,
		retriable: false,
	}
	applyOpts(f, opts)
	return f
}

// ConnectorRuntime builds a ConnectorRuntimeError failure. Retriable
// is caller-determined — a panic might be transient, a contract
// violation is not.
func ConnectorRuntime(message string, retriable bool, opts ...Option) *Failure {
	f := &Failure{
		class:     ConnectorRuntimeError,
		boundary:  Sandbox,
		message:   message,
		retriable: retriable,
	}
	applyOpts(f, opts)
	return f
}

// Network builds a NetworkError failure for outbound network calls
// that failed before reaching a server. Always retriable.
func Network(message string, opts ...Option) *Failure {
	f := &Failure{
		class:     NetworkError,
		boundary:  External,
		message:   message,
		retriable: true,
	}
	applyOpts(f, opts)
	return f
}

// Upstream builds an ExternalAPIError failure for upstream HTTP
// responses. Status is recorded in details.status. Retriable when
// status is 429 or 5xx; non-retriable otherwise.
func Upstream(status int, message string, opts ...Option) *Failure {
	f := &Failure{
		class:     ExternalAPIError,
		boundary:  External,
		message:   message,
		retriable: isHTTPRetriable(status),
		details:   map[string]any{"status": status},
	}
	applyOpts(f, opts)
	return f
}

// HashMismatchFailure builds a HashMismatch failure. Bytes on disk
// don't match the declared hash; never retriable.
func HashMismatchFailure(message string, opts ...Option) *Failure {
	f := &Failure{
		class:     HashMismatch,
		boundary:  Runtime,
		message:   message,
		retriable: false,
	}
	applyOpts(f, opts)
	return f
}

// SignatureFailureFailure builds a SignatureFailure failure. A
// binary's signature didn't verify; never retriable.
func SignatureFailureFailure(message string, opts ...Option) *Failure {
	f := &Failure{
		class:     SignatureFailure,
		boundary:  Runtime,
		message:   message,
		retriable: false,
	}
	applyOpts(f, opts)
	return f
}

// isHTTPRetriable reports whether an HTTP status code from an
// upstream service should be retried per ADR-0010's policy: 429 and
// any 5xx are retriable; everything else is not.
func isHTTPRetriable(status int) bool {
	if status == 429 {
		return true
	}
	return status >= 500 && status <= 599
}
