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

// SchemeHeaderTemplate emits an arbitrary header set to a verbatim
// template value, with a token placeholder the egress injector (#1194)
// substitutes with the credential bytes. It expresses a vendor header
// that carries the token with no fixed prefix, e.g. Linear's verbatim
// `Authorization: <key>` (no `Bearer`). Carries [HostBinding.HeaderName]
// and [HostBinding.HeaderTemplate], both non-secret.
const SchemeHeaderTemplate = "header-template"

// SchemeQueryParam emits the credential as a URL query parameter named by
// [HostBinding.QueryParamName].
const SchemeQueryParam = "query-param"

// SchemeSigV4Resign re-signs the request with an AWS Signature Version 4
// signature. It carries the non-secret [HostBinding.AccessKeyID],
// [HostBinding.Region], and [HostBinding.Service], all required for this
// scheme. The secret access key travels in the resolved credential value
// (never in the binding), and the egress injector consumes it only as HMAC
// key material to derive the signing key. Static keys only: session tokens
// (X-Amz-Security-Token) are a declared follow-up, not supported here.
const SchemeSigV4Resign = "sigv4-resign"

// HostBindingEffects is the closed set of trust-contract effect strings a
// host binding may declare. It mirrors the runtime's effect vocabulary,
// naming both `runtime.Effect` and `runtime.TrustContract` as the source
// of truth so the two cannot silently diverge: the manifest `trustContract`
// block declares an effect via `runtime.Effect`, the freeze/launch emit
// path copies it onto a per-step [HostBinding], and the proxy enforces it.
//
// The `binding` package cannot import `runtime` (layering), so the set is
// redefined here as plain strings with this cross-reference. A binding that
// names an effect outside this set is a configuration error caught at
// construction rather than a silent unconstrained egress at the boundary.
var HostBindingEffects = map[string]struct{}{
	EffectRead:         {},
	EffectWrite:        {},
	EffectDelete:       {},
	EffectSpend:        {},
	EffectExternalSend: {},
}

// EffectRead is the read-class trust-contract effect. It mirrors
// `runtime.EffectRead`. A binding declaring it constrains egress on the
// bound host to HTTP-safe methods (GET/HEAD/OPTIONS): a mutating method is
// denied at the proxy injection point.
const EffectRead = "read"

// EffectWrite is the write-class trust-contract effect. It mirrors
// `runtime.EffectWrite`. A binding declaring it (or any other write-class
// effect below) admits every HTTP method at the proxy: the proxy sees only
// method + host and cannot distinguish write from delete/spend/external-send
// on the wire, so finer effect narrowing is enforced upstream at the runtime
// action-call approval seam, not here.
const EffectWrite = "write"

// EffectDelete mirrors `runtime.EffectDelete`. It is a write-class effect
// at the proxy seam (see [EffectWrite]).
const EffectDelete = "delete"

// EffectSpend mirrors `runtime.EffectSpend`. It is a write-class effect at
// the proxy seam (see [EffectWrite]).
const EffectSpend = "spend"

// EffectExternalSend mirrors `runtime.EffectExternalSend`. It is a
// write-class effect at the proxy seam (see [EffectWrite]).
const EffectExternalSend = "external-send"

// EmitMechanism names how the in-container client is made to emit a
// request the proxy can seal at egress (ADR-0019). It is orthogonal to
// [HostBinding.Scheme], which is how the proxy injects the credential
// once the request arrives.
type EmitMechanism string

