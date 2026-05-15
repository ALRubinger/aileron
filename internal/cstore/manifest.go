// Package cstore implements Aileron's content-addressed connector store and
// the install pipeline that populates it.
//
// The store layout, hash semantics, FQN resolution, and install pipeline are
// ratified by [ADR-0002] and [ADR-0004]. Connectors are content-addressed by
// SHA-256 of `connector.<binary> || manifest.toml` (signature excluded), live
// at `~/.aileron/store/connectors/sha256/<hash>/`, and are reachable via an
// FQN+version → hash index that is rebuildable from on-disk contents.
//
// [ADR-0002]: https://docs.withaileron.ai/adr/0002-connector-model
// [ADR-0004]: https://docs.withaileron.ai/adr/0004-dependency-resolution
package cstore

import (
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Manifest is the parsed contents of a connector's `manifest.toml`. The
// manifest is the connector's *request* for runtime grants (per ADR-0002).
// The runtime grants nothing not declared here.
type Manifest struct {
	Connector ManifestConnector `toml:"connector"`

	// Capabilities holds the connector's grant requests. Each sub-table is
	// independently optional — a connector that needs no network access
	// simply omits `[capabilities.network]`.
	Capabilities ManifestCapabilities `toml:"capabilities"`

	// Provides advertises the intents this connector implements. Used by
	// the Hub for discovery and by action authors when wiring connectors.
	Provides ManifestProvides `toml:"provides"`
}

// IsIdempotent reports whether the runtime should treat this connector
// as idempotent for retry purposes. Per ADR-0010, idempotency is the
// default; a connector opts out via `[connector.idempotency] default
// = "not_idempotent"`. The retry primitive consults this before
// attempting a retry.
func (m *Manifest) IsIdempotent() bool {
	if m == nil || m.Connector.Idempotency == nil {
		return true
	}
	return m.Connector.Idempotency.Default != IdempotencyNotIdempotent
}

// ManifestConnector is the `[connector]` table — the connector's identity.
type ManifestConnector struct {
	// Name is the fully-qualified URI of this connector
	// (e.g. "github://aileron/slack"). At install time it must match the
	// FQN under which the binary is being fetched.
	Name string `toml:"name"`

	// Version is a strict SemVer string per ADR-0002.
	Version string `toml:"version"`

	// ProvenanceHash is the binary+manifest content hash at publish time
	// (e.g. "sha256:abc123..."). The runtime checks the on-disk bytes
	// against this before every execution.
	ProvenanceHash string `toml:"provenance_hash"`

	// Publisher is the human-readable publisher name shown in the Hub /
	// install consent UI. Not load-bearing; provenance lives in the FQN.
	Publisher string `toml:"publisher"`

	// Forwarder, when set, opts the connector into the daemon-embedded
	// shared spawn-forwarder WASM (per ADR-0002's spawn-primitive
	// section). The only allowed value in v1 is "builtin://spawn-forwarder".
	// When set, the install pipeline routes around the per-connector
	// WASM fetch; the daemon supplies the forwarder bytes at execution
	// time and the manifest is the connector's complete identity.
	Forwarder string `toml:"forwarder,omitempty"`

	// Idempotency is the optional `[connector.idempotency]` block per
	// ADR-0010. Absent → idempotent default; present → must declare
	// `default = "idempotent" | "not_idempotent"`. The retry primitive
	// reads this via [Manifest.IsIdempotent] to decide whether to
	// retry a failed call.
	Idempotency *ManifestIdempotency `toml:"idempotency"`

	// Origin distinguishes how this connector entered the user's
	// installation. Empty (the default) means "hub-published":
	// the connector arrived through the content-addressed store
	// under `~/.aileron/store/connectors/sha256/` after a publisher
	// signed it and the operator installed via `aileron action add`.
	// Non-empty values flag connectors that bypassed that path:
	//
	//   - [OriginLocal]: BYOCLI connector authored at install time
	//     via `aileron cli add` (issue #749). Stored under
	//     `~/.aileron/connectors/local/<name>/manifest.toml` rather
	//     than the content-addressed store; not signed by a publisher
	//     and not shareable.
	//
	// Future tap-driven installs (issue #752) will land here as a
	// `tap://...` value. The action-list path renders the column;
	// callers should treat an unknown non-empty value as a tap
	// origin rather than failing closed.
	Origin string `toml:"origin,omitempty"`
}

// OriginLocal is the [ManifestConnector.Origin] value for BYOCLI
// connectors registered via `aileron cli add`. See the field
// documentation for the full origin taxonomy.
const OriginLocal = "local"

// BuiltinForwarderSpawn is the reserved FQN for the daemon-embedded
// spawn-forwarder. The only allowed value of ManifestConnector.Forwarder
// in v1; new builtin:// shapes require an ADR amendment.
const BuiltinForwarderSpawn = "builtin://spawn-forwarder"

// ManifestIdempotency is `[connector.idempotency]` — the connector's
// declaration of whether a retry could double-send a side effect.
type ManifestIdempotency struct {
	// Default is one of [IdempotencyIdempotent] or
	// [IdempotencyNotIdempotent]. Empty value is rejected at validation
	// time; absent block is treated as idempotent.
	Default string `toml:"default"`
}

// Idempotency declarations recognised by the runtime. Per ADR-0010,
// the closed set is just two values; an empty `default` is invalid
// (authors must opt in explicitly to opt out).
const (
	IdempotencyIdempotent    = "idempotent"
	IdempotencyNotIdempotent = "not_idempotent"
)

// ManifestCapabilities holds the sub-tables for each primitive capability
// type the runtime knows about (per ADR-0002).
type ManifestCapabilities struct {
	Network    *ManifestNetwork    `toml:"network"`
	Credential *ManifestCredential `toml:"credential"`
	Runtime    *ManifestRuntime    `toml:"runtime"`
	Spawn      *ManifestSpawn      `toml:"spawn"`
}

// ManifestNetwork is `[capabilities.network]` — declared outbound grants.
// Hosts are pinned to specific `host:port` pairs; no wildcards.
type ManifestNetwork struct {
	Hosts []string `toml:"hosts"`
}

// ManifestCredential is `[capabilities.credential]` — abstract credential
// type and scope. The connector declares the *type*; the user binds a
// concrete vault entry at install or first use (ADR-0002, ADR-0006).
//
// `Scope` is human-readable prose surfaced in CLI prompts (e.g. "Read
// your email and send messages"). For OAuth credentials, the technical
// scope strings sent to the provider live on `OAuth2.Scopes` instead;
// the two fields serve different audiences.
type ManifestCredential struct {
	Kind   string          `toml:"kind"`
	Scope  string          `toml:"scope"`
	OAuth2 *ManifestOAuth2 `toml:"oauth2,omitempty"`
}

// ManifestOAuth2 is `[capabilities.credential.oauth2]` — the OAuth
// provider configuration the connector publisher registered. Required
// when `Kind == "oauth2"`. Per ADR-0002, the publisher owns the OAuth
// app: each connector publisher registers their own client with the
// service (Google, Slack, Notion, etc.), and the consent screen the
// user sees names that publisher.
//
// v1 requires PKCE (S256) and uses loopback redirect only. PKCE is the
// binding security mechanism — `client_secret` (when present) is a
// public client identifier that some providers nonetheless require at
// the token endpoint for installed-app client types.
//
// `ClientSecret` is optional. Some providers (Google's "Desktop app"
// OAuth client type, notably) reject token-exchange requests that
// omit a registered client_secret even when PKCE is used. The
// "secret" Google issues for installed apps ships in distributed
// binaries (gcloud, gh, and every IDE extension does this) and is
// not cryptographically secret per Google's own installed-app
// guidance — but the API enforces its presence. Providers that
// genuinely treat client_secret as a server-only credential
// (web-app OAuth flows behind a hosted backend) MUST NOT set this
// field on a connector manifest; PKCE alone protects loopback
// installed-app flows where it does.
type ManifestOAuth2 struct {
	// AuthorizeURL is the provider's authorization endpoint. The CLI
	// directs the user's browser here with the connector's client_id,
	// the requested scopes, and a PKCE challenge.
	AuthorizeURL string `toml:"authorize_url"`

	// TokenURL is the provider's token endpoint. The runtime exchanges
	// the authorization code here for an access + refresh token, and
	// later POSTs refresh requests here transparently when the access
	// token nears expiry.
	TokenURL string `toml:"token_url"`

	// ClientID is the connector publisher's OAuth client id, as
	// registered with the provider.
	ClientID string `toml:"client_id"`

	// ClientSecret is the publisher's OAuth client secret. Optional
	// and only meaningful for installed-app client types whose
	// providers require it at the token endpoint despite PKCE
	// (Google Desktop). The value ships in the connector binary;
	// PKCE remains the binding security mechanism. Leave unset
	// for providers where the loopback flow does not require it.
	ClientSecret string `toml:"client_secret,omitempty"`

	// Scopes is the list of technical scope strings the provider
	// expects (e.g. `https://www.googleapis.com/auth/gmail.send`).
	Scopes []string `toml:"scopes"`
}

// ManifestRuntime is `[capabilities.runtime]` — host-function imports. Used
// by the WASM sandbox to gate which host functions the connector may call.
type ManifestRuntime struct {
	Imports []string `toml:"imports"`
}

// ManifestSpawn is `[capabilities.spawn]` (per ADR-0002 spawn primitive,
// ADR-0014 sandbox technology). Declares the connector's local subprocess
// grants: which programs may be invoked, with which argv shapes, which
// environment keys may be forwarded, and which filesystem scopes the
// subprocess may read or write. Absent block means the connector may not
// spawn anything.
//
// The runtime enforces this declaration at host-function call time via
// HostPolicy.CheckSpawn, mirroring the network primitive's
// HostPolicy.CheckURL.
type ManifestSpawn struct {
	// Programs lists allowed binaries. Each entry pins the binary by
	// absolute filesystem path (or `~/`-anchored path) and optionally
	// by content hash. The runtime resolves the path before exec and
	// refuses spawn if it does not match an entry. When Hash is set,
	// the runtime additionally verifies the binary's bytes.
	Programs []ManifestSpawnProgram `toml:"programs"`

	// Operations is the closed map of operations the connector exposes.
	// Each entry is keyed by op name (matched at call time against the
	// agent's tool-call op) and carries an argv pattern with `{name}`
	// placeholders the runtime substitutes from the agent's args. The
	// gate's argv allow-list is derived from operations[*].argv.
	//
	// An incoming op whose name is not in this map is denied with
	// capability_denied. The forwarder WASM consumes this table via the
	// aileron_host.spawn_op host function; per-binary connectors using
	// the low-level aileron_host.spawn can also read it from the runtime
	// gate.
	Operations map[string]ManifestSpawnOperation `toml:"operations"`

	// EnvPassthrough is the closed set of environment keys the runtime
	// may set on the subprocess. The subprocess receives only declared
	// keys; nothing is forwarded by default. Credential injection rides
	// this channel (per ADR-0002 spawn-primitive credential rule).
	EnvPassthrough []string `toml:"env_passthrough"`

	// CredentialEnvKeys is the subset of EnvPassthrough into which the
	// runtime injects the resolved credential value (per [capabilities.credential]).
	// Forwarder connectors use this declaration so the runtime knows
	// which env keys to fill with the vault binding at spawn_op time.
	// Each entry must also appear in EnvPassthrough.
	CredentialEnvKeys []string `toml:"credential_env_keys,omitempty"`

	// FSRead is the filesystem read scope. Each entry is an absolute
	// path or `~/`-anchored path. The sandbox restricts subprocess
	// reads to these paths.
	FSRead []string `toml:"fs_read"`

	// FSWrite is the filesystem write scope. Same shape as FSRead.
	FSWrite []string `toml:"fs_write"`

	// Cwd is the optional working-directory policy. When set, the
	// runtime invokes the subprocess with this cwd; the cwd must be
	// within FSRead. When unset, the runtime uses a per-invocation
	// temporary directory.
	Cwd string `toml:"cwd,omitempty"`

	// Limits is the optional `[capabilities.spawn.limits]` block. When
	// absent, the runtime applies [DefaultMaxStdoutBytes] and
	// [DefaultMaxStderrBytes]. When present, the declared values
	// override the defaults; either field may be omitted to use the
	// default for that stream alone. The caps bound the bytes the
	// runtime captures from the subprocess; output past the cap is
	// truncated and a structured marker appended.
	//
	// Caps are part of the security model: stdout is read back into
	// the connector and ultimately the agent's LLM context, so an
	// unbounded subprocess can be coerced into producing an arbitrary
	// volume of attacker-chosen bytes. A finite cap bounds the side
	// channel's blast radius.
	Limits *ManifestSpawnLimits `toml:"limits,omitempty"`
}

// ManifestSpawnLimits is `[capabilities.spawn.limits]` — per-stream
// byte caps on the subprocess's captured output. Values are absolute
// byte counts. A nil pointer means "use the default for both
// streams"; a partially-set struct (one field nil) means "default
// for the unset stream."
type ManifestSpawnLimits struct {
	// MaxStdoutBytes caps the bytes the runtime captures from the
	// subprocess's stdout. When the subprocess produces more, the
	// extra bytes are dropped and a structured truncation marker is
	// appended to the captured slice. nil means "use [DefaultMaxStdoutBytes]."
	MaxStdoutBytes *int64 `toml:"max_stdout_bytes,omitempty"`

	// MaxStderrBytes caps the bytes the runtime captures from the
	// subprocess's stderr. Same truncation semantics as
	// MaxStdoutBytes. nil means "use [DefaultMaxStderrBytes]."
	MaxStderrBytes *int64 `toml:"max_stderr_bytes,omitempty"`
}

// Spawn output cap defaults and upper bound. Per ADR-0014's
// network-confinement and output-cap consequences, the runtime
// enforces these even when the manifest omits the limits block.
const (
	// DefaultMaxStdoutBytes is 1 MiB — large enough for typical
	// structured CLI output (JSON dumps, log tails), small enough
	// to bound the side channel through the agent's LLM context.
	DefaultMaxStdoutBytes int64 = 1 << 20

	// DefaultMaxStderrBytes is 256 KiB — diagnostic messages on
	// failure rarely need more.
	DefaultMaxStderrBytes int64 = 256 << 10

	// MaxSpawnOutputCap is the absolute ceiling the runtime will
	// honor for either stream's cap. Beyond this, the manifest
	// should be expressing "write to a file" via `fs_write` rather
	// than holding bytes in memory.
	MaxSpawnOutputCap int64 = 64 << 20
)

// StdoutCap returns the byte cap the runtime applies to subprocess
// stdout. Resolves the manifest's declared value when present, falls
// back to [DefaultMaxStdoutBytes] otherwise.
func (s *ManifestSpawn) StdoutCap() int64 {
	if s == nil || s.Limits == nil || s.Limits.MaxStdoutBytes == nil {
		return DefaultMaxStdoutBytes
	}
	return *s.Limits.MaxStdoutBytes
}

// StderrCap returns the byte cap the runtime applies to subprocess
// stderr. Resolves the manifest's declared value when present, falls
// back to [DefaultMaxStderrBytes] otherwise.
func (s *ManifestSpawn) StderrCap() int64 {
	if s == nil || s.Limits == nil || s.Limits.MaxStderrBytes == nil {
		return DefaultMaxStderrBytes
	}
	return *s.Limits.MaxStderrBytes
}

// ManifestSpawnOperation is one entry in [capabilities.spawn.operations]:
// an argv template the runtime substitutes from the agent's args plus
// optional human-readable description for action.md generation.
type ManifestSpawnOperation struct {
	// Argv is the templated argv shape. Tokens are whitespace-separated;
	// each token may contain `{name}` placeholders that bind to the
	// agent's args at call time. The runtime validates the substituted
	// argv against this pattern and refuses any envelope whose argv
	// doesn't match.
	Argv string `toml:"argv"`

	// Description is a one-line human-readable summary. Surfaced in the
	// agent's tool list and the wrap tool's generated action.md.
	Description string `toml:"description,omitempty"`
}

// ArgvPatterns returns the flat list of argv templates derived from the
// manifest's operations map. The sandbox gate (SpawnPolicy.CheckSpawn)
// consumes this list as its argv allow-list.
func (s *ManifestSpawn) ArgvPatterns() []string {
	if s == nil || len(s.Operations) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Operations))
	for _, op := range s.Operations {
		out = append(out, op.Argv)
	}
	return out
}

