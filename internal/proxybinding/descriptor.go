// Package proxybinding parses and loads declarative host->credential
// binding descriptors and converts them into the binding-table entries
// the TLS forward-proxy boundary consults (ADR-0019, umbrella #1191).
//
// A descriptor is the "config, not code" half of the credential-sealing
// substrate. The binding table (#1193, internal/binding.HostBindings) and
// the scheme-keyed injector (#1194, internal/credential/inject) are the
// "code" half: they consult a host->credential mapping at egress and bind
// the resolved secret onto the outbound request. This package supplies
// the missing declarative format so a CLI vendor or community profile can
// ship a descriptor that flows generically through the loader into the
// table, with zero per-CLI proxy code. The proving example is Linear
// (api.linear.app, header-template emitting a verbatim
// "Authorization: <token>" with no Bearer prefix); nothing in the proxy
// path branches on "linear" or any CLI name.
//
// # Secret handling
//
// A descriptor carries only a credential *reference* (a vault path), never
// the credential bytes. This package never reads, resolves, or logs secret
// material: resolution happens daemon-side at injection time (#1193's
// responsibility). The descriptor format is therefore safe to embed,
// commit, and ship.
//
// # Scope
//
// This package defines the descriptor format and the two-layer loader.
// It does not implement the binding-table consult (#1193) or the injectors
// (#1194). It accepts both emit mechanisms: "inject" (inject at the proxy)
// and "sentinel-swap". A sentinel-swap binding declares a non-secret
// `sentinel` block (a placeholder value plus the env-var name the launcher
// plants it under); this loader validates that schema and the egress code
// consumes those fields. Persisting a stateful CLI's local cache across
// ephemeral sandboxes is out of scope and tracked separately (#1190).
//
// [ADR-0019]: https://docs.withaileron.ai/adr/0019-v4-https-data-plane
package proxybinding

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/credential/inject"
)

// placeholderPattern matches an angle-bracket span (e.g. "<AccessKeyId>",
// "<token>", "<your-region>"). A descriptor author who copies a template
// verbatim and leaves an un-substituted placeholder in a parsed value ships
// a binding that authenticates nothing: at launch time the credential-sealing
// path emits the literal placeholder upstream and the request fails with an
// opaque provider error (the reported case was a sigv4-resign entry with
// access_key_id "<AccessKeyId>" surfacing as UnrecognizedClientException).
// Any string field carrying such a span is a load-time error so the daemon
// fails loudly at boot rather than at launch. Descriptor comments are not
// parsed values, so a "<...>" in a YAML comment is never scanned; the
// header-template "{token}" slot uses braces, not angle brackets, so it is
// unaffected.
var placeholderPattern = regexp.MustCompile(`<[^>]+>`)

// sigv4AccessKeyIDPattern is the canonical shape of an AWS access key ID used
// by the sigv4-resign scheme: an "AKIA" (long-term) or "ASIA" (temporary/STS)
// prefix followed by 16 uppercase-alphanumeric characters. A well-formed
// descriptor whose access_key_id does not match this shape still loads (the
// value is non-secret and AWS may evolve the format), but it is surfaced as a
// non-fatal startup warning so a typo'd or placeholder-shaped key is visible
// before it fails at launch. This is intentionally warn-only: the repository's
// own SigV4 test vector uses the documented "AKIDEXAMPLE" key, which must keep
// loading clean.
var sigv4AccessKeyIDPattern = regexp.MustCompile(`^(AKIA|ASIA)[A-Z0-9]{16}$`)

// SchemaVersion is the only descriptor schema version this loader
// understands. The format is versioned so it can evolve under 0.0.x
// without a silent misparse: a descriptor that names any other version is
// a load-time error rather than a best-effort decode against the wrong
// field set.
const SchemaVersion = "v1"

