package cli

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// Parse strictly decodes a single CLI-capability unit and validates it.
// Decoding is strict: an unknown YAML key is an error rather than a silently
// ignored field, mirroring both source packages so a typo in a unit fails fast
// instead of shipping a capability that does nothing. Malformed YAML and any
// field that fails [Unit.Validate] are errors.
//
// Parse never reads secret bytes. A unit carries only non-secret acquisition
// knowledge and credential references.
func Parse(data []byte) (Unit, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var u Unit
	if err := dec.Decode(&u); err != nil {
		return Unit{}, fmt.Errorf("cli: parse unit: %w", err)
	}

	if err := u.Validate(); err != nil {
		return Unit{}, fmt.Errorf("cli: unit %q: %w", u.Name, err)
	}

	return u, nil
}

// Validate checks a unit's envelope rules, then delegates acquisition and
// sealing validation to the canonical capture and proxybinding validators. It
// enforces the required name and well-formed key, rejects any re-declaration
// of a key-derived field, rejects the reserved planes this umbrella does not
// implement, and surfaces the delegated validators' errors verbatim under a
// cli-scoped prefix.
//
// Validate never re-implements scheme, host, sentinel, or arg-vector
// semantics. Those live in the source packages and are reached through the
// conversion adapters.
func (u *Unit) Validate() error {
	if u.Name == "" {
		return fmt.Errorf("missing required field: name")
	}
	if u.Key == "" {
		return fmt.Errorf("missing required field: key")
	}
	kind, ok := deriveKind(u.Key)
	if !ok {
		return fmt.Errorf("key %q is malformed: want \"<kind>/<service>\" with a non-empty first segment", u.Key)
	}

	// The key is the single source of store_at, kind, and every sealing
	// credential_ref. A unit that re-declares any derived field (even equal to
	// the derived value) is an error so the key stays the one source of truth.
	if u.StoreAt != "" {
		return fmt.Errorf("store_at must not be declared: it is derived from key %q (got store_at %q)", u.Key, u.StoreAt)
	}
	if u.CredentialRef != "" {
		return fmt.Errorf("credential_ref must not be declared: it is derived from key %q (got credential_ref %q)", u.Key, u.CredentialRef)
	}
	if u.Kind != "" {
		return fmt.Errorf("kind must not be declared: it is derived from key %q (got kind %q)", u.Key, u.Kind)
	}

	// Reserved planes: forward-declared this umbrella, not implemented. Reject
	// explicitly so a unit that names them fails loudly rather than silently
	// doing nothing.
	if u.Acquisition.Mode == AcquisitionModeDirectToken {
		return fmt.Errorf("acquisition.mode %q is not yet implemented in this umbrella", AcquisitionModeDirectToken)
	}
	if len(u.State) > 0 {
		return fmt.Errorf("state is reserved and not yet implemented in this umbrella")
	}

	// Delegate acquisition validation to capture's canonical validator.
	desc, err := u.toCaptureDescriptor(kind)
	if err != nil {
		return err
	}
	if err := desc.Validate(); err != nil {
		return fmt.Errorf("acquisition: %w", err)
	}

	// Delegate sealing validation to proxybinding's canonical validator, one
	// entry at a time. A sealing entry that re-declares credential_ref is
	// caught by the per-binding guard inside the conversion.
	entries, err := u.toSealingEntries()
	if err != nil {
		return err
	}
	for i := range entries {
		if err := entries[i].Validate(); err != nil {
			return fmt.Errorf("sealing[%d] (%q): %w", i, entries[i].Host, err)
		}
	}

	return nil
}

// deriveKind returns the kind stamp derived from a key, the first
// slash-delimited segment (e.g. "user/github" -> "user"). It reports false
// when the key has no non-empty first segment, so a malformed key is a
// validation error rather than an empty kind.
func deriveKind(key string) (string, bool) {
	idx := strings.IndexByte(key, '/')
	if idx <= 0 {
		return "", false
	}
	return key[:idx], true
}

