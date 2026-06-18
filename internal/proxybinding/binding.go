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
	}

	if e.EmitMechanism == string(binding.EmitMechanismB) {
		opts = append(opts, binding.WithEmitMechanismB())
	}

	hb, err := binding.NewHostBinding(e.Host, e.CredentialRef, e.Scheme, opts...)
	if err != nil {
		return binding.HostBinding{}, fmt.Errorf("adapt descriptor entry: %w", err)
	}
	return hb, nil
}

// LoadHostBindings is the convenience the apiServer wiring calls: it loads
// the three merged descriptor layers and adapts them to the canonical
// binding table in one step. A load or adaptation error is surfaced rather
// than swallowed so a malformed descriptor fails construction loudly
// instead of silently shipping an empty (passthrough) table.
func LoadHostBindings(opts LoadOptions) (binding.HostBindings, error) {
	entries, err := Load(opts)
	if err != nil {
		return nil, err
	}
	return ToHostBindings(entries)
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
