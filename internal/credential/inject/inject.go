// Package inject binds a host-resolved credential onto an outbound HTTP
// request according to a closed set of injection [Scheme]s.
//
// It is a sub-package of credential so it can reference the resolved
// secret bytes without creating an import cycle (credential does not
// import inject). The injector is a pure function over
// (scheme, request, secret, params) with zero knowledge of connectors,
// CLIs, GitHub, AWS, or any specific service. The scheme-keyed dispatch
// in [Inject] replaces the per-service/per-kind branching that previously
// lived at the proxy egress point.
//
// # Secret handling
//
// The secret bytes are passed to [Inject] separately from [Params] so
// the param struct can be logged or echoed safely while the secret never
// can. This package contains no logging calls, and no code path writes
// the secret into an error message, a return value, or any other
// observable surface other than the request header/query value it is
// being injected into. The secret is host-side only and must never reach
// an audit record or the sandbox guest (see [ADR-0005] and [ADR-0011]).
//
// # Scheme set
//
// The five schemes ([SchemeBearer], [SchemeBasic], [SchemeHeaderTemplate],
// [SchemeQueryParam], [SchemeSigV4Resign]) are the closed set ratified by
// [ADR-0019]. [SchemeSigV4Resign] is enumerated but deferred; it returns
// [ErrSchemeNotImplemented].
//
// [ADR-0005]: https://docs.withaileron.ai/adr/0005-sandbox-choice
// [ADR-0011]: https://docs.withaileron.ai/adr/0011-local-credential-vault
// [ADR-0019]: https://docs.withaileron.ai/adr/0019-v4-https-data-plane
package inject

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// tokenPlaceholder is the substring substituted with the secret in a
// [SchemeHeaderTemplate] [Params.Template]. It mirrors the "{key}"
// convention used by the connector manifest's credentialFormat field.
const tokenPlaceholder = "{token}"

// Params carries the scheme-specific, non-secret inputs to [Inject].
// Every field is safe to log or echo; the secret bytes are never stored
// here. Each scheme reads only the fields it needs and validates their
// presence before touching the secret.
type Params struct {
	// Username is the user portion of HTTP basic auth, used only by
	// [SchemeBasic] (e.g. "x-access-token" for git-over-HTTPS). Required
	// for that scheme.
	Username string

	// HeaderName is the header to set, used only by
	// [SchemeHeaderTemplate] (e.g. "Authorization" or a vendor header).
	// Required for that scheme.
	HeaderName string

	// Template is the verbatim header value for [SchemeHeaderTemplate],
	// with the "{token}" placeholder substituted with the secret at
	// inject time. If empty, the header is set to the raw token.
	Template string

	// ParamName is the query-parameter name to set, used only by
	// [SchemeQueryParam]. Required for that scheme.
	ParamName string
}

// Inject binds secret onto req according to scheme. The secret bytes are
// written only into the request surface the scheme defines (a header
// value or a query parameter); they never appear in the returned error.
//
// Returns [ErrMissingParam] if a scheme's required [Params] field is
// absent, [ErrSchemeNotImplemented] for [SchemeSigV4Resign], and
// [ErrUnknownScheme] for any value outside the closed set. On any error
// the request is left unmutated.
func Inject(req *http.Request, scheme Scheme, secret []byte, params Params) error {
	if req == nil {
		return fmt.Errorf("%w: request is nil", ErrMissingParam)
	}

	switch scheme {
	case SchemeBearer:
		req.Header.Set("Authorization", "Bearer "+string(secret))
		return nil

	case SchemeBasic:
		if params.Username == "" {
			return fmt.Errorf("%w: basic scheme requires Username", ErrMissingParam)
		}
		userPass := params.Username + ":" + string(secret)
		encoded := base64.StdEncoding.EncodeToString([]byte(userPass))
		req.Header.Set("Authorization", "Basic "+encoded)
		return nil

	case SchemeHeaderTemplate:
		if params.HeaderName == "" {
			return fmt.Errorf("%w: header-template scheme requires HeaderName", ErrMissingParam)
		}
		value := string(secret)
		if params.Template != "" {
			value = strings.ReplaceAll(params.Template, tokenPlaceholder, string(secret))
		}
		req.Header.Set(params.HeaderName, value)
		return nil

	case SchemeQueryParam:
		if params.ParamName == "" {
			return fmt.Errorf("%w: query-param scheme requires ParamName", ErrMissingParam)
		}
		q := req.URL.Query()
		q.Set(params.ParamName, string(secret))
		req.URL.RawQuery = q.Encode()
		return nil

	case SchemeSigV4Resign:
		return fmt.Errorf("%w: sigv4-resign is enumerated but deferred until an AWS-style consumer appears", ErrSchemeNotImplemented)

	default:
		// scheme did not originate from ParseScheme; treat as unknown.
		return fmt.Errorf("%w: %q", ErrUnknownScheme, string(scheme))
	}
}
