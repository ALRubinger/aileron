// Package systestlib provides generic assertion + logging helpers for the
// `aileron launch` system-test scenarios (#1476 codex, #1477 claude), ported
// from the POSIX-shell assert.sh helper library.
//
// The shell helpers signal a violation by returning non-zero and writing an
// actionable message to stderr; nothing aborts on its own, so a caller decides
// whether a failed assertion is fatal. The Go port models that contract with
// the idiomatic error return: nil means the asserted condition holds, and a
// non-nil error carries the diagnostic message (faithful to the shell stderr
// text) so callers and tests can inspect it.
package systestlib

import (
	"fmt"
	"strings"
)

// Fail returns a non-nil error carrying msg, mirroring the shell `fail` helper
// that always prints `systest: FAIL: <msg>` to stderr and returns 1. Use it for
// bespoke checks the Assert* helpers below do not cover.
func Fail(msg string) error {
	return fmt.Errorf("systest: FAIL: %s", msg)
}

// AssertEq reports byte-exact string equality between expected and actual.
// It returns nil when the two are equal and, on mismatch, a non-nil error whose
// message includes both values so the failure is diagnosable from CI logs alone
// (mirroring assert_eq's stderr output).
func AssertEq(expected, actual, what string) error {
	if expected == actual {
		return nil
	}
	return fmt.Errorf("systest: FAIL: %s\n  expected: %s\n  actual:   %s", what, expected, actual)
}

// AssertContains reports whether needle is a substring of haystack. It returns
// nil when haystack contains needle and, otherwise, a non-nil error naming both.
// strings.Contains is byte-oriented and therefore newline-safe, matching the
// shell helper's case-glob behavior on multi-line haystacks (env dumps, mount
// lists).
func AssertContains(haystack, needle, what string) error {
	if strings.Contains(haystack, needle) {
		return nil
	}
	return fmt.Errorf("systest: FAIL: %s\n  expected to contain: %s\n  in: %s", what, needle, haystack)
}

// AssertNotEmpty reports whether value is non-empty. It returns nil for a
// non-empty value and, for an empty value, the same error Fail would produce
// for `<what> (value was empty)`, mirroring assert_not_empty.
func AssertNotEmpty(value, what string) error {
	if value != "" {
		return nil
	}
	return Fail(fmt.Sprintf("%s (value was empty)", what))
}