// EmitMechanisms is the closed set of emit-mechanism values a descriptor
// may declare at load time. "inject" adds the credential at the proxy and
// covers two client shapes: a request that arrives with no credential (the
// proxy adds one), and a self-signing client such as `aws`/botocore that
// emits a placeholder-signed request the `sigv4-resign` scheme strips and
// re-signs with the vault credential at the boundary. "sentinel-swap" plants a
// non-secret sentinel the proxy swaps for the real credential at egress;
// a sentinel-swap binding must declare a `sentinel` block naming the
// placeholder value and the env-var name the launcher plants it under. A
// value outside this set (e.g. "C") is a load-time error, so a descriptor
// never validates against a mechanism the proxy cannot enforce.
var EmitMechanisms = map[string]struct{}{
	string(binding.EmitMechanismInject):       {},
	string(binding.EmitMechanismSentinelSwap): {},
}

// Descriptor is a parsed, versioned binding-descriptor document. One
// document carries a version and an ordered list of per-host entries.
// A CLI vendor or community profile ships a Descriptor; the loader merges
// the built-in and user layers into a single validated set.
type Descriptor struct {
	// Version is the schema version. It must equal [SchemaVersion]; any
	// other value is rejected at parse time so the format can evolve under
	// 0.0.x without a silent misparse.
	Version string `yaml:"version"`

	// Bindings is the ordered list of per-host binding entries in this
	// document. Within a single document, host keys must be unique; across
	// layers, a later layer's entry for a host overrides an earlier one
	// (see [Load]).
	Bindings []Entry `yaml:"bindings"`
}