// ManifestSpawnProgram is one entry in [capabilities.spawn].programs:
// a program path plus an optional content-hash pin.
type ManifestSpawnProgram struct {
	// Path is the absolute (or `~/`-anchored) filesystem path of the
	// binary. The runtime resolves the path and compares before exec.
	Path string `toml:"path"`

	// Hash is the optional SHA-256 of the binary's bytes, prefixed
	// with `sha256:`. When set, the runtime verifies on every call.
	Hash string `toml:"hash,omitempty"`
}

// ManifestProvides is `[provides]` — discovery metadata. Not enforced by
// the runtime; consumed by the Hub and by action authors.
type ManifestProvides struct {
	Intents []string `toml:"intents"`
}

// ParseManifest decodes a connector manifest from raw TOML bytes. `file` is
// the path of the source file (for error reporting) and may be empty when
// the manifest is in-memory.
func ParseManifest(file string, data []byte) (*Manifest, error) {
	m := &Manifest{}
	meta, err := toml.Decode(string(data), m)
	if err != nil {
		return nil, newParseErr(file, tomlErrorLine(err), "invalid TOML: %s", err.Error())
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, newParseErr(file, 0, "unknown manifest key(s): %s", strings.Join(keys, ", "))
	}
	return m, nil
}

// ValidateManifest enforces the schema described in ADR-0002. It does not
// check that the manifest's declared FQN matches the FQN the binary is
// being installed under — that's the install pipeline's job.
func ValidateManifest(m *Manifest, file string) error {
	if m == nil {
		return newValidationErr(file, "manifest is nil")
	}
	if m.Connector.Name == "" {
		return newValidationErr(file, "[connector].name is required")
	}
	if _, err := ParseFQN(m.Connector.Name); err != nil {
		return newValidationErr(file, "[connector].name: %s", err.Error())
	}
	if m.Connector.Version == "" {
		return newValidationErr(file, "[connector].version is required")
	}
	if !semverRe.MatchString(m.Connector.Version) {
		return newValidationErr(file, "[connector].version %q must be strict SemVer", m.Connector.Version)
	}
	if m.Connector.ProvenanceHash != "" {
		if !strings.HasPrefix(m.Connector.ProvenanceHash, "sha256:") || len(m.Connector.ProvenanceHash) <= len("sha256:") {
			return newValidationErr(file, "[connector].provenance_hash %q must be prefixed with sha256:", m.Connector.ProvenanceHash)
		}
	}
	if m.Connector.Forwarder != "" {
		if m.Connector.Forwarder != BuiltinForwarderSpawn {
			return newValidationErr(file,
				"[connector].forwarder %q is not recognized; v1 supports only %q",
				m.Connector.Forwarder, BuiltinForwarderSpawn)
		}
		if m.Capabilities.Spawn == nil {
			return newValidationErr(file,
				"[connector].forwarder is set but [capabilities.spawn] is absent; the forwarder requires a spawn capability to drive")
		}
	}
	if m.Capabilities.Network != nil {
		for i, h := range m.Capabilities.Network.Hosts {
			if !strings.Contains(h, ":") {
				return newValidationErr(file, "[capabilities.network].hosts[%d] %q must be host:port", i, h)
			}
		}
	}
	if cred := m.Capabilities.Credential; cred != nil {
		if err := validateCredential(cred, file); err != nil {
			return err
		}
	}
	if sp := m.Capabilities.Spawn; sp != nil {
		if err := validateSpawn(sp, file); err != nil {
			return err
		}
	}
	if m.Connector.Idempotency != nil {
		switch m.Connector.Idempotency.Default {
		case IdempotencyIdempotent, IdempotencyNotIdempotent:
		case "":
			return newValidationErr(file,
				"[connector.idempotency].default is required when the table is present (use %q or %q)",
				IdempotencyIdempotent, IdempotencyNotIdempotent)
		default:
			return newValidationErr(file,
				"[connector.idempotency].default %q must be %q or %q",
				m.Connector.Idempotency.Default, IdempotencyIdempotent, IdempotencyNotIdempotent)
		}
	}
	return nil
}

