// Package cli defines the declarative CLI-capability unit that lives under a
// devcontainer Feature's customizations.aileron.cli block (umbrella #1319).
// A unit declares one CLI tool's complete credential story as data: how the
// credential is acquired and where each outbound host's credential is sealed
// at the proxy boundary.
//
// A unit is an envelope, not a new credential format. The acquisition block
// is the wire shape of internal/auth/capture's CaptureDescriptor, and each
// sealing entry is the wire shape of internal/proxybinding's Entry. This
// package owns only the envelope concerns the unit adds on top of those two
// formats. It owns the unit shape, the single-source derivation of vault keys
// from one key field, and the rejection of the reserved planes this umbrella
// forward-declares but does not yet implement.
//
// Field semantics are not duplicated here. The meaning of a login arg vector,
// a sealing scheme, a sentinel block, or a host pattern lives in the source
// packages. This package converts a unit into a capture.CaptureDescriptor and
// a slice of proxybinding.Entry, then delegates validation to the canonical
// capture.(*CaptureDescriptor).Validate and proxybinding.(*Entry).Validate.
// There is one matcher, one name contract, and one scheme set, and they live
// where they always have.
//
// # Scope
//
// This package ships the type, the parser, the validator, and the two
// conversion adapters. It does not load, merge, or embed any defaults, and it
// adds no daemon command. Loader wiring is tracked separately (#1322).
package cli

// AcquisitionModeDeviceFlow is the only acquisition mode this umbrella
// implements. A device-flow unit drives an interactive container login and
// reads the token back out, mapping field-for-field onto capture's driver.
const AcquisitionModeDeviceFlow = "device-flow"

// AcquisitionModeDirectToken is the forward-declared mode for units that
// supply a token directly rather than acquiring one through an interactive
// login. It is reserved for this umbrella. A unit naming it fails validation with
// an explicit not-yet-implemented error rather than a silent best-effort
// decode.
const AcquisitionModeDirectToken = "direct-token"

// Unit is the YAML-facing CLI-capability unit parsed from a Feature's
// customizations.aileron.cli block. One unit declares a single CLI tool's
// name, its one vault key, how the credential is acquired, and how each
// outbound host seals it. The unit derives every per-store vault key from the
// single Key field so a tool's credential identity is declared once.
type Unit struct {
	// Name is the tool identifier (e.g. "gh"). Required and non-empty. It is
	// the registry key the acquisition descriptor is keyed by.
	Name string `yaml:"name"`

	// Key is the single vault key the unit's credential lives at, in
	// "<kind>/<service>" form (e.g. "user/github"). Required. It is the single
	// source for the acquisition store_at, the acquisition kind (the first
	// path segment), and every sealing entry's credential_ref. The unit must
	// not re-declare any of those derived fields; doing so is a load error.
	Key string `yaml:"key"`

	// Presence is the reserved built-in-presence declaration (e.g.
	// {builtin: base}). It is accepted and validated for well-formedness but
	// no behavior branches on it this umbrella. It is a pointer so an absent
	// block is distinguishable from a present one.
	Presence *Presence `yaml:"presence"`

	// Acquisition is the credential-acquisition block. Its fields mirror
	// capture.CaptureDescriptor one-to-one so conversion is a field copy.
	// Required.
	Acquisition Acquisition `yaml:"acquisition"`

	// Sealing is the ordered list of per-host credential-sealing bindings.
	// Each entry mirrors proxybinding.Entry's wire shape, minus the
	// credential_ref the unit derives from Key. Optional; an empty list is a
	// valid unit that acquires a credential but seals no host.
	Sealing []Binding `yaml:"sealing"`

	// State is the reserved cross-sandbox state block. It is typed as a map so
	// an absent or empty block is distinguishable from a non-empty one. An
	// absent or empty State is valid; a non-empty State fails validation with
	// an explicit not-yet-implemented error this umbrella.
	State map[string]any `yaml:"state"`

	// StoreAt, CredentialRef, and Kind are the derived fields. The unit must
	// not declare them: they are the single-source derivation from Key, and an
	// explicit re-declaration (even one equal to the derived value) is a load
	// error so Key stays the one source of truth. They carry the unit's YAML
	// tags so a re-declaration is captured by the strict decoder rather than
	// rejected as an unknown field.
	StoreAt       string `yaml:"store_at"`
	CredentialRef string `yaml:"credential_ref"`
	Kind          string `yaml:"kind"`
}