// Entry is a single declarative host->credential binding: the {host,
// credential-ref, scheme, emit-mechanism} quad plus the scheme-specific
// non-secret params. It is a self-contained value type so this package is
// independently testable; [Entry.ToHostBinding] adapts it to the canonical
// internal/binding.HostBinding the proxy table consumes.
type Entry struct {
	// Host is the upstream host pattern matched at the proxy boundary. It
	// is an exact host ("api.linear.app") or a single leading-wildcard
	// form ("*.example.com"), mirroring internal/binding.HostBinding's
	// match semantics rather than inventing a second matcher. Ports are
	// not part of the pattern.
	//
	// Host is optional when the entry declares a complete credential
	// identity ([Entry.Kind] + [Entry.IdentityLabel]): such an identity
	// binding is selected at egress by its (kind, label) pair, not by host
	// (#1978). An entry with neither a host nor a complete identity is a
	// load-time error.
	Host string `yaml:"host,omitempty"`

	// Kind and IdentityLabel are the non-secret manifest credential-identity
	// pair (#1978): the credential `kind` (e.g. "aws-sigv4") and the manifest
	// `identity_label` naming which credential of that kind to inject. When
	// both are set, the entry is a host-less-permitted identity binding the
	// proxy selects by identity at egress. The pair is canonical: declare both
	// or neither. A half-identity (exactly one set) is a load-time error. Maps
	// onto internal/binding.HostBinding via [binding.WithIdentity].
	Kind          string `yaml:"kind,omitempty"`
	IdentityLabel string `yaml:"identity_label,omitempty"`

	// CredentialRef is a vault credential reference resolved daemon-side
	// at injection time, never to the container. It is a connector-style
	// binding name ("<kind>/<service>/<identity>") or a user-level ref
	// ("user/<service>"), the same name contract internal/binding
	// enforces. It is never the credential bytes.
	CredentialRef string `yaml:"credential_ref,omitempty"`

	// Scheme is one of the closed injection-scheme set (#1194):
	// bearer | basic | header-template | query-param | sigv4-resign. An
	// unknown scheme is a load-time error (fail closed, no silent skip).
	Scheme string `yaml:"scheme,omitempty"`

	// EmitMechanism declares how the credential reaches egress: "inject"
	// (inject at the proxy) or "sentinel-swap". Optional; empty defaults to
	// "inject". A sentinel-swap binding must declare a non-empty
	// [Entry.Sentinel] block; an inject binding (explicit or defaulted)
	// must declare none. Any value outside the closed set is a load-time
	// error.
	EmitMechanism string `yaml:"emit_mechanism,omitempty"`

	// Sentinel is the sentinel-swap placeholder declaration: the non-secret
	// value the launcher plants inside the container and the env-var name it
	// plants under. It is required and non-empty for "sentinel-swap" and
	// forbidden for "inject" (a stray sentinel on an inject binding is a
	// load-time error, since the field is meaningless without sentinel-swap).
	// It is a pointer so an absent block is distinguishable from a present
	// block with empty fields. See [Sentinel].
	Sentinel *Sentinel `yaml:"sentinel,omitempty"`

	// Username is the non-secret HTTP basic-auth username, required only
	// for the basic scheme (e.g. "x-access-token" for git-over-HTTPS).
	Username string `yaml:"username,omitempty"`

	// Header is the header name to set, required only for the
	// header-template scheme (e.g. "Authorization" or a vendor header).
	Header string `yaml:"header,omitempty"`

	// Template is the verbatim header value for the header-template
	// scheme, with the "{token}" placeholder substituted with the secret
	// at inject time. Required for header-template. To emit Linear's
	// verbatim "Authorization: <key>" with no Bearer prefix, set
	// Template to "{token}".
	Template string `yaml:"template,omitempty"`

	// QueryParam is the query-parameter name to set, required only for the
	// query-param scheme.
	QueryParam string `yaml:"query_param,omitempty"`

	// AccessKeyID is the non-secret AWS access key ID, required only for the
	// sigv4-resign scheme. It appears verbatim in the signed request's
	// Credential= field; the secret access key travels in the resolved
	// credential value, never here. A sigv4-resign entry carries no region or
	// service: the egress injector derives the SigV4 credential scope from the
	// resolved upstream host (#1978), so there is no second, operator-supplied
	// copy of the region that could drift from the host being signed for.
	AccessKeyID string `yaml:"access_key_id,omitempty"`

	// AllowedHosts is the optional per-binding trust-contract host
	// allowlist. Empty means unconstrained: egress on the bound host stays
	// scoped only to [Entry.Host], exactly as before. A non-empty list gates
	// egress at the proxy injection point (#1735): the upstream host must
	// match an entry (host or host:port form) or the request is denied and
	// audited. Non-secret. Maps onto internal/binding.HostBinding via
	// [binding.WithTrustContract].
	AllowedHosts []string `yaml:"allowed_hosts,omitempty"`

	// Effect is the optional per-binding trust-contract effect, one of
	// internal/binding.HostBindingEffects (read | write | delete | spend |
	// external-send), mirroring the runtime's effect vocabulary. Empty means
	// no effect gate (unconstrained). A `read` effect constrains egress on
	// the bound host to HTTP-safe methods at the proxy (#1735); a write-class
	// effect admits all methods (the proxy cannot distinguish write from
	// delete/spend/external-send on the wire). An unknown effect is a
	// load-time error (fail closed). Non-secret.
	Effect string `yaml:"effect,omitempty"`
}

// Sentinel is the per-binding sentinel-swap placeholder declaration. It makes
// both the placeholder value and its plant location data rather than Go
// constants, so a token-in-env CLI is expressible purely by a descriptor.
//
// Every field is non-secret. The Value carries no authority: presenting it
// upstream authenticates nothing, so it is safe to embed in source, commit,
// and print in logs. The launcher plants Value under the Env env-var name
// inside the container so the CLI's local validation passes and it issues
// the request; the proxy then recognizes Value at egress and swaps in the
// real credential. The placeholder bytes never reach upstream.
type Sentinel struct {
	// Value is the non-secret, format-mimicking placeholder the launcher
	// plants and the proxy recognizes (e.g.
	// "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"). It is required and
	// non-empty for "sentinel-swap". It is non-secret and safe to commit.
	Value string `yaml:"value"`

	// Env is the environment-variable name the launcher sets to [Sentinel.Value]
	// inside the container (e.g. "GH_TOKEN"). It is required and non-empty for
	// "sentinel-swap". It generalizes the plant target so the launcher is not
	// hardcoded to a single CLI's env var.
	Env string `yaml:"env"`
}