// CredentialKindOAuth2 names the OAuth 2.0 credential kind.
const CredentialKindOAuth2 = "oauth2"

// CredentialKindAPIKey names the API-key credential kind.
const CredentialKindAPIKey = "api_key"

// validateCredential enforces the v1 closed set of credential kinds
// and the kind-specific manifest requirements documented in ADR-0002.
//
//   - kind = "api_key": no further fields required; the
//     `[capabilities.credential.oauth2]` table must be absent.
//   - kind = "oauth2": the `[capabilities.credential.oauth2]` table is
//     required and must declare authorize_url, token_url, client_id,
//     and at least one scope. URL fields must be valid `https://`
//     URLs.
//
// Other kinds are rejected at install time. Kinds outside the v1 set
// (basic, x509, etc.) are post-MVP and require an ADR amendment.
func validateCredential(c *ManifestCredential, file string) error {
	switch c.Kind {
	case "":
		return newValidationErr(file, "[capabilities.credential].kind is required")
	case CredentialKindAPIKey:
		if c.OAuth2 != nil {
			return newValidationErr(file,
				"[capabilities.credential.oauth2] must be absent when kind = %q", c.Kind)
		}
		return nil
	case CredentialKindOAuth2:
		if c.OAuth2 == nil {
			return newValidationErr(file,
				"[capabilities.credential.oauth2] is required when kind = %q", c.Kind)
		}
		return validateOAuth2(c.OAuth2, file)
	default:
		return newValidationErr(file,
			"[capabilities.credential].kind %q is not in the v1 closed set (%q, %q)",
			c.Kind, CredentialKindAPIKey, CredentialKindOAuth2)
	}
}

