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
// (#1194). It accepts both emit mechanisms: mechanism "A" (inject at the
// proxy) and mechanism "B" (sentinel-swap). A mechanism-B binding declares
// a non-secret `sentinel` block (a placeholder value plus the env-var name
// the launcher plants it under); this loader validates that schema while
// the egress code that consumes those fields lands separately (#1247).
// Persisting a stateful CLI's local cache across ephemeral sandboxes is out
// of scope and tracked separately (#1190).
//
// [ADR-0019]: https://docs.withaileron.ai/adr/0019-v4-https-data-plane
package proxybinding

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/credential/inject"
)

// SchemaVersion is the only descriptor schema version this loader
// understands. The format is versioned so it can evolve under 0.0.x
// without a silent misparse: a descriptor that names any other version is
// a load-time error rather than a best-effort decode against the wrong
// field set.
const SchemaVersion = "v1"

// EmitMechanisms is the closed set of emit-mechanism values a descriptor
// may declare at load time. Mechanism "A" injects unconditionally at the
// proxy (the client emits a no-credential request). Mechanism "B" is
// sentinel-swap (the launcher plants a non-secret sentinel the proxy swaps
// for the real credential at egress); a mechanism-B binding must declare a
// `sentinel` block naming the placeholder value and the env-var name the
// launcher plants it under. A value outside this set (e.g. "C") is a
// load-time error, so a descriptor never validates against a mechanism the
// proxy cannot enforce.
var EmitMechanisms = map[string]struct{}{
	string(binding.EmitMechanismA): {},
	string(binding.EmitMechanismB): {},
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
	Host string `yaml:"host"`

	// CredentialRef is a vault credential reference resolved daemon-side
	// at injection time, never to the container. It is a connector-style
	// binding name ("<kind>/<service>/<identity>") or a user-level ref
	// ("user/<service>"), the same name contract internal/binding
	// enforces. It is never the credential bytes.
	CredentialRef string `yaml:"credential_ref"`

	// Scheme is one of the closed injection-scheme set (#1194):
	// bearer | basic | header-template | query-param | sigv4-resign. An
	// unknown scheme is a load-time error (fail closed, no silent skip).
	Scheme string `yaml:"scheme"`

	// EmitMechanism declares how the credential reaches egress: "A"
	// (inject at the proxy) or "B" (sentinel-swap). Optional; empty
	// defaults to "A". A mechanism-B binding must declare a non-empty
	// [Entry.Sentinel] block; a mechanism-A binding (explicit or defaulted)
	// must declare none. Any value outside the closed set is a load-time
	// error.
	EmitMechanism string `yaml:"emit_mechanism"`

	// Sentinel is the mechanism-B placeholder declaration: the non-secret
	// value the launcher plants inside the container and the env-var name it
	// plants under. It is required and non-empty for mechanism "B" and
	// forbidden for mechanism "A" (a stray sentinel on a mechanism-A binding
	// is a load-time error, since the field is meaningless without B). It is
	// a pointer so an absent block is distinguishable from a present block
	// with empty fields. See [Sentinel].
	Sentinel *Sentinel `yaml:"sentinel"`

	// Username is the non-secret HTTP basic-auth username, required only
	// for the basic scheme (e.g. "x-access-token" for git-over-HTTPS).
	Username string `yaml:"username"`

	// Header is the header name to set, required only for the
	// header-template scheme (e.g. "Authorization" or a vendor header).
	Header string `yaml:"header"`

	// Template is the verbatim header value for the header-template
	// scheme, with the "{token}" placeholder substituted with the secret
	// at inject time. Required for header-template. To emit Linear's
	// verbatim "Authorization: <key>" with no Bearer prefix, set
	// Template to "{token}".
	Template string `yaml:"template"`

	// QueryParam is the query-parameter name to set, required only for the
	// query-param scheme.
	QueryParam string `yaml:"query_param"`
}

// Sentinel is the per-binding mechanism-B placeholder declaration. It makes
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
	// non-empty for mechanism "B". It is non-secret and safe to commit.
	Value string `yaml:"value"`

	// Env is the environment-variable name the launcher sets to [Sentinel.Value]
	// inside the container (e.g. "GH_TOKEN"). It is required and non-empty for
	// mechanism "B". It generalizes the plant target so the launcher is not
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
			return Descriptor{}, fmt.Errorf("proxybinding: binding %d (%q): %w", i, e.Host, err)
		}
		if _, dup := seen[e.Host]; dup {
			return Descriptor{}, fmt.Errorf("proxybinding: duplicate host %q within a single descriptor", e.Host)
		}
		seen[e.Host] = struct{}{}
	}

	return d, nil
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
	if e.Host == "" {
		return fmt.Errorf("missing required field: host")
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
		mech = string(binding.EmitMechanismA)
	}
	if _, ok := EmitMechanisms[mech]; !ok {
		return fmt.Errorf("unknown emit_mechanism %q (want one of %q or %q)", e.EmitMechanism, string(binding.EmitMechanismA), string(binding.EmitMechanismB))
	}

	// Mechanism B requires a non-empty sentinel block (value + env);
	// mechanism A (explicit or defaulted) forbids one. The sentinel names
	// the placeholder the launcher plants and the env var it plants it
	// under, so the field is meaningless without B (fail closed).
	switch mech {
	case string(binding.EmitMechanismB):
		if e.Sentinel == nil {
			return fmt.Errorf("emit_mechanism %q requires a sentinel block", string(binding.EmitMechanismB))
		}
		if e.Sentinel.Value == "" {
			return fmt.Errorf("emit_mechanism %q requires a non-empty sentinel.value", string(binding.EmitMechanismB))
		}
		if e.Sentinel.Env == "" {
			return fmt.Errorf("emit_mechanism %q requires a non-empty sentinel.env", string(binding.EmitMechanismB))
		}
	default:
		if e.Sentinel != nil {
			return fmt.Errorf("emit_mechanism %q must not declare a sentinel block (sentinel applies only to mechanism %q)", string(binding.EmitMechanismA), string(binding.EmitMechanismB))
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