// TokenPlaceholder is the substring a header-template's Template
// substitutes with the resolved secret at inject time. It mirrors the
// placeholder the injector (#1194) uses, so a descriptor author and the
// egress injector agree on the token slot.
const TokenPlaceholder = "{token}"

// Parse strictly decodes a single descriptor document and validates it.
// Decoding is strict: an unknown YAML key is an error rather than a
// silently ignored field, so a typo in a descriptor fails fast instead of
// shipping a binding that does nothing. Malformed YAML, a wrong or missing
// version, and any entry that fails [Entry.Validate] are all errors.
//
// Parse never reads secret bytes; a descriptor carries only a credential
// reference.
func Parse(data []byte) (Descriptor, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var d Descriptor
	if err := dec.Decode(&d); err != nil {
		return Descriptor{}, fmt.Errorf("proxybinding: parse descriptor: %w", err)
	}

	if d.Version != SchemaVersion {
		return Descriptor{}, fmt.Errorf("proxybinding: unsupported descriptor version %q (want %q)", d.Version, SchemaVersion)
	}

	seen := make(map[string]struct{}, len(d.Bindings))
	for i := range d.Bindings {
		e := &d.Bindings[i]
		if err := e.Validate(); err != nil {
			return Descriptor{}, fmt.Errorf("proxybinding: binding %d (%s): %w", i, e.dedupLabel(), err)
		}
		key := e.dedupKey()
		if _, dup := seen[key]; dup {
			return Descriptor{}, fmt.Errorf("proxybinding: duplicate binding %s within a single descriptor", e.dedupLabel())
		}
		seen[key] = struct{}{}
	}

	return d, nil
}

// dedupKey is the uniqueness/override key for an entry. A host entry keys on
// its lowercased host; a host-less identity entry keys on its (kind, label)
// pair. Keying host-less entries on the identity pair (not on host == "")
// lets two identity bindings with different labels coexist without a spurious
// `duplicate host ""` collision, and lets a later layer override an earlier
// identity binding with the same pair (matching the per-host override
// semantics). The two namespaces are prefix-disjoint so a host can never
// collide with an identity. An entry carrying both a host and an identity keys
// on the host: it is still primarily a host binding, and its identity is an
// additional selection key rather than its uniqueness key.
func (e *Entry) dedupKey() string {
	if e.Host != "" {
		return "host:" + strings.ToLower(e.Host)
	}
	return "id:" + e.Kind + "\x00" + e.IdentityLabel
}

// dedupLabel is the human-readable form of the dedup key for error messages.
func (e *Entry) dedupLabel() string {
	if e.Host != "" {
		return fmt.Sprintf("host %q", e.Host)
	}
	return fmt.Sprintf("identity (kind %q, identity_label %q)", e.Kind, e.IdentityLabel)
}

