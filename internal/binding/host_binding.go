package binding

import (
	"fmt"
	"regexp"
	"strings"
)

// HostBindingSchemes is the closed set of injection schemes a host
// binding may declare. The egress-side injector that consumes a matched
// binding (#1194) is keyed on these exact strings. v1 implements
// `bearer` concretely; the remaining schemes are reserved for the
// scheme-keyed injector that lands alongside this table.
//
// The set is closed deliberately: a host binding that names a scheme
// outside this set is a configuration error caught at construction
// rather than a silent passthrough at the egress boundary.
var HostBindingSchemes = map[string]struct{}{
	"bearer":          {},
	"basic":           {},
	"header-template": {},
	"query-param":     {},
	"sigv4-resign":    {},
}

// SchemeBearer mirrors the connector-spec path's
// `Authorization: Bearer <token>` behavior.
const SchemeBearer = "bearer"

// SchemeBasic emits `Authorization: Basic base64(<username>:<token>)`,
// the HTTP basic-auth convention git-over-HTTPS uses (username
// `x-access-token`, token in the password field). The username travels
// in [HostBinding.BasicUsername]; it is a non-secret param, never the
// credential bytes.
const SchemeBasic = "basic"

// HostBinding is the declarative triple that maps an upstream host
// pattern to a vault credential and the scheme used to inject it. It is
// a distinct concept from [Binding]: a [Binding] is keyed on
// (connector FQN, kind) and resolves a connector-spec credential
// reference, whereas a HostBinding is keyed on a host pattern and is
// consulted at the TLS forward-proxy boundary when no connector spec
// matched (ADR-0019 launch passthrough).
//
// The zero value is invalid; construct via [NewHostBinding].
type HostBinding struct {
	// HostPattern is an exact host (`api.example.com`) or a single
	// leading-wildcard form (`*.example.com`). The wildcard matches
	// exactly one or more leading labels' worth of suffix (see
	// [HostBindings.Match] for the matching rule). Ports are not part
	// of the pattern; callers strip the port before matching.
	HostPattern string

	// CredentialRef is a vault path the daemon resolves through the
	// credential layer at egress time. Two shapes are valid: a
	// connector-style binding name (`<kind>/<service>/<identity>`) or a
	// user-level credential ref (`user/<service>`), the namespace that
	// `aileron auth <service>` writes. It is never the credential bytes;
	// it points at where the bytes live in the user's vault.
	CredentialRef string

	// Scheme is the injection scheme, one of [HostBindingSchemes].
	Scheme string

	// BasicUsername is the username portion of HTTP basic auth, used
	// only when Scheme is [SchemeBasic] (e.g. `x-access-token` for
	// git-over-HTTPS). It is a non-secret param: the token always goes
	// in the password field, never here. Required for the basic scheme,
	// ignored for every other scheme.
	BasicUsername string
}

// userRefRe matches the user-level credential namespace `user/<service>`
// that `aileron auth <service>` writes (see userCredentialVaultPath). It
// is the only valid two-segment credential ref: connector-style refs
// must still carry the full `<kind>/<service>/<identity>` triple.
var userRefRe = regexp.MustCompile(`^user/([a-z0-9][a-z0-9_-]*)$`)

// HostBindingOption configures optional, scheme-specific fields on a
// [HostBinding] at construction. Options keep the positional signature
// of [NewHostBinding] stable while letting schemes that need extra
// non-secret params (e.g. basic auth's username) declare them.
type HostBindingOption func(*HostBinding)

// WithBasicUsername sets [HostBinding.BasicUsername], the non-secret
// username used by [SchemeBasic]. It is required for that scheme.
func WithBasicUsername(username string) HostBindingOption {
	return func(hb *HostBinding) { hb.BasicUsername = username }
}