// ToCaptureDescriptor converts the unit's acquisition block into the canonical
// capture.CaptureDescriptor, deriving store_at and kind from the unit's key.
// It validates the envelope (a well-formed key) before converting so a caller
// receives either a valid-shaped descriptor or an error, never a half-derived
// one. The returned descriptor is the single source the capture driver
// consumes; this package never re-implements its field semantics.
func (u *Unit) ToCaptureDescriptor() (capture.CaptureDescriptor, error) {
	if u.Key == "" {
		return capture.CaptureDescriptor{}, fmt.Errorf("missing required field: key")
	}
	kind, ok := deriveKind(u.Key)
	if !ok {
		return capture.CaptureDescriptor{}, fmt.Errorf("key %q is malformed: want \"<kind>/<service>\" with a non-empty first segment", u.Key)
	}
	return u.toCaptureDescriptor(kind)
}

// toCaptureDescriptor builds the descriptor from a pre-derived kind. It is the
// internal helper Validate and ToCaptureDescriptor share so the field mapping
// lives in one place.
func (u *Unit) toCaptureDescriptor(kind string) (capture.CaptureDescriptor, error) {
	return capture.CaptureDescriptor{
		Version:       capture.CaptureSchemaVersion,
		Name:          u.Name,
		Image:         u.Acquisition.Image,
		ContainerName: u.Acquisition.ContainerName,
		LoginCmd:      u.Acquisition.LoginCmd,
		TokenCmd:      u.Acquisition.TokenCmd,
		BrowserShim:   u.Acquisition.BrowserShim,
		ConfigDir:     u.Acquisition.ConfigDir,
		StoreAt:       u.Key,
		Kind:          kind,
	}, nil
}

// ToSealingEntries converts the unit's sealing block into canonical
// proxybinding.Entry values, deriving each entry's credential_ref from the
// unit's key. A sealing entry that re-declares credential_ref is rejected so
// the key stays the single source. The returned entries are validated by the
// caller (or by Validate) through proxybinding's canonical Entry.Validate;
// this package never re-implements scheme, host, or sentinel semantics.
func (u *Unit) ToSealingEntries() ([]proxybinding.Entry, error) {
	if u.Key == "" {
		return nil, fmt.Errorf("missing required field: key")
	}
	if _, ok := deriveKind(u.Key); !ok {
		return nil, fmt.Errorf("key %q is malformed: want \"<kind>/<service>\" with a non-empty first segment", u.Key)
	}
	return u.toSealingEntries()
}

// toSealingEntries is the internal helper that maps each Binding onto a
// proxybinding.Entry. It enforces the no-re-declaration rule on each entry's
// credential_ref and copies the sentinel block when present.
func (u *Unit) toSealingEntries() ([]proxybinding.Entry, error) {
	if len(u.Sealing) == 0 {
		return nil, nil
	}
	entries := make([]proxybinding.Entry, 0, len(u.Sealing))
	for i := range u.Sealing {
		b := &u.Sealing[i]
		if b.CredentialRef != "" {
			return nil, fmt.Errorf("sealing[%d] (%q): credential_ref must not be declared: it is derived from key %q (got credential_ref %q)", i, b.Host, u.Key, b.CredentialRef)
		}
		var sentinel *proxybinding.Sentinel
		if b.Sentinel != nil {
			sentinel = &proxybinding.Sentinel{
				Value: b.Sentinel.Value,
				Env:   b.Sentinel.Env,
			}
		}
		entries = append(entries, proxybinding.Entry{
			Host:          b.Host,
			CredentialRef: u.Key,
			Scheme:        b.Scheme,
			EmitMechanism: b.EmitMechanism,
			Sentinel:      sentinel,
			Username:      b.Username,
			Header:        b.Header,
			Template:      b.Template,
			QueryParam:    b.QueryParam,
		})
	}
	return entries, nil
}