// Validate checks that an Entry's required fields are present and
// internally consistent. It enforces the closed scheme set, the closed
// emit-mechanism set, and the scheme-specific param requirements. It does
// not resolve the credential or touch any secret.
//
// Validation reuses the canonical internal/binding constructor so the
// descriptor format and the binding table agree on host-pattern and
// credential-ref legality: there is exactly one matcher and one name
// contract, not two.
func (e *Entry) Validate() error {
	// Reject un-substituted template placeholders before any other check.
	// An angle-bracket span in a parsed value (e.g. access_key_id
	// "<AccessKeyId>") is never legitimate: it means an author left a
	// copy-paste placeholder in place, so fail closed at boot naming the
	// field rather than shipping a binding that authenticates nothing.
	if err := e.rejectPlaceholders(); err != nil {
		return err
	}

	// An entry is keyed by a host, a credential identity, or both. The
	// identity pair is canonical: a half-identity (exactly one of kind or
	// identity_label set) fails closed, and an entry with neither a host nor
	// a complete identity has no selection key at all. The constructor
	// (ToHostBinding, called at the end of Validate) is the single source of
	// truth, but naming the failure here yields a clearer descriptor error.
	hasIdentity := e.Kind != "" && e.IdentityLabel != ""
	if (e.Kind != "") != (e.IdentityLabel != "") {
		return fmt.Errorf("credential identity requires both kind and identity_label; a half-identity is invalid")
	}
	if e.Host == "" && !hasIdentity {
		return fmt.Errorf("missing required field: host (or a complete credential identity: kind + identity_label)")
	}
	if e.CredentialRef == "" {
		return fmt.Errorf("missing required field: credential_ref")
	}
	if e.Scheme == "" {
		return fmt.Errorf("missing required field: scheme")
	}
	if _, err := inject.ParseScheme(e.Scheme); err != nil {
		return fmt.Errorf("unknown scheme %q (want one of %v)", e.Scheme, schemeNames())
	}

	mech := e.EmitMechanism
	if mech == "" {
		mech = string(binding.EmitMechanismInject)
	}
	if _, ok := EmitMechanisms[mech]; !ok {
		return fmt.Errorf("unknown emit_mechanism %q (want one of %q or %q)", e.EmitMechanism, string(binding.EmitMechanismInject), string(binding.EmitMechanismSentinelSwap))
	}

	// Sentinel-swap requires a non-empty sentinel block (value + env);
	// inject (explicit or defaulted) forbids one. The sentinel names the
	// placeholder the launcher plants and the env var it plants it under,
	// so the field is meaningless without sentinel-swap (fail closed).
	switch mech {
	case string(binding.EmitMechanismSentinelSwap):
		if e.Sentinel == nil {
			return fmt.Errorf("emit_mechanism %q requires a sentinel block", string(binding.EmitMechanismSentinelSwap))
		}
		if e.Sentinel.Value == "" {
			return fmt.Errorf("emit_mechanism %q requires a non-empty sentinel.value", string(binding.EmitMechanismSentinelSwap))
		}
		if e.Sentinel.Env == "" {
			return fmt.Errorf("emit_mechanism %q requires a non-empty sentinel.env", string(binding.EmitMechanismSentinelSwap))
		}
	default:
		if e.Sentinel != nil {
			return fmt.Errorf("emit_mechanism %q must not declare a sentinel block (sentinel applies only to %q)", string(binding.EmitMechanismInject), string(binding.EmitMechanismSentinelSwap))
		}
	}

	switch e.Scheme {
	case string(inject.SchemeBasic):
		if e.Username == "" {
			return fmt.Errorf("basic scheme requires a username")
		}
	case string(inject.SchemeHeaderTemplate):
		if e.Header == "" {
			return fmt.Errorf("header-template scheme requires a header name")
		}
		if e.Template == "" {
			return fmt.Errorf("header-template scheme requires a template")
		}
	case string(inject.SchemeQueryParam):
		if e.QueryParam == "" {
			return fmt.Errorf("query-param scheme requires a query_param name")
		}
	case string(inject.SchemeSigV4Resign):
		// A sigv4-resign entry is identity-only (#1978). It requires the
		// non-derivable access_key_id (it appears verbatim in the signed
		// Credential= field) plus a complete credential identity, and it must
		// not carry a host: the egress injector derives the SigV4 credential
		// scope (region + service) from the resolved upstream host and selects
		// the credential by its (kind, identity_label) pair, so a host on the
		// entry would be the very second copy the umbrella eliminates.
		if e.AccessKeyID == "" {
			return fmt.Errorf("sigv4-resign scheme requires an access_key_id")
		}
		if !hasIdentity {
			return fmt.Errorf("sigv4-resign scheme requires a complete credential identity (kind + identity_label)")
		}
		if e.Host != "" {
			return fmt.Errorf("sigv4-resign scheme must not carry a host; the credential is selected by identity and the SigV4 scope is derived from the resolved upstream host at egress")
		}
	}

	// Reuse the canonical constructor's host-pattern and credential-ref
	// validation. ToHostBinding is the single source of truth for what a
	// well-formed binding looks like; calling it here surfaces a malformed
	// host or credential-ref as a parse error rather than a wiring-time
	// surprise.
	if _, err := e.ToHostBinding(); err != nil {
		return err
	}
	return nil
}