// validateOAuth2 enforces the schema of `[capabilities.credential.oauth2]`.
// All four fields (authorize_url, token_url, client_id, scopes) are
// required; URL fields must be `https://` URLs (production providers)
// or loopback `http://localhost` / `http://127.0.0.1` URLs (tests and
// local development per RFC 8252 §7.3).
func validateOAuth2(o *ManifestOAuth2, file string) error {
	if strings.TrimSpace(o.AuthorizeURL) == "" {
		return newValidationErr(file, "[capabilities.credential.oauth2].authorize_url is required")
	}
	if !isAllowedOAuthURL(o.AuthorizeURL) {
		return newValidationErr(file,
			"[capabilities.credential.oauth2].authorize_url %q must be https:// (or http:// loopback for tests)", o.AuthorizeURL)
	}
	if strings.TrimSpace(o.TokenURL) == "" {
		return newValidationErr(file, "[capabilities.credential.oauth2].token_url is required")
	}
	if !isAllowedOAuthURL(o.TokenURL) {
		return newValidationErr(file,
			"[capabilities.credential.oauth2].token_url %q must be https:// (or http:// loopback for tests)", o.TokenURL)
	}
	if strings.TrimSpace(o.ClientID) == "" {
		return newValidationErr(file, "[capabilities.credential.oauth2].client_id is required")
	}
	if len(o.Scopes) == 0 {
		return newValidationErr(file, "[capabilities.credential.oauth2].scopes is required (at least one)")
	}
	for i, s := range o.Scopes {
		if strings.TrimSpace(s) == "" {
			return newValidationErr(file, "[capabilities.credential.oauth2].scopes[%d] is empty", i)
		}
	}
	return nil
}

