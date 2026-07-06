package credreq

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// RequiredBinding is one credential an operator must supply before a plan can
// run: the machine-readable requirement `aileron skill bind` (#1998) consumes.
// It is derived from a single (or a set of deduplicated) tool-step trust
// contract(s). It carries no credential value, only the non-secret declaration
// of what to onboard and where its secret must live.
type RequiredBinding struct {
	// CredentialKind is the trust contract's credential.kind (for example
	// "aws-sigv4"). One of the mapped kinds in scheme.go; `none` never appears.
	CredentialKind string
	// IdentityLabel is the trust contract's credential.identityLabel, the
	// non-secret account/subject label. Empty when the contract declares none.
	IdentityLabel string
	// Scheme is the injection scheme mapped from CredentialKind, one of
	// inject.AllSchemes() (for example "bearer" or "sigv4-resign").
	Scheme string
	// Hosts are the trust contract's declared upstream hosts, deduplicated
	// (case-insensitively, since hosts are case-insensitive by DNS semantics)
	// and lexically sorted for a stable representation. A retained entry keeps a
	// verbatim original casing, never a synthesized lowercase form; an entry may
	// carry a `{{ inputs.<name> }}` template token. For a host-less-identity
	// binding these are audit context, not the selection key.
	Hosts []string
	// TemplatedHosts is the subset of Hosts that carry at least one
	// `{{ inputs.<name> }}` interpolation token, deduplicated and sorted. The
	// token is flagged, never resolved (resolution is out of scope). It lets
	// #1998 decide how to surface a templated host to the operator.
	TemplatedHosts []string
	// HostShape records how the egress boundary selects this binding: by host
	// (host-keyed) or by credential identity (host-less-identity). See
	// HostShape and HostLessIdentity.
	HostShape HostShape
	// CredentialRef is the derived user-level vault path the operator's secret
	// must live at (for example "user/prod-reader"). It always satisfies the
	// user-ref grammar `user/[a-z0-9][a-z0-9_-]*`. See the package doc for the
	// exact, stable derivation rule.
	CredentialRef string
}

// HostLessIdentity reports whether the binding is selected by its (kind,
// identityLabel) pair rather than by upstream host (the AWS SigV4 shape,
// #1978). Tests and callers assert on this helper rather than on a bare field
// so the semantic, not the representation, is the contract.
func (r RequiredBinding) HostLessIdentity() bool {
	return r.HostShape == HostShapeHostLessIdentity
}

// unnamedCredentialService is the stable fallback service segment used only
// when neither the identity label nor the credential kind slugs to a non-empty
// value (a pathological, directly-constructed contract). It keeps the derived
// ref well-formed under the user-ref grammar in every case.
const unnamedCredentialService = "credential"

// slug normalizes an arbitrary label into a user-ref service segment. It
// lowercases, replaces each maximal run of characters outside [a-z0-9] with a
// single '-', and trims leading/trailing '-'. The result contains only
// [a-z0-9-] (a subset of the grammar's [a-z0-9_-]) and, when non-empty, starts
// with [a-z0-9], so it always satisfies the user-ref service grammar. Dots,
// spaces, mixed case, and non-ASCII all collapse to '-' boundaries; the result
// is empty only when the input carries no [a-z0-9] character at all.
func slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// deriveCredentialRef computes the deterministic, stable user-level vault path
// the operator's secret must live at. The service segment is slug(identityLabel)
// when a label is present, else slug(credentialKind), with a stable fallback
// when both slug to empty. The result always satisfies the user-ref grammar.
// See the package doc: this rule must not change without a migration.
func deriveCredentialRef(kind, identityLabel string) string {
	svc := ""
	if identityLabel != "" {
		svc = slug(identityLabel)
	}
	if svc == "" {
		svc = slug(kind)
	}
	if svc == "" {
		svc = unnamedCredentialService
	}
	return "user/" + svc
}

// templatedHosts returns the subset of hosts carrying at least one
// `{{ inputs.<name> }}` token, using the manifest package's source-of-truth
// grammar detector so this can never drift from the interpolation grammar.
// The plan reached Derive through Decode, which already validated the token
// grammar, so a scan error here is defensive; it is surfaced rather than
// swallowed.
func templatedHosts(hosts []string) ([]string, error) {
	var out []string
	for _, h := range hosts {
		refs, err := manifest.CommandInputRefs(h)
		if err != nil {
			return nil, fmt.Errorf("credreq: host %q: %w", h, err)
		}
		if len(refs) > 0 {
			out = append(out, h)
		}
	}
	return out, nil
}