const (
	// EmitMechanismInject is the default: the launcher plants no token. The
	// client emits an unauthenticated (or no-credential) request, and the
	// proxy unconditionally injects the bound credential at egress. Used
	// for clients that issue the request without holding a token (curl,
	// git-over-HTTPS with a no-op credential helper).
	EmitMechanismInject EmitMechanism = "inject"

	// EmitMechanismSentinelSwap is sentinel-swap: the launcher plants a
	// non-secret, format-mimicking sentinel token so a client that
	// short-circuits locally without a token (e.g. `gh`) issues its
	// request. The proxy swaps the sentinel for the real credential at
	// egress, and ONLY swaps when it recognizes its own planted sentinel. A
	// foreign token on a sentinel-swap host is left untouched (no
	// steal-swap, no real secret injected). See the swap recognizer at the
	// egress seam.
	EmitMechanismSentinelSwap EmitMechanism = "sentinel-swap"
)

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

	// HeaderName is the header to set, used only when Scheme is
	// [SchemeHeaderTemplate] (e.g. `Authorization` or a vendor header).
	// It is a non-secret param. Required for the header-template scheme,
	// ignored otherwise.
	HeaderName string

	// HeaderTemplate is the verbatim header value for
	// [SchemeHeaderTemplate], with a token placeholder substituted with
	// the credential bytes at egress (the injector owns the placeholder
	// convention). It lets a vendor express a header that carries the
	// token without a fixed prefix (e.g. Linear's verbatim
	// `Authorization: <key>` with no `Bearer`). Non-secret: it is a
	// template, never the credential bytes. Required for the
	// header-template scheme, ignored otherwise.
	HeaderTemplate string

	// QueryParamName is the query-parameter name to set, used only when
	// Scheme is [SchemeQueryParam]. Non-secret. Required for the
	// query-param scheme, ignored otherwise.
	QueryParamName string

	// AccessKeyID is the AWS access key ID, used only when Scheme is
	// [SchemeSigV4Resign]. It is a non-secret param: it appears verbatim in
	// the Credential= field of the signed Authorization header. The secret
	// access key travels in the resolved credential value, never here.
	// Required for the sigv4-resign scheme, ignored otherwise.
	AccessKeyID string

	// Region is the AWS region (e.g. "us-east-1") used for the SigV4
	// credential scope, used only when Scheme is [SchemeSigV4Resign]. It is
	// a non-secret param and is never inferred from the request host.
	// Required for the sigv4-resign scheme, ignored otherwise.
	Region string

	// Service is the AWS service name (e.g. "s3") used for the SigV4
	// credential scope, used only when Scheme is [SchemeSigV4Resign]. It is
	// a non-secret param and is never inferred from the request host.
	// Required for the sigv4-resign scheme, ignored otherwise.
	Service string

	// EmitMechanism declares how the in-container client is made to emit
	// a sealable request (see [EmitMechanism]). The zero value is
	// [EmitMechanismInject] (plant nothing, inject unconditionally).
	// [EmitMechanismSentinelSwap] enables the sentinel-swap gate at egress
	// for hosts whose client short-circuits without a planted token. Set
	// via [WithEmitMechanismSentinelSwap], which also requires the sentinel
	// shape below.
	EmitMechanism EmitMechanism

	// SentinelValue is the non-secret, format-mimicking placeholder the
	// launcher plants so a client that short-circuits locally without a
	// token still issues its request (e.g. `gh`'s `ghp_…` sentinel). The
	// egress recognizer compares the inbound carrier against this exact
	// value to decide whether to swap in the real credential. It carries
	// no authority and is safe to embed, log, and read in-sandbox (see the
	// internal/sentinel package doc). Required for
	// [EmitMechanismSentinelSwap], forbidden for [EmitMechanismInject]. Set
	// via [WithSentinel].
	SentinelValue string

	// SentinelEnv is the environment-variable name the launcher sets to
	// [HostBinding.SentinelValue] in the sandbox so the client treats
	// itself as authenticated (e.g. `GH_TOKEN`). It is a non-secret env
	// name, never the credential bytes. Required for
	// [EmitMechanismSentinelSwap], forbidden for [EmitMechanismInject]. Set
	// via [WithSentinel].
	SentinelEnv string

	// Effect is the per-step trust-contract effect the binding was frozen
	// with, one of [HostBindingEffects] (mirroring `runtime.Effect`; see
	// [HostBindingEffects] for the source-of-truth cross-reference). The
	// zero value ("") means the binding declares no effect and the proxy
	// applies no effect gate (unconstrained, today's behavior). A non-empty
	// value gates egress at the injection point: [EffectRead] admits only
	// HTTP-safe methods; every write-class value admits all methods. Set via
	// [WithTrustContract]. It is a non-secret param, never the credential
	// bytes.
	Effect string

	// AllowedHosts is the per-step trust-contract host allowlist the
	// binding was frozen with (mirroring `runtime.TrustContract.Hosts`). An
	// empty slice means the binding declares no allowlist and egress stays
	// scoped only to the matched [HostBinding.HostPattern] (unconstrained by
	// contract, today's behavior). A non-empty allowlist gates egress at the
	// injection point: the upstream host must match an entry or the request
	// is denied. Entries are trimmed and lowercased at construction (host or
	// host:port form). Set via [WithTrustContract]. Non-secret.
	AllowedHosts []string

	// PlanID, StepID, and ToolName are the audit-addressing identity triple
	// the freeze/launch emit path stamps onto a per-step binding so a
	// binding_injected or trust_denied audit record can name the flight-plan
	// plan, step, and tool that drove the egress. All three are optional and
	// non-secret; empty values are omitted from the audit payload. They are
	// audit addressing, not committed descriptor config, so they ride only on
	// the runtime-emitted binding. Set via [WithToolIdentity].
	PlanID   string
	StepID   string
	ToolName string
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

