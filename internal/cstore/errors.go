package cstore

import "fmt"

// FailureClass is the closed-set classification of cstore failures. Aligns
// with [ADR-0010]'s structured-error envelope; classes specific to the
// install pipeline (`hash_mismatch`, `signature_failure`) are explicit
// members of the ADR-0010 taxonomy.
//
// [ADR-0010]: https://docs.withaileron.ai/adr/0010-failure-handling
type FailureClass string

const (
	// ClassParseError is a manifest TOML parse failure.
	ClassParseError FailureClass = "parse_error"

	// ClassValidationError is a manifest schema validation failure.
	ClassValidationError FailureClass = "validation_error"

	// ClassUnknownScheme is a hard fail at install when a connector FQN
	// uses a scheme the resolver does not know about.
	ClassUnknownScheme FailureClass = "unknown_scheme"

	// ClassFetchFailed is a non-zero outcome from the source's HTTP API
	// (404 tag-not-found, 5xx, network unreachable).
	ClassFetchFailed FailureClass = "fetch_failed"

	// ClassMalformedTarball is an extraction failure: missing required
	// file, multiple binaries, unreadable archive.
	ClassMalformedTarball FailureClass = "malformed_tarball"

	// ClassSignatureFailure is the canonical class from ADR-0010 for an
	// install whose signature does not verify.
	ClassSignatureFailure FailureClass = "signature_failure"

	// ClassHashMismatch is the canonical class from ADR-0010 for a
	// computed hash that does not match the action's declared hash. Per
	// ADR-0004 nothing is written to the store on this failure.
	ClassHashMismatch FailureClass = "hash_mismatch"

	// ClassFQNMismatch is an install where the manifest's declared FQN
	// does not match the FQN the binary was fetched under.
	ClassFQNMismatch FailureClass = "fqn_mismatch"

	// ClassStoreUnwritable is a permissions / disk failure interacting
	// with the on-disk content-addressed store.
	ClassStoreUnwritable FailureClass = "store_unwritable"
)

// Boundary identifies which layer produced a failure (per ADR-0010).
type Boundary string

const (
	BoundaryRuntime  Boundary = "runtime"
	BoundaryExternal Boundary = "external"
)

// Error is a structured cstore error per ADR-0010.
type Error struct {
	Class     FailureClass
	Message   string
	Retriable bool
	Boundary  Boundary
	File      string
	Line      int
	Details   map[string]any
}

// Error implements the error interface.
func (e *Error) Error() string {
	loc := ""
	switch {
	case e.File != "" && e.Line > 0:
		loc = fmt.Sprintf(" (%s:%d)", e.File, e.Line)
	case e.File != "":
		loc = fmt.Sprintf(" (%s)", e.File)
	}
	return fmt.Sprintf("%s: %s%s", e.Class, e.Message, loc)
}

func newParseErr(file string, line int, format string, args ...any) *Error {
	return &Error{
		Class:    ClassParseError,
		Message:  fmt.Sprintf(format, args...),
		Boundary: BoundaryRuntime,
		File:     file,
		Line:     line,
	}
}

func newValidationErr(file string, format string, args ...any) *Error {
	return &Error{
		Class:    ClassValidationError,
		Message:  fmt.Sprintf(format, args...),
		Boundary: BoundaryRuntime,
		File:     file,
	}
}

func newError(class FailureClass, boundary Boundary, retriable bool, format string, args ...any) *Error {
	return &Error{
		Class:     class,
		Message:   fmt.Sprintf(format, args...),
		Boundary:  boundary,
		Retriable: retriable,
	}
}