// dedupHostsSorted returns a stable host-list representation:
// case-insensitively deduplicated and lexically sorted. Hosts are
// case-insensitive by DNS semantics, and the host-keyed dedup key already
// case-folds (see lowerJoin), so two entries differing only in case are one
// logical host and collapse here too. The retained entry keeps a VERBATIM
// casing (never a synthesized lowercase form): for a set of case-variants the
// lexicographically smallest original is chosen, so the result is fully
// deterministic regardless of input order across merged steps.
func dedupHostsSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	best := make(map[string]string, len(in))
	for _, s := range in {
		k := strings.ToLower(s)
		if cur, ok := best[k]; !ok || s < cur {
			best[k] = s
		}
	}
	out := make([]string, 0, len(best))
	for _, v := range best {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// lowerJoin lowercases and sorts hosts, deduplicated, then joins them, for use
// as the host-keyed dedup key. Lowercasing means two steps that declare the
// same hosts in different case collapse to one binding.
func lowerJoin(hosts []string) string {
	seen := make(map[string]struct{}, len(hosts))
	low := make([]string, 0, len(hosts))
	for _, h := range hosts {
		l := strings.ToLower(h)
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		low = append(low, l)
	}
	sort.Strings(low)
	return strings.Join(low, ",")
}

// Derive turns a decoded plan's tool-step trust contracts into the
// deduplicated, deterministically ordered set of credential-binding
// requirements an operator must supply. It is pure: no I/O, and the same plan
// always yields the same set.
//
// It considers only runtime.KindTool steps carrying a non-nil TrustContract
// (action-level contracts are out of scope). A `none`-kind step onboards no
// credential and is skipped. A contract whose credential.kind is outside the
// closed scheme table is a hard error (fail closed): the derivation never
// guesses a scheme for an unknown kind.
//
// Deduplication collapses requirements that are interchangeable for onboarding:
//   - host-less-identity (sigv4): keyed by (kind, identityLabel, scheme); hosts
//     are excluded from the key, so steps sharing one identity but reaching
//     different (possibly templated) hosts collapse to a single binding whose
//     Hosts is the union.
//   - host-keyed: keyed by (kind, identityLabel, scheme, sorted-lowercased
//     hosts), so distinct host sets stay distinct.
//
// The returned slice is sorted by (CredentialRef, CredentialKind, IdentityLabel,
// Scheme, Hosts) so the result is stable and refactoring-proof.
func Derive(plan *runtime.Plan) ([]RequiredBinding, error) {
	if plan == nil {
		return nil, fmt.Errorf("credreq: nil plan")
	}

	byKey := map[string]*RequiredBinding{}
	// order preserves first-seen key order only as a tie-safe intermediate;
	// the final slice is sorted deterministically below.
	var keys []string

	for i := range plan.Steps {
		s := plan.Steps[i]
		if s.Kind != runtime.KindTool || s.TrustContract == nil {
			continue
		}
		tc := s.TrustContract
		scheme, shape, skip, err := schemeFor(tc.CredentialKind)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}

		templated, err := templatedHosts(tc.Hosts)
		if err != nil {
			return nil, err
		}

		var key string
		if shape == HostShapeHostLessIdentity {
			// Hosts excluded from the key: one identity collapses regardless of
			// which (templated) hosts each step declares.
			key = strings.Join([]string{"hli", tc.CredentialKind, tc.IdentityLabel, scheme}, "\x00")
		} else {
			key = strings.Join([]string{"hk", tc.CredentialKind, tc.IdentityLabel, scheme, lowerJoin(tc.Hosts)}, "\x00")
		}

		if existing, ok := byKey[key]; ok {
			// Merge: union the hosts and templated hosts for a stable
			// representation. For a host-keyed key the host sets are already
			// equal (they are part of the key), so the union is a no-op there;
			// for a host-less key it is the load-bearing merge.
			existing.Hosts = dedupHostsSorted(append(existing.Hosts, tc.Hosts...))
			existing.TemplatedHosts = dedupHostsSorted(append(existing.TemplatedHosts, templated...))
			continue
		}

		rb := &RequiredBinding{
			CredentialKind: tc.CredentialKind,
			IdentityLabel:  tc.IdentityLabel,
			Scheme:         scheme,
			Hosts:          dedupHostsSorted(tc.Hosts),
			TemplatedHosts: dedupHostsSorted(templated),
			HostShape:      shape,
			CredentialRef:  deriveCredentialRef(tc.CredentialKind, tc.IdentityLabel),
		}
		byKey[key] = rb
		keys = append(keys, key)
	}

	out := make([]RequiredBinding, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	sort.Slice(out, func(a, b int) bool {
		return less(out[a], out[b])
	})
	return out, nil
}

// less orders two requirements by (CredentialRef, CredentialKind,
// IdentityLabel, Scheme, Hosts) so Derive's output is stable and independent of
// step declaration order.
func less(a, b RequiredBinding) bool {
	if a.CredentialRef != b.CredentialRef {
		return a.CredentialRef < b.CredentialRef
	}
	if a.CredentialKind != b.CredentialKind {
		return a.CredentialKind < b.CredentialKind
	}
	if a.IdentityLabel != b.IdentityLabel {
		return a.IdentityLabel < b.IdentityLabel
	}
	if a.Scheme != b.Scheme {
		return a.Scheme < b.Scheme
	}
	return strings.Join(a.Hosts, ",") < strings.Join(b.Hosts, ",")
}

// DeriveFromFrozen reads and signature-verifies a frozen skill version from the
// store, then derives its credential-binding requirements. It is a thin wrapper
// over runtime.LoadVerified followed by Derive: the only I/O is the store read
// the brief permits, and the requirements are derived from the verified,
// decoded plan (so the derivation inherits signature + content-hash
// verification for free). All derivation logic lives in the pure Derive.
func DeriveFromFrozen(s *store.Store, name, id string) ([]RequiredBinding, error) {
	lp, err := runtime.LoadVerified(s, name, id)
	if err != nil {
		return nil, err
	}
	return Derive(lp.Plan)
}
