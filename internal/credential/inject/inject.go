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
// [ADR-0019]. [SchemeSigV4Resign] re-signs the request with an AWS
// Signature Version 4 signature derived from the secret access key; the
// signing core is std-library-only (no AWS SDK). [ErrSchemeNotImplemented]
// is retained as a sentinel for the closed-set contract but is no longer
// returned by [Inject] for any current scheme.
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
// [SchemeHeaderTemplate] [Params.Template]. The injector uses "{token}"
// while the connector manifest's credentialFormat field uses "{key}";
// these are intentionally distinct placeholder strings, not a shared
// convention.
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

	// AccessKeyID is the AWS access key ID, used only by
	// [SchemeSigV4Resign]. It is non-secret (it appears verbatim in the
	// Credential= field of the Authorization header) and is required for
	// that scheme. The secret access key arrives via the secret argument
	// to [Inject], never through this struct.
	AccessKeyID string

	// Region is the AWS region (e.g. "us-east-1"), used only by
	// [SchemeSigV4Resign] for the credential scope. Required for that
	// scheme.
	Region string

	// Service is the AWS service name (e.g. "s3"), used only by
	// [SchemeSigV4Resign] for the credential scope. Required for that
	// scheme.
	Service string
}

// Inject binds secret onto req according to scheme. The secret bytes are
// written only into the request surface the scheme defines (a header
// value or a query parameter); they never appear in the returned error.
//
// For [SchemeSigV4Resign] the required [Params] fields are AccessKeyID,
// Region, and Service, and the secret argument carries the AWS secret
// access key; the secret is used only as HMAC key material to derive the
// signing key and never appears in the resulting Authorization header.
//
// Returns [ErrMissingParam] if a scheme's required [Params] field is
// absent and [ErrUnknownScheme] for any value outside the closed set. On
// any error the request is left unmutated.
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
		if params.AccessKeyID == "" {
			return fmt.Errorf("%w: sigv4-resign scheme requires AccessKeyID", ErrMissingParam)
		}
		if params.Region == "" {
			return fmt.Errorf("%w: sigv4-resign scheme requires Region", ErrMissingParam)
		}
		if params.Service == "" {
			return fmt.Errorf("%w: sigv4-resign scheme requires Service", ErrMissingParam)
		}
		return signSigV4(req, secret, params.AccessKeyID, params.Region, params.Service, now())

	default:
		// scheme did not originate from ParseScheme; treat as unknown.
		return fmt.Errorf("%w: %q", ErrUnknownScheme, string(scheme))
	}
}
