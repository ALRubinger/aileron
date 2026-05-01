// Package binding implements the per-user capability binding store.
//
// Per [ADR-0006], a binding maps a connector's declared
// `[capabilities.credential]` to a concrete vault entry that holds the
// credential bytes. Bindings are explicit (no silent matching), named
// `<kind>/<service>/<identity>`, and consulted by the runtime at
// credential-mediation time. The same name string is the vault path
// the runtime resolves — there is no second store.
//
// This package wraps [vault.Vault] in a typed domain layer. Listing
// and inspection do not require an unlock (per [ADR-0011], metadata
// is plaintext); creating, replacing, and resolving the underlying
// credential bytes do.
//
// [ADR-0006]: https://docs.withaileron.ai/adr/0006-capability-binding-ux
// [ADR-0011]: https://docs.withaileron.ai/adr/0011-local-credential-vault
package binding

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/vault"
)

// nameRe matches a valid binding name `<kind>/<service>/<identity>`.
// Each segment is lowercase + digits + dot/dash/underscore, must start
// with an alphanumeric character. The format is the load-bearing
// identity contract from ADR-0006: kind-then-service-then-identity, in
// that order, three segments, no path traversal.
var nameRe = regexp.MustCompile(`^([a-z][a-z0-9_-]*)/([a-z0-9][a-z0-9._-]*)/([a-z0-9][a-z0-9._-]*)$`)

// Name is a fully-qualified binding name. The zero value is invalid.
type Name string

// Parse splits a binding name into its kind, service, and identity
// segments. Returns an error if the input does not match the canonical
// format documented on [Name].
func Parse(s string) (Name, string, string, string, error) {
	m := nameRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", "", "", fmt.Errorf("invalid binding name %q: must match <kind>/<service>/<identity>", s)
	}
	return Name(s), m[1], m[2], m[3], nil
}

// MakeName composes a binding name from its three segments. Returns an
// error if the result would not parse — i.e. any segment is empty or
// uses disallowed characters.
func MakeName(kind, service, identity string) (Name, error) {
	candidate := kind + "/" + service + "/" + identity
	if !nameRe.MatchString(candidate) {
		return "", fmt.Errorf("invalid binding segments: kind=%q service=%q identity=%q", kind, service, identity)
	}
	return Name(candidate), nil
}

// String returns the canonical string form.
func (n Name) String() string { return string(n) }

// Status enumerates the coarse health flags v1 surfaces. Stale and
// revoked are reserved for future refresh-tracking work; the v1
// runtime always reports `active` for present bindings.
type Status string

const (
	StatusActive  Status = "active"
	StatusStale   Status = "stale"
	StatusRevoked Status = "revoked"
)

// Binding is the typed view of a vault entry that holds a bound
// credential. It carries metadata the runtime and CLI need to display
// the binding to the user; it never carries the credential bytes.
type Binding struct {
	Name                Name
	Kind                string
	Service             string
	Identity            string
	Scope               string
	ConnectorFQN        string
	Account             string
	CreatedAt           time.Time
	LastUsedAt          time.Time
	LastRefreshedAt     time.Time
	RefreshTokenPresent bool
	Status              Status
}

// Label keys used to encode binding metadata into vault.Metadata.Labels.
// These are stable wire identifiers — changing them invalidates
// existing on-disk bindings.
const (
	labelService           = "service"
	labelIdentity          = "identity"
	labelScope             = "scope"
	labelConnectorFQN      = "connector_fqn"
	labelAccount           = "account"
	labelCreatedAt         = "created_at"
	labelLastUsedAt        = "last_used_at"
	labelLastRefreshedAt   = "last_refreshed_at"
	labelRefreshTokenFlag  = "refresh_token_present"
	labelStatus            = "status"
)

// toMetadata encodes the binding's non-secret fields into vault
// metadata. The credential bytes go into the vault entry's Value via
// a separate Put call.
func (b Binding) toMetadata() vault.Metadata {
	labels := map[string]string{
		labelService:      b.Service,
		labelIdentity:     b.Identity,
		labelConnectorFQN: b.ConnectorFQN,
		labelStatus:       string(b.Status),
		labelCreatedAt:    formatTime(b.CreatedAt),
	}
	if b.Scope != "" {
		labels[labelScope] = b.Scope
	}
	if b.Account != "" {
		labels[labelAccount] = b.Account
	}
	if !b.LastUsedAt.IsZero() {
		labels[labelLastUsedAt] = formatTime(b.LastUsedAt)
	}
	if !b.LastRefreshedAt.IsZero() {
		labels[labelLastRefreshedAt] = formatTime(b.LastRefreshedAt)
	}
	if b.RefreshTokenPresent {
		labels[labelRefreshTokenFlag] = "true"
	}
	return vault.Metadata{Type: b.Kind, Labels: labels}
}

// fromEntry decodes a vault listing row into a Binding. Returns an
// error when the path is not a valid binding name; the entry's
// metadata may be missing fields, which decode to zero values.
func fromEntry(e vault.Entry) (Binding, error) {
	name, kind, service, identity, err := Parse(e.Path)
	if err != nil {
		return Binding{}, err
	}
	labels := e.Metadata.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	// Prefer Metadata.Type (the vault SPI's canonical kind slot) over
	// the parsed name's kind segment when they disagree, because the
	// labels are written by Put and reflect intent at write time.
	if e.Metadata.Type != "" {
		kind = e.Metadata.Type
	}
	// Service/identity may also be carried in labels when the parsed
	// name agrees; trust the labels when present.
	if v := labels[labelService]; v != "" {
		service = v
	}
	if v := labels[labelIdentity]; v != "" {
		identity = v
	}
	status := Status(labels[labelStatus])
	if status == "" {
		status = StatusActive
	}
	return Binding{
		Name:                name,
		Kind:                kind,
		Service:             service,
		Identity:            identity,
		Scope:               labels[labelScope],
		ConnectorFQN:        labels[labelConnectorFQN],
		Account:             labels[labelAccount],
		CreatedAt:           parseTime(labels[labelCreatedAt]),
		LastUsedAt:          parseTime(labels[labelLastUsedAt]),
		LastRefreshedAt:     parseTime(labels[labelLastRefreshedAt]),
		RefreshTokenPresent: labels[labelRefreshTokenFlag] == "true",
		Status:              status,
	}, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.UTC().Unix(), 10)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// IsBindingPath reports whether the given vault path looks like a
// binding name. Used to filter out non-binding vault entries (e.g. the
// passphrase verification blob) when listing.
func IsBindingPath(path string) bool {
	if strings.HasPrefix(path, "_") {
		return false
	}
	return nameRe.MatchString(path)
}