// Presence is the reserved built-in-presence declaration. It names whether a
// tool is present in a built-in image tier. It is accepted as data this
// umbrella with no behavior branching on it.
type Presence struct {
	// Builtin names the built-in image tier the tool is present in (e.g.
	// "base"). Reserved; no behavior branches on it this umbrella.
	Builtin string `yaml:"builtin"`
}

// Acquisition is the credential-acquisition block of a unit. Its fields mirror
// internal/auth/capture's CaptureDescriptor one-to-one (minus the version,
// store_at, and kind the envelope supplies) so the unit converts to a
// CaptureDescriptor by field copy and delegates validation to capture.
type Acquisition struct {
	// Mode is the acquisition mode, one of the AcquisitionMode constants.
	// Required. Only device-flow is implemented this umbrella; direct-token is
	// forward-declared and rejected.
	Mode string `yaml:"mode"`

	// LoginCmd is the interactive login command run inside the container,
	// minus the exec scaffolding. Maps to CaptureDescriptor.LoginCmd.
	LoginCmd []string `yaml:"login_cmd"`

	// TokenCmd is the token-read command run inside the same container. Maps
	// to CaptureDescriptor.TokenCmd.
	TokenCmd []string `yaml:"token_cmd"`

	// BrowserShim, when non-empty, is the BROWSER shim planted on the login
	// exec (e.g. "echo"). Optional. Maps to CaptureDescriptor.BrowserShim.
	BrowserShim string `yaml:"browser_shim"`

	// ConfigDir, when non-empty, is the single K=V config-dir env token
	// planted on both execs. Optional; gh omits it. Maps to
	// CaptureDescriptor.ConfigDir.
	ConfigDir string `yaml:"config_dir"`

	// Image is an optional pre-resolved container image. Normally empty. Maps
	// to CaptureDescriptor.Image.
	Image string `yaml:"image"`

	// ContainerName is the deterministic name for the long-lived capture
	// container (e.g. "aileron-auth-github"). Required for device-flow. Maps
	// to CaptureDescriptor.ContainerName.
	ContainerName string `yaml:"container_name"`
}

// Binding is one per-host sealing entry of a unit. Its fields mirror
// internal/proxybinding's Entry wire shape, minus the credential_ref the unit
// derives from Key. A unit converts each Binding into a proxybinding.Entry and
// delegates validation to proxybinding so the scheme set, sentinel rules, and
// host-pattern legality live in one place.
type Binding struct {
	// Host is the upstream host pattern matched at the proxy boundary. Maps to
	// proxybinding.Entry.Host.
	Host string `yaml:"host"`

	// Scheme is the injection scheme. Maps to proxybinding.Entry.Scheme.
	Scheme string `yaml:"scheme"`

	// EmitMechanism declares how the credential reaches egress. Optional;
	// empty defaults to inject. Maps to proxybinding.Entry.EmitMechanism.
	EmitMechanism string `yaml:"emit_mechanism"`

	// Sentinel is the sentinel-swap placeholder declaration. Maps to
	// proxybinding.Entry.Sentinel.
	Sentinel *SentinelSpec `yaml:"sentinel"`

	// Username is the basic-auth username. Maps to proxybinding.Entry.Username.
	Username string `yaml:"username"`

	// Header is the header name for the header-template scheme. Maps to
	// proxybinding.Entry.Header.
	Header string `yaml:"header"`

	// Template is the verbatim header value for the header-template scheme.
	// Maps to proxybinding.Entry.Template.
	Template string `yaml:"template"`

	// QueryParam is the query-parameter name for the query-param scheme. Maps
	// to proxybinding.Entry.QueryParam.
	QueryParam string `yaml:"query_param"`

	// CredentialRef is the derived field. The unit must not declare it: it is
	// derived from Key, and an explicit re-declaration is a load error so Key
	// stays the single source. It carries the proxybinding YAML tag so a
	// re-declaration is captured by the strict decoder rather than rejected as
	// an unknown field.
	CredentialRef string `yaml:"credential_ref"`
}

// SentinelSpec is a unit's sentinel-swap placeholder declaration. It mirrors
// internal/proxybinding's Sentinel: a non-secret placeholder value plus the
// env-var name the launcher plants it under. Every field is non-secret.
type SentinelSpec struct {
	// Value is the non-secret, format-mimicking placeholder. Maps to
	// proxybinding.Sentinel.Value.
	Value string `yaml:"value"`

	// Env is the env-var name the launcher plants the value under. Maps to
	// proxybinding.Sentinel.Env.
	Env string `yaml:"env"`
}