// envKeyRe matches a POSIX-shape environment variable name: a leading
// letter or underscore, followed by letters, digits, or underscores.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// opNameRe matches an operation name in [capabilities.spawn.operations].
// Lowercase, alphanumeric, underscores, and hyphens; must begin with a
// letter. Names appear in action.md filenames and in the agent's tool
// surface, so the shape is conservative.
var opNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// validateSpawn enforces the [capabilities.spawn] schema described in
// ADR-0002's spawn-primitive section. See also ADR-0014 for the runtime
// enforcement mechanism the declaration translates into.
//
// A spawn block is only meaningful when it grants at least one program;
// an empty programs list is rejected so a connector cannot accidentally
// ship an unenforceable spawn capability. All paths (program paths, FS
// scopes, cwd) must be absolute or `~/`-anchored so the runtime can
// resolve them portably across Linux, macOS, and Windows. Argv patterns
// must each have at least one token. Environment keys must be valid
// POSIX-shape identifiers.
func validateSpawn(s *ManifestSpawn, file string) error {
	if len(s.Programs) == 0 {
		return newValidationErr(file,
			"[capabilities.spawn].programs is required (at least one)")
	}
	for i, p := range s.Programs {
		if strings.TrimSpace(p.Path) == "" {
			return newValidationErr(file,
				"[capabilities.spawn].programs[%d].path is required", i)
		}
		if !isAbsoluteOrTildePath(p.Path) {
			return newValidationErr(file,
				"[capabilities.spawn].programs[%d].path %q must be absolute (/...) or ~/-anchored",
				i, p.Path)
		}
		if p.Hash != "" {
			if !strings.HasPrefix(p.Hash, "sha256:") || len(p.Hash) <= len("sha256:") {
				return newValidationErr(file,
					"[capabilities.spawn].programs[%d].hash %q must be prefixed with sha256:",
					i, p.Hash)
			}
		}
	}
	if len(s.Operations) == 0 {
		return newValidationErr(file,
			"[capabilities.spawn.operations] is required (at least one operation)")
	}
	for name, op := range s.Operations {
		if !opNameRe.MatchString(name) {
			return newValidationErr(file,
				"[capabilities.spawn.operations].%q name must match %s",
				name, opNameRe.String())
		}
		if strings.TrimSpace(op.Argv) == "" {
			return newValidationErr(file,
				"[capabilities.spawn.operations].%q.argv is required", name)
		}
	}
	envSet := make(map[string]struct{}, len(s.EnvPassthrough))
	for i, k := range s.EnvPassthrough {
		if !envKeyRe.MatchString(k) {
			return newValidationErr(file,
				"[capabilities.spawn].env_passthrough[%d] %q is not a valid environment variable name",
				i, k)
		}
		envSet[k] = struct{}{}
	}
	for i, k := range s.CredentialEnvKeys {
		if !envKeyRe.MatchString(k) {
			return newValidationErr(file,
				"[capabilities.spawn].credential_env_keys[%d] %q is not a valid environment variable name",
				i, k)
		}
		if _, ok := envSet[k]; !ok {
			return newValidationErr(file,
				"[capabilities.spawn].credential_env_keys[%d] %q must also appear in env_passthrough",
				i, k)
		}
	}
	for i, p := range s.FSRead {
		if !isAbsoluteOrTildePath(p) {
			return newValidationErr(file,
				"[capabilities.spawn].fs_read[%d] %q must be absolute (/...) or ~/-anchored",
				i, p)
		}
	}
	for i, p := range s.FSWrite {
		if !isAbsoluteOrTildePath(p) {
			return newValidationErr(file,
				"[capabilities.spawn].fs_write[%d] %q must be absolute (/...) or ~/-anchored",
				i, p)
		}
	}
	if s.Cwd != "" && !isAbsoluteOrTildePath(s.Cwd) {
		return newValidationErr(file,
			"[capabilities.spawn].cwd %q must be absolute (/...) or ~/-anchored",
			s.Cwd)
	}
	if s.Limits != nil {
		if err := validateSpawnLimits(s.Limits, file); err != nil {
			return err
		}
	}
	return nil
}