// NewHostBinding validates and constructs a HostBinding. It rejects an
// empty or malformed host pattern, a credential-ref that is neither a
// connector binding name (`<kind>/<service>/<identity>`) nor a
// user-level ref (`user/<service>`), and a scheme outside
// [HostBindingSchemes]. When the scheme is [SchemeBasic] a non-empty
// [WithBasicUsername] is required: a basic binding with no username
// could never produce a well-formed Authorization header, so it is a
// configuration error caught here rather than a fail-closed reject at
// egress. The validation is the construction-time guard the issue's
// acceptance criterion requires: no invalid binding ever reaches the
// matcher.
func NewHostBinding(hostPattern, credentialRef, scheme string, opts ...HostBindingOption) (HostBinding, error) {
	hp := strings.TrimSpace(strings.ToLower(hostPattern))
	if hp == "" {
		return HostBinding{}, fmt.Errorf("host binding: empty host pattern")
	}
	if err := validateHostPattern(hp); err != nil {
		return HostBinding{}, err
	}
	if !nameRe.MatchString(credentialRef) && !userRefRe.MatchString(credentialRef) {
		return HostBinding{}, fmt.Errorf("host binding: invalid credential ref %q: must match <kind>/<service>/<identity> or user/<service>", credentialRef)
	}
	if _, ok := HostBindingSchemes[scheme]; !ok {
		return HostBinding{}, fmt.Errorf("host binding: unknown injection scheme %q", scheme)
	}
	hb := HostBinding{HostPattern: hp, CredentialRef: credentialRef, Scheme: scheme}
	for _, opt := range opts {
		opt(&hb)
	}
	if scheme == SchemeBasic && hb.BasicUsername == "" {
		return HostBinding{}, fmt.Errorf("host binding: basic scheme requires a username (WithBasicUsername)")
	}
	return hb, nil
}

// validateHostPattern enforces the exact-host or single leading-wildcard
// form. A wildcard pattern is `*.<suffix>` where <suffix> is a
// non-empty dotted host with at least one dot (so `*.com` matching a
// bare TLD is rejected as too broad). Exact patterns must be a
// non-empty dotted or single-label host with no wildcard.
func validateHostPattern(hp string) error {
	if strings.HasPrefix(hp, "*.") {
		suffix := strings.TrimPrefix(hp, "*.")
		if suffix == "" || strings.Contains(suffix, "*") || !strings.Contains(suffix, ".") {
			return fmt.Errorf("host binding: invalid wildcard host pattern %q: want *.<domain.tld>", hp)
		}
		return nil
	}
	if strings.Contains(hp, "*") {
		return fmt.Errorf("host binding: wildcard only allowed as a single leading label (%q)", hp)
	}
	return nil
}

// HostBindings is an ordered lookup over host bindings. Match resolves
// the most specific binding for a host: an exact-host binding always
// beats a wildcard, and among wildcards the longest matching suffix
// wins. The zero value (nil) is a valid empty table whose Match always
// returns false, which is what preserves today's passthrough behavior
// when no bindings are configured.
type HostBindings []HostBinding

// Match returns the most specific HostBinding whose pattern matches the
// given host, and true, or the zero binding and false when none match.
//
// Matching rule:
//   - The host is lowercased and any port is the caller's
//     responsibility to strip before calling.
//   - An exact-host pattern matches only that host.
//   - A wildcard pattern `*.example.com` matches any host that is a
//     proper subdomain (`a.example.com`, `a.b.example.com`) but NOT the
//     apex (`example.com`).
//   - Resolution is deterministic: exact beats wildcard; among matching
//     wildcards the longest suffix wins. There is no silent multi-match.
func (h HostBindings) Match(host string) (HostBinding, bool) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return HostBinding{}, false
	}

	var exact *HostBinding
	var bestWildcard *HostBinding
	bestWildcardLen := -1

	for i := range h {
		hb := h[i]
		if strings.HasPrefix(hb.HostPattern, "*.") {
			suffix := strings.TrimPrefix(hb.HostPattern, "*.")
			// Proper-subdomain match: host ends with ".<suffix>" and has
			// at least one label before the suffix. The apex host
			// (equal to suffix) does not match a wildcard.
			if strings.HasSuffix(host, "."+suffix) && len(host) > len(suffix)+1 {
				if len(suffix) > bestWildcardLen {
					bestWildcardLen = len(suffix)
					hbCopy := hb
					bestWildcard = &hbCopy
				}
			}
			continue
		}
		if hb.HostPattern == host {
			hbCopy := hb
			exact = &hbCopy
		}
	}

	if exact != nil {
		return *exact, true
	}
	if bestWildcard != nil {
		return *bestWildcard, true
	}
	return HostBinding{}, false
}