// WithHeaderTemplate sets the non-secret [HostBinding.HeaderName] and
// [HostBinding.HeaderTemplate] used by [SchemeHeaderTemplate]. Both are
// required for that scheme: a header-template binding with no header name
// or no template could never produce a well-formed header, so an empty
// value is a construction error.
func WithHeaderTemplate(header, template string) HostBindingOption {
	return func(hb *HostBinding) {
		hb.HeaderName = header
		hb.HeaderTemplate = template
	}
}

// WithQueryParam sets the non-secret [HostBinding.QueryParamName] used by
// [SchemeQueryParam]. It is required for that scheme.
func WithQueryParam(name string) HostBindingOption {
	return func(hb *HostBinding) { hb.QueryParamName = name }
}

// WithSigV4Resign sets the non-secret [HostBinding.AccessKeyID],
// [HostBinding.Region], and [HostBinding.Service] used by
// [SchemeSigV4Resign]. All three are required for that scheme: a
// sigv4-resign binding with no access key id, region, or service could
// never produce a well-formed signature, so an empty value is a
// construction error. The secret access key is never set here; it travels
// in the resolved credential value.
func WithSigV4Resign(accessKeyID, region, service string) HostBindingOption {
	return func(hb *HostBinding) {
		hb.AccessKeyID = accessKeyID
		hb.Region = region
		hb.Service = service
	}
}

// WithEmitMechanismSentinelSwap marks the binding as
// [EmitMechanismSentinelSwap]. Use it for a host whose in-container
// client short-circuits locally without a planted token (e.g. `gh`): the
// launcher plants a non-secret sentinel and the egress seam swaps it for
// the real credential only when it recognizes its own plant. Without this
// option a binding defaults to [EmitMechanismInject] (plant nothing,
// inject unconditionally).
//
// A sentinel-swap binding also requires the sentinel shape: pair this
// option with [WithSentinel] so the binding carries the value to plant
// and the env var to plant it into. The constructor rejects a
// sentinel-swap binding with no sentinel, since it could never be planted
// or recognized.
func WithEmitMechanismSentinelSwap() HostBindingOption {
	return func(hb *HostBinding) { hb.EmitMechanism = EmitMechanismSentinelSwap }
}

// WithSentinel sets the non-secret [HostBinding.SentinelValue] and
// [HostBinding.SentinelEnv] the launcher plants for a sentinel-swap
// binding. Both are required for [EmitMechanismSentinelSwap] and must not
// be set on an [EmitMechanismInject] binding (an inject binding plants
// nothing, so a stray sentinel is a configuration error caught at
// construction). The value is the single source of truth shared by the
// launch-side planter and the proxy-side recognizer, so the two cannot
// drift.
func WithSentinel(value, env string) HostBindingOption {
	return func(hb *HostBinding) {
		hb.SentinelValue = value
		hb.SentinelEnv = env
	}
}