// validateSpawnLimits enforces the [capabilities.spawn.limits]
// schema: each declared cap must be positive (zero is reserved for
// "the default" by omitting the field, not by setting it to zero)
// and at most [MaxSpawnOutputCap]. The runtime treats unbounded
// output as a security risk per ADR-0014; the manifest cannot
// express it.
func validateSpawnLimits(l *ManifestSpawnLimits, file string) error {
	if l.MaxStdoutBytes != nil {
		v := *l.MaxStdoutBytes
		if v <= 0 {
			return newValidationErr(file,
				"[capabilities.spawn.limits].max_stdout_bytes %d must be positive (omit the field to use the default)", v)
		}
		if v > MaxSpawnOutputCap {
			return newValidationErr(file,
				"[capabilities.spawn.limits].max_stdout_bytes %d exceeds the maximum permitted value %d", v, MaxSpawnOutputCap)
		}
	}
	if l.MaxStderrBytes != nil {
		v := *l.MaxStderrBytes
		if v <= 0 {
			return newValidationErr(file,
				"[capabilities.spawn.limits].max_stderr_bytes %d must be positive (omit the field to use the default)", v)
		}
		if v > MaxSpawnOutputCap {
			return newValidationErr(file,
				"[capabilities.spawn.limits].max_stderr_bytes %d exceeds the maximum permitted value %d", v, MaxSpawnOutputCap)
		}
	}
	return nil
}

