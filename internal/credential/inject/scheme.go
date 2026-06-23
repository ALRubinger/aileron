package inject

import (
	"errors"
	"fmt"
)

// Scheme is the closed set of credential-injection schemes ratified by
// [ADR-0019]. A Scheme names *how* a resolved secret is bound onto an
// outbound HTTP request (e.g. as a bearer header, HTTP basic auth, or a
// query parameter). The set is intentionally closed: anything outside
// the enumerated members is rejected by [ParseScheme] so a typo or an
// unsupported vendor convention fails fast rather than silently leaking
// a credential into the wrong place.
//
// [ADR-0019]: https://docs.withaileron.ai/adr/0019-v4-https-data-plane
type Scheme string

const (
	// SchemeBearer sets "Authorization: Bearer <token>".
	SchemeBearer Scheme = "bearer"

	// SchemeBasic sets "Authorization: Basic base64(<username>:<token>)".
	SchemeBasic Scheme = "basic"

	// SchemeHeaderTemplate sets an arbitrary header to a verbatim
	// template value with the "{token}" placeholder substituted.
	SchemeHeaderTemplate Scheme = "header-template"

	// SchemeQueryParam sets a query parameter on the request URL to the
	// token value.
	SchemeQueryParam Scheme = "query-param"

	// SchemeSigV4Resign re-signs the request with an AWS Signature
	// Version 4 signature derived from the secret access key. The signing
	// core is std-library-only (crypto/hmac + crypto/sha256); no AWS SDK
	// dependency is pulled in.
	SchemeSigV4Resign Scheme = "sigv4-resign"
)

// Sentinel errors. Callers pattern-match with errors.Is.
var (
	// ErrUnknownScheme means a string did not name one of the closed-set
	// schemes. Returned by [ParseScheme].
	ErrUnknownScheme = errors.New("inject: unknown injection scheme")

	// ErrSchemeNotImplemented means the scheme is a recognized member of
	// the closed set but its injection logic is deferred. It is retained
	// as a sentinel for the closed-set contract; no current scheme is
	// deferred, so [Inject] does not return it today.
	ErrSchemeNotImplemented = errors.New("inject: injection scheme not implemented")

	// ErrMissingParam means a scheme requires a [Params] field that was
	// not supplied (e.g. basic auth without a username). Returned by
	// [Inject].
	ErrMissingParam = errors.New("inject: required parameter missing")
)

// allSchemes is the canonical closed set, in ADR order. Exposed via
// [AllSchemes]; tests assert the set is exactly these five members.
var allSchemes = []Scheme{
	SchemeBearer,
	SchemeBasic,
	SchemeHeaderTemplate,
	SchemeQueryParam,
	SchemeSigV4Resign,
}

// AllSchemes returns a copy of the closed set of schemes, in ADR order.
func AllSchemes() []Scheme {
	out := make([]Scheme, len(allSchemes))
	copy(out, allSchemes)
	return out
}

// ParseScheme maps a string to its [Scheme] constant. Any value outside
// the closed set returns a wrapped [ErrUnknownScheme].
func ParseScheme(s string) (Scheme, error) {
	for _, scheme := range allSchemes {
		if string(scheme) == s {
			return scheme, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownScheme, s)
}