// WithTrustContract sets the per-step trust scope [HostBinding.Effect] and
// [HostBinding.AllowedHosts] the proxy enforces at the injection point. Both
// are optional: an empty effect applies no effect gate and an empty
// allowedHosts applies no host allowlist, so a binding constructed without
// this option (or with empty values) stays unconstrained-by-contract and
// injects exactly as before. A non-empty effect must be a member of
// [HostBindingEffects] or construction fails closed. allowedHosts entries
// are trimmed and lowercased. The values are non-secret; they name the
// scope the manifest `trustContract` declared, never the credential bytes.
func WithTrustContract(effect string, allowedHosts []string) HostBindingOption {
	return func(hb *HostBinding) {
		hb.Effect = strings.TrimSpace(strings.ToLower(effect))
		hb.AllowedHosts = normalizeAllowedHosts(allowedHosts)
	}
}

// WithToolIdentity sets the non-secret audit-addressing triple
// [HostBinding.PlanID], [HostBinding.StepID], and [HostBinding.ToolName] the
// freeze/launch emit path stamps onto a per-step binding. All three are
// optional; empty values are omitted from the audit payload. They are audit
// addressing, never the credential bytes.
func WithToolIdentity(planID, stepID, toolName string) HostBindingOption {
	return func(hb *HostBinding) {
		hb.PlanID = strings.TrimSpace(planID)
		hb.StepID = strings.TrimSpace(stepID)
		hb.ToolName = strings.TrimSpace(toolName)
	}
}

// normalizeAllowedHosts trims and lowercases each allowlist entry, dropping
// empties. It reuses the schema's host vocabulary (host or host:port form)
// so the allowlist and the matcher agree on casing.
func normalizeAllowedHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(strings.ToLower(h))
		if h == "" {
			continue
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
	hb := HostBinding{HostPattern: hp, CredentialRef: credentialRef, Scheme: scheme, EmitMechanism: EmitMechanismInject}
	for _, opt := range opts {
		opt(&hb)
	}
	if scheme == SchemeBasic && hb.BasicUsername == "" {
		return HostBinding{}, fmt.Errorf("host binding: basic scheme requires a username (WithBasicUsername)")
	}
	if scheme == SchemeHeaderTemplate && (hb.HeaderName == "" || hb.HeaderTemplate == "") {
		return HostBinding{}, fmt.Errorf("host binding: header-template scheme requires a header name and template (WithHeaderTemplate)")
	}
	if scheme == SchemeQueryParam && hb.QueryParamName == "" {
		return HostBinding{}, fmt.Errorf("host binding: query-param scheme requires a param name (WithQueryParam)")
	}
	if scheme == SchemeSigV4Resign && (hb.AccessKeyID == "" || hb.Region == "" || hb.Service == "") {
		return HostBinding{}, fmt.Errorf("host binding: sigv4-resign scheme requires access key id, region, and service (WithSigV4Resign)")
	}
	if hb.EmitMechanism == EmitMechanismSentinelSwap && (hb.SentinelValue == "" || hb.SentinelEnv == "") {
		return HostBinding{}, fmt.Errorf("host binding: emit-mechanism sentinel-swap requires a sentinel value and env (WithSentinel)")
	}
	if hb.EmitMechanism == EmitMechanismInject && (hb.SentinelValue != "" || hb.SentinelEnv != "") {
		return HostBinding{}, fmt.Errorf("host binding: emit-mechanism inject must not carry a sentinel (WithSentinel)")
	}
	if hb.Effect != "" {
		if _, ok := HostBindingEffects[hb.Effect]; !ok {
			return HostBinding{}, fmt.Errorf("host binding: unknown trust-contract effect %q", hb.Effect)
		}
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
