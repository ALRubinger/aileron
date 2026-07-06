package proxybinding

import (
	"fmt"

	"github.com/ALRubinger/aileron/internal/binding"
)

// ToHostBinding adapts a descriptor Entry into the canonical
// internal/binding.HostBinding the proxy table (#1193) consumes. It maps
// the declared scheme and emit-mechanism plus the scheme-specific
// non-secret params onto the constructor's options.
//
// The constructor is the single source of truth for binding legality:
// host-pattern form and credential-ref name contract are validated there,
// so the descriptor format and the binding table can never disagree about
// what a well-formed binding is. ToHostBinding carries no secret bytes; a
// HostBinding, like an Entry, names where the credential lives, never its
// value.
func (e *Entry) ToHostBinding() (binding.HostBinding, error) {
	var opts []binding.HostBindingOption

	switch e.Scheme {
	case binding.SchemeBasic:
		opts = append(opts, binding.WithBasicUsername(e.Username))
	case binding.SchemeHeaderTemplate:
		opts = append(opts, binding.WithHeaderTemplate(e.Header, e.Template))
	case binding.SchemeQueryParam:
		opts = append(opts, binding.WithQueryParam(e.QueryParam))
	case binding.SchemeSigV4Resign:
		opts = append(opts, binding.WithSigV4Resign(e.AccessKeyID, e.Region, e.Service))
	}

	// Carry the optional per-binding trust scope (#1735). Empty effect and
	// empty allowed_hosts yield an unconstrained binding (today's behavior);
	// a non-empty effect is validated against the closed set by the
	// constructor below, the single source of truth for effect legality.
	if e.Effect != "" || len(e.AllowedHosts) > 0 {
		opts = append(opts, binding.WithTrustContract(e.Effect, e.AllowedHosts))
	}

	// Carry the optional credential identity (#1978). When declared, the
	// binding is selectable by its (kind, identity_label) pair at egress and
	// its host may be empty. The constructor enforces the pair-is-canonical
	// and host-or-identity rules, the single source of truth for legality, so
	// a malformed identity entry that reached here fails construction below.
	if e.Kind != "" || e.IdentityLabel != "" {
		opts = append(opts, binding.WithIdentity(e.Kind, e.IdentityLabel))
	}

	if e.EmitMechanism == string(binding.EmitMechanismSentinelSwap) {
		opts = append(opts, binding.WithEmitMechanismSentinelSwap())
		// A sentinel-swap entry carries the sentinel shape (value + env) it
		// validated at parse time. Carry it onto the binding so the launcher
		// plants the value into the env and the proxy recognizes it at
		// egress, both reading one source of truth. The constructor enforces
		// the sentinel-swap-requires-sentinel rule, so a malformed entry that
		// reached here with no sentinel fails construction below rather than
		// silently shipping an unplantable binding.
		if e.Sentinel != nil {
			opts = append(opts, binding.WithSentinel(e.Sentinel.Value, e.Sentinel.Env))
		}
	}

	hb, err := binding.NewHostBinding(e.Host, e.CredentialRef, e.Scheme, opts...)
	if err != nil {
		return binding.HostBinding{}, fmt.Errorf("adapt descriptor entry: %w", err)
	}
	return hb, nil
}

// LoadHostBindings is the convenience the apiServer wiring calls: it loads
// the two merged descriptor layers (built-in -> user) and adapts them to the canonical
// binding table in one step. A load or adaptation error is surfaced rather
// than swallowed so a malformed descriptor fails construction loudly
// instead of silently shipping an empty (passthrough) table.
func LoadHostBindings(opts LoadOptions) (binding.HostBindings, error) {
	table, _, err := LoadHostBindingsWithWarnings(opts)
	return table, err
}

// LoadHostBindingsWithWarnings is LoadHostBindings plus the non-fatal
// startup warnings aggregated from the merged entries (see [Entry.Warnings]).
// Warnings never block construction; the caller (the daemon boot path) logs
// them so an operator sees a suspect-but-well-formed binding before it fails
// at launch. Warnings are collected from the post-merge entry set, so a
// warning reflects the entry that actually reaches the table (a user override
// is warned on, the shadowed built-in is not).
func LoadHostBindingsWithWarnings(opts LoadOptions) (binding.HostBindings, []string, error) {
	entries, err := Load(opts)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	for i := range entries {
		warnings = append(warnings, entries[i].Warnings()...)
	}
	table, err := ToHostBindings(entries)
	if err != nil {
		return nil, nil, err
	}
	return table, warnings, nil
}

// ToHostBindings adapts a slice of validated entries into a
// binding.HostBindings table, preserving order. A nil or empty slice
// yields a nil table, which internal/binding treats as a valid empty
// table whose Match always misses, preserving today's passthrough
// behavior when no descriptors are configured.
func ToHostBindings(entries []Entry) (binding.HostBindings, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(binding.HostBindings, 0, len(entries))
	for i := range entries {
		hb, err := entries[i].ToHostBinding()
		if err != nil {
			return nil, err
		}
		out = append(out, hb)
	}
	return out, nil
}
