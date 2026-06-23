package nodedist

import "fmt"

// ErrorKind is the closed-set classification of nodedist failures. It
// borrows cstore's failure vocabulary in spirit (fetch / checksum / signature
// / store) while staying local to this package, since the Node flow does not
// share cstore's connector-specific classes.
type ErrorKind string

const (
	// ErrUnsupportedTarget is returned for a GOOS/GOARCH pair this package
	// has no Node distribution mapping for.
	ErrUnsupportedTarget ErrorKind = "unsupported_target"

	// ErrInvalidVersion is an empty or malformed Node version string.
	ErrInvalidVersion ErrorKind = "invalid_version"

	// ErrFetchFailed is a download failure for one of the three URLs.
	ErrFetchFailed ErrorKind = "fetch_failed"

	// ErrBadSignature is a SHASUMS256.txt.asc that does not verify against
	// the trusted keyring. No checksum from the file is trusted in this
	// case.
	ErrBadSignature ErrorKind = "bad_signature"

	// ErrChecksumParse is a SHASUMS256.txt body that could not be parsed, or
	// that does not contain an entry for the requested archive.
	ErrChecksumParse ErrorKind = "checksum_parse"

	// ErrChecksumMismatch is a downloaded archive whose SHA-256 does not
	// match the signed entry in SHASUMS256.txt. Nothing is committed to the
	// store on this failure.
	ErrChecksumMismatch ErrorKind = "checksum_mismatch"

	// ErrUnpack is an archive extraction failure (corrupt archive, zip-slip
	// path traversal, unexpected layout).
	ErrUnpack ErrorKind = "unpack_failed"

	// ErrStore is a failure committing the unpacked tree to the
	// content-addressed cache.
	ErrStore ErrorKind = "store_failed"

	// ErrConfig is a misconfigured Fetcher (nil HTTP, nil keyring, empty
	// root).
	ErrConfig ErrorKind = "config_error"
)

// Error is a structured nodedist failure carrying a classification, a
// human-readable message, and an optional wrapped cause.
type Error struct {
	Kind    ErrorKind
	Message string
	Cause   error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// wrapErr builds a structured *Error of the given kind.
func wrapErr(kind ErrorKind, cause error, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Cause: cause}
}
