package action

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// semverRe matches strict SemVer 2.0.0 (no leading "v"), per ADR-0002 and
// ADR-0003. Pre-release and build-metadata tails are accepted.
var semverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// nameRe validates the action's bare local handle. Per ADR-0003 the user
// owns the file and chooses the name; we constrain it to a portable filename
// shape so listing endpoints and on-disk discovery agree.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// fqnSchemes is the closed set of FQN schemes recognized in v1, per ADR-0002.
// Adding a scheme requires an ADR amendment (or a future ADR delegating
// scheme registration).
var fqnSchemes = map[string]bool{
	"github": true,
	"gitlab": true,
	"hub":    true,
}

// Validate checks a parsed manifest against the schema described in
// ADR-0001 and ADR-0003. Returns a *Error on the first failure. Successive
// fields are checked in declared order so authors see one specific message
// at a time rather than a flood.
func Validate(m *Manifest, file string) error {
	if m == nil {
		return newValidationErr(file, "manifest is nil")
	}
	if m.Name == "" {
		return newValidationErr(file, "name is required")
	}
	if !nameRe.MatchString(m.Name) {
		return newValidationErr(file, "name %q must match %s", m.Name, nameRe.String())
	}
	if m.Version == "" {
		return newValidationErr(file, "version is required")
	}
	if !semverRe.MatchString(m.Version) {
		return newValidationErr(file, "version %q must be strict SemVer (e.g. 1.2.3)", m.Version)
	}
	if m.Source == "" {
		return newValidationErr(file, "source is required")
	}
	if err := validateSourceFQN(m.Source); err != nil {
		return newValidationErr(file, "invalid source: %s", err.Error())
	}
	if len(m.Requires.Connectors) == 0 {
		return newValidationErr(file, "[[requires.connectors]] is required (at least one connector)")
	}
	for i, c := range m.Requires.Connectors {
		if c.Name == "" {
			return newValidationErr(file, "requires.connectors[%d].name is required", i)
		}
		if err := validateConnectorFQN(c.Name); err != nil {
			return newValidationErr(file, "requires.connectors[%d].name: %s", i, err.Error())
		}
		if c.Version == "" {
			return newValidationErr(file, "requires.connectors[%d].version is required", i)
		}
		if !semverRe.MatchString(c.Version) {
			return newValidationErr(file, "requires.connectors[%d].version %q must be strict SemVer", i, c.Version)
		}
		if c.Hash == "" {
			return newValidationErr(file, "requires.connectors[%d].hash is required", i)
		}
		if !strings.HasPrefix(c.Hash, "sha256:") || len(c.Hash) <= len("sha256:") {
			return newValidationErr(file, "requires.connectors[%d].hash %q must be prefixed with sha256:", i, c.Hash)
		}
		if len(c.Capabilities) == 0 {
			return newValidationErr(file, "requires.connectors[%d].capabilities is required (declare the subset the action exercises)", i)
		}
		for j, cap := range c.Capabilities {
			if strings.TrimSpace(cap) == "" {
				return newValidationErr(file, "requires.connectors[%d].capabilities[%d] is empty", i, j)
			}
		}
	}
	if m.Match.Intent == "" {
		return newValidationErr(file, "[match].intent is required")
	}
	if len(m.Execute) == 0 {
		return newValidationErr(file, "[[execute]] is required (at least one step)")
	}
	connectorByFQN := map[string]bool{}
	for _, c := range m.Requires.Connectors {
		connectorByFQN[c.Name] = true
	}
	stepIDs := map[string]bool{}
	for i, s := range m.Execute {
		if s.ID == "" {
			return newValidationErr(file, "execute[%d].id is required", i)
		}
		if stepIDs[s.ID] {
			return newValidationErr(file, "execute[%d].id %q is duplicated", i, s.ID)
		}
		stepIDs[s.ID] = true
		if s.Connector == "" {
			return newValidationErr(file, "execute[%d].connector is required", i)
		}
		if !connectorByFQN[s.Connector] {
			return newValidationErr(file, "execute[%d].connector %q is not declared in [[requires.connectors]]", i, s.Connector)
		}
		if s.Op == "" {
			return newValidationErr(file, "execute[%d].op is required", i)
		}
	}
	return nil
}

// validateSourceFQN parses an action source URI of the shape
// `<scheme>://<owner>/<path>@<version>`. The version suffix is mandatory for
// `source` per ADR-0003.
func validateSourceFQN(s string) error {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return fmt.Errorf("missing @<version> suffix")
	}
	base, ver := s[:at], s[at+1:]
	if !semverRe.MatchString(ver) {
		return fmt.Errorf("version %q must be strict SemVer", ver)
	}
	return validateConnectorFQN(base)
}

// validateConnectorFQN parses a connector URI of the shape
// `<scheme>://<owner>/<path>` (no version suffix — the version pins live in
// adjacent fields).
func validateConnectorFQN(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URI: %s", err.Error())
	}
	if !fqnSchemes[u.Scheme] {
		schemes := make([]string, 0, len(fqnSchemes))
		for k := range fqnSchemes {
			schemes = append(schemes, k+"://")
		}
		return fmt.Errorf("scheme %q is not recognized (expected one of %s)", u.Scheme, strings.Join(schemes, ", "))
	}
	if u.Host == "" {
		return fmt.Errorf("missing owner segment")
	}
	if u.Path == "" || u.Path == "/" {
		return fmt.Errorf("missing path segment")
	}
	return nil
}