// rejectPlaceholders scans every string field of the Entry for an
// angle-bracket span and returns a field-named error on the first match. It
// is the load-time guard against un-substituted template placeholders. Parse
// wraps the returned error with the binding index and host, and
// Load/parseLayerFile wrap it with the user file path, so the operator sees
// file + entry + field. Slice fields (allowed_hosts) are scanned per element
// with the index named.
func (e *Entry) rejectPlaceholders() error {
	type namedField struct {
		name  string
		value string
	}
	fields := []namedField{
		{"host", e.Host},
		{"kind", e.Kind},
		{"identity_label", e.IdentityLabel},
		{"credential_ref", e.CredentialRef},
		{"scheme", e.Scheme},
		{"emit_mechanism", e.EmitMechanism},
		{"username", e.Username},
		{"header", e.Header},
		{"template", e.Template},
		{"query_param", e.QueryParam},
		{"access_key_id", e.AccessKeyID},
		{"effect", e.Effect},
	}
	if e.Sentinel != nil {
		fields = append(fields,
			namedField{"sentinel.value", e.Sentinel.Value},
			namedField{"sentinel.env", e.Sentinel.Env},
		)
	}
	for i, h := range e.AllowedHosts {
		fields = append(fields, namedField{fmt.Sprintf("allowed_hosts[%d]", i), h})
	}
	for _, f := range fields {
		if match := placeholderPattern.FindString(f.value); match != "" {
			return fmt.Errorf("field %s contains an un-substituted template placeholder %q; replace it with a real value", f.name, match)
		}
	}
	return nil
}

// Warnings returns non-fatal advisories about a well-formed Entry that loads
// successfully but is suspect. Unlike [Entry.Validate], a warning never blocks
// boot: it is logged at daemon startup so an operator sees a likely mistake
// before it fails at launch. The strings name the offending field and value so
// the aggregating caller can prefix them with the file and entry context.
//
// Today the only warning is a sigv4-resign access_key_id that does not match
// the canonical AWS shape ([sigv4AccessKeyIDPattern]). This is warn-only by
// design: the value is non-secret, the AWS format may evolve, and the
// documented "AKIDEXAMPLE" test vector must keep loading clean, so a
// shape mismatch is surfaced without failing construction.
func (e *Entry) Warnings() []string {
	var out []string
	if e.Scheme == string(inject.SchemeSigV4Resign) &&
		e.AccessKeyID != "" &&
		!sigv4AccessKeyIDPattern.MatchString(e.AccessKeyID) {
		out = append(out, fmt.Sprintf(
			"binding %s: sigv4-resign access_key_id %q does not match the expected AWS access key ID shape (AKIA/ASIA + 16 uppercase alphanumerics); verify it is correct",
			e.dedupLabel(), e.AccessKeyID))
	}
	return out
}

// schemeNames returns the closed scheme set as plain strings, for error
// messages. It tracks inject.AllSchemes so a new scheme appears in the
// message without a second hand-maintained list.
func schemeNames() []string {
	all := inject.AllSchemes()
	out := make([]string, len(all))
	for i, s := range all {
		out[i] = string(s)
	}
	return out
}
