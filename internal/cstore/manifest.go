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

	// Idempotency is the optional `[connector.idempotency]` block per
	// ADR-0010. Absent → idempotent default; present → must declare
	// `default = "idempotent" | "not_idempotent"`. The retry primitive
	// reads this via [Manifest.IsIdempotent] to decide whether to
	// retry a failed call.
	Idempotency *ManifestIdempotency `toml:"idempotency"`
}

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
