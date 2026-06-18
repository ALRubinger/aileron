package binding

import (
	"fmt"
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

// SchemeBearer is the only scheme this PR injects concretely. It
// mirrors the connector-spec path's `Authorization: Bearer <token>`
// behavior. The remaining schemes in [HostBindingSchemes] are accepted
// at construction but slot into the #1194 injector at egress time.
const SchemeBearer = "bearer"

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

	// CredentialRef is a vault binding name (`<kind>/<service>/<identity>`)
	// the daemon resolves through the credential layer at egress time.
	// It is never the credential bytes; it points at where the bytes
	// live in the user's vault.
	CredentialRef string

	// Scheme is the injection scheme, one of [HostBindingSchemes].
	Scheme string
}

// NewHostBinding validates and constructs a HostBinding. It rejects an
// empty or malformed host pattern, a credential-ref that does not parse
// as a binding name, and a scheme outside [HostBindingSchemes]. The
// validation is the construction-time guard the issue's acceptance
// criterion requires: no invalid binding ever reaches the matcher.
func NewHostBinding(hostPattern, credentialRef, scheme string) (HostBinding, error) {
	hp := strings.TrimSpace(strings.ToLower(hostPattern))
	if hp == "" {
		return HostBinding{}, fmt.Errorf("host binding: empty host pattern")
	}
	if err := validateHostPattern(hp); err != nil {
		return HostBinding{}, err
	}
	if !nameRe.MatchString(credentialRef) {
		return HostBinding{}, fmt.Errorf("host binding: invalid credential ref %q: must match <kind>/<service>/<identity>", credentialRef)
	}
	if _, ok := HostBindingSchemes[scheme]; !ok {
		return HostBinding{}, fmt.Errorf("host binding: unknown injection scheme %q", scheme)
	}
	return HostBinding{HostPattern: hp, CredentialRef: credentialRef, Scheme: scheme}, nil
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