// isAbsoluteOrTildePath reports whether p is either an absolute path
// (`/...`) or a `~`-anchored path (`~`, `~/...`). Windows-style
// drive-rooted paths (`C:\...`) are not accepted in the manifest
// schema; per ADR-0014 the runtime translates `~/`-anchored paths to
// the host's user profile on Windows.
func isAbsoluteOrTildePath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") {
		return true
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		return true
	}
	return false
}

// isAllowedOAuthURL reports whether s is an allowable OAuth provider
// URL: https:// for production, or http:// against a loopback host
// (`localhost`, `127.0.0.1`) per RFC 8252 §7.3 for installed apps and
// for test harnesses using httptest.NewServer.
func isAllowedOAuthURL(s string) bool {
	if strings.HasPrefix(s, "https://") {
		return true
	}
	if !strings.HasPrefix(s, "http://") {
		return false
	}
	rest := strings.TrimPrefix(s, "http://")
	host := rest
	if i := strings.IndexAny(rest, ":/"); i >= 0 {
		host = rest[:i]
	}
	return host == "localhost" || host == "127.0.0.1"
}

// tomlErrorLine extracts a 1-based line number from a BurntSushi/toml error
// when the error type exposes one. Returns 0 otherwise.
func tomlErrorLine(err error) int {
	type lineError interface {
		Line() int
	}
	if le, ok := err.(lineError); ok && le.Line() > 0 {
		return le.Line()
	}
	return 0
}
