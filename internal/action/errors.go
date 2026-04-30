package action

import (
	"fmt"
)

// Boundary identifies which layer produced an error, per ADR-0010.
//
// Action loading and capability enforcement live at the action boundary.
type Boundary string

const (
	// BoundaryAction names the action layer (manifest parse failures and
	// capability-subset denials).
	BoundaryAction Boundary = "action"
	// BoundaryRuntime names the surrounding runtime layer.
	BoundaryRuntime Boundary = "runtime"
)

// FailureClass is the closed-set classification of failures per ADR-0010.
// Adding a new class requires an ADR amendment.
type FailureClass string

const (
	// ClassParseError is a structured parse failure for an action manifest.
	// Not part of the ADR-0010 closed taxonomy directly; surfaces during
	// load before any execution path runs.
	ClassParseError FailureClass = "parse_error"

	// ClassValidationError is a structured validation failure for an action
	// manifest (missing required field, invalid FQN, etc.).
	ClassValidationError FailureClass = "validation_error"

	// ClassCapabilityDenied is the canonical class from ADR-0010 for a
	// capability-subset refusal at the action boundary.
	ClassCapabilityDenied FailureClass = "capability_denied"

	// ClassNotFound names an action lookup that did not match any installed
	// action by name.
	ClassNotFound FailureClass = "not_found"
)

// Error is a structured action-layer error per ADR-0010. Loading and
// enforcement errors carry enough context (file, line, boundary) for callers
// to surface a precise message to the user without re-parsing or
// re-formatting.
type Error struct {
	// Class is the canonical failure classification.
	Class FailureClass
	// Message is a human-readable description; safe to surface to the user.
	// Does not contain credentials.
	Message string
	// Retriable signals whether the failure is safe to retry. Per ADR-0010,
	// validation failures and capability denials are terminal.
	Retriable bool
	// Boundary is the layer that produced the error.
	Boundary Boundary
	// File is the path of the action file that produced the error, when
	// applicable.
	File string
	// Line is the line within File where the error was detected, when
	// applicable. Zero means "unknown / not applicable".
	Line int
	// Details is class-specific structured context (e.g. denied capability,
	// requested op). Keys are stable strings.
	Details map[string]any
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

// ErrNotFound is the sentinel matched by errors.Is for "no installed action
// with the given name".
var ErrNotFound = &Error{
	Class:    ClassNotFound,
	Boundary: BoundaryAction,
	Message:  "action not found",
}

// newParseErr builds a ClassParseError with the given file/line context.
func newParseErr(file string, line int, format string, args ...any) *Error {
	return &Error{
		Class:    ClassParseError,
		Message:  fmt.Sprintf(format, args...),
		Boundary: BoundaryAction,
		File:     file,
		Line:     line,
	}
}

// newValidationErr builds a ClassValidationError with the given file context.
func newValidationErr(file string, format string, args ...any) *Error {
	return &Error{
		Class:    ClassValidationError,
		Message:  fmt.Sprintf(format, args...),
		Boundary: BoundaryAction,
		File:     file,
	}
}
