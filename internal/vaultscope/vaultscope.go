// Package vaultscope is the single source of truth for classifying a raw
// vault path into its namespace scope. It is shared by the daemon's vault
// union endpoint (which projects it into the `VaultEntry.scope` wire field)
// and the `aileron secret list` CLI (which filters offline by the same
// classification), so both surfaces agree on exactly one code path for what
// a path means.
//
// The classification here is intentionally self-contained: it depends only
// on internal/binding for the binding-path grammar. It does NOT depend on
// internal/app, so the CLI can classify a path without spawning or reaching
// the daemon. The write-path validators and path builders (which construct
// `agents/<name>/<purpose>` and `user/<service>` keys) stay in internal/app
// where the credential handlers live; vaultscope only ever parses and
// classifies already-stored paths, never constructs them.
package vaultscope

import (
	"regexp"
	"strings"

	"github.com/ALRubinger/aileron/internal/binding"
)

// Namespace prefixes. The two control-plane namespaces
// (ConnectedAccountsVaultPrefix, LlmConfigVaultPrefix) are tenant-keyed
// records the cloud-shaped control plane owns (a per-user connected OAuth
// account and a per-owner LLM config), not local single-user `aileron
// launch` credentials. Classify flags them so a caller can exclude them
// from the default union view; an explicit opt-in includes them. The other
// prefixes name locally-owned namespaces that are always part of the union.
const (
	AgentVaultPrefix             = "agents/"
	UserVaultPrefix              = "user/"
	SecretVaultPrefix            = "secret/"
	ConnectedAccountsVaultPrefix = "connected-accounts/"
	LlmConfigVaultPrefix         = "llm-config/"
)

// Scope labels surfaced in the union view's `scope` field. These mirror the
// documented values in the OpenAPI `VaultEntry.scope` description; the spec
// models the field as a free-form string (not an enum) so these literals do
// not collide with the same words in other enums and force a global
// enum-constant rename in the generated code.
const (
	ScopeAgent            = "agent"
	ScopeUser             = "user"
	ScopeBinding          = "binding"
	ScopeConnectedAccount = "connected-account"
	ScopeLlmConfig        = "llm-config"
	ScopeSecret           = "secret"
	ScopeOther            = "other"
)

// agentPurposeSegmentRe and userServiceSegmentRe are private, defensive
// allow-lists for the trailing segment of a stored path. They mirror the
// write-path validators in internal/app (`^[a-z0-9][a-z0-9_-]*$`) so a
// malformed stored path can never surface a junk purpose or service in a
// classification. They are deliberately duplicated here rather than shared
// with the app write path: moving the app validators into this package
// would force a circular import (app -> vaultscope -> app) or a wide,
// security-sensitive rewrite of the write handlers for zero functional
// benefit. Keeping a small private copy for parse-time defense keeps the
// write-path behavior byte-identical and this package self-contained.
var (
	agentPurposeSegmentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	userServiceSegmentRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// AgentNameAndPurposeFromVaultPath splits an `agents/<name>/<purpose>` vault
// path into its name and purpose segments. It accepts any conforming path
// (any purpose, not just `oauth`) so a list surfaces apikey-only agents that
// an `/oauth`-suffix match would make invisible. It returns false for any
// path that does not match the scheme so a list filter ignores non-agent
// entries (user credentials, bindings, etc.).
//
// The purpose segment is re-validated through the same allow-list the write
// path uses so a malformed stored path can never surface a junk purpose.
func AgentNameAndPurposeFromVaultPath(path string) (name, purpose string, ok bool) {
	if !strings.HasPrefix(path, AgentVaultPrefix) {
		return "", "", false
	}
	rest := path[len(AgentVaultPrefix):]
	// Exactly two remaining segments: <name>/<purpose>. A name or purpose
	// containing a slash (extra segments) is rejected.
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	name, purpose = parts[0], parts[1]
	if name == "" || purpose == "" {
		return "", "", false
	}
	if !agentPurposeSegmentRe.MatchString(purpose) {
		return "", "", false
	}
	return name, purpose, true
}

// UserServiceFromVaultPath extracts the service name from a `user/<service>`
// vault path. It returns false for any path that does not match the scheme
// so a list filter ignores non-user entries (agent OAuth envelopes,
// bindings, etc.). The returned service must still pass the defensive
// allow-list: a stored path that somehow contains a `/` or other
// metacharacter in the service segment is rejected rather than surfaced.
func UserServiceFromVaultPath(path string) (string, bool) {
	if !strings.HasPrefix(path, UserVaultPrefix) {
		return "", false
	}
	service := path[len(UserVaultPrefix):]
	if !userServiceSegmentRe.MatchString(service) {
		return "", false
	}
	return service, true
}

// Classify maps a raw vault path to its scope label and reports whether it
// belongs to a control-plane namespace.
//
// The locally-owned namespaces (`agents/`, `user/`, `secret/`, and
// capability bindings — which include connector credentials like
// `connectors/<provider>/default` and OAuth bindings like
// `oauth2/google/...`) are always part of the union. The two tenant-keyed
// control-plane namespaces (`connected-accounts/`, `llm-config/`) are
// classified too but flagged so the caller can exclude them by default.
//
// Classification order is load-bearing: the prefixed namespaces are checked
// before the generic three-segment binding shape, because an
// `agents/<name>/<purpose>` path also matches the binding name grammar
// (`<kind>/<service>/<identity>`) and must classify as `agent`, not
// `binding`.
//
// Every path receives a scope. A path matching none of the known namespaces
// classifies as `other` so the union never silently hides an entry —
// surfacing unknown entries is the whole point of the union view (#1402).
func Classify(path string) (scope string, controlPlane bool) {
	switch {
	case strings.HasPrefix(path, ConnectedAccountsVaultPrefix):
		return ScopeConnectedAccount, true
	case strings.HasPrefix(path, LlmConfigVaultPrefix):
		return ScopeLlmConfig, true
	case strings.HasPrefix(path, SecretVaultPrefix):
		// Secrets set via `aileron secret set` are locally-owned, so they
		// are part of the default union (controlPlane=false). This is
		// checked before the binding grammar below because a slash-bearing
		// secret path could otherwise match the three-segment binding
		// shape.
		return ScopeSecret, false
	}
	if _, _, ok := AgentNameAndPurposeFromVaultPath(path); ok {
		return ScopeAgent, false
	}
	if _, ok := UserServiceFromVaultPath(path); ok {
		return ScopeUser, false
	}
	if binding.IsBindingPath(path) {
		return ScopeBinding, false
	}
	return ScopeOther, false
}
