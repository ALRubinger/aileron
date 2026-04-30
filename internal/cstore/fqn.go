package cstore

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// FQN is a parsed connector fully-qualified URI per ADR-0002:
//
//	<scheme>://<owner>/<repo-or-namespace>/<...subpath>/<connector>
//
// For git-host schemes (`github://`, `gitlab://`), the first two segments
// after the scheme are the source repo (`<owner>/<repo>`) and any further
// segments locate the connector within the repo's tree (the *subpath*).
// For `hub://`, the path after the owner is the publisher's organizational
// choice within their Hub namespace (the subpath represents nested
// catalog structure rather than a repo subdirectory).
type FQN struct {
	// Raw is the original FQN string (e.g. "github://aileron/slack").
	Raw string

	// Scheme is one of `github`, `gitlab`, `hub`. Other schemes fail at
	// install per ADR-0004.
	Scheme string

	// Owner is the first path segment after the scheme — the publisher's
	// identity within the channel.
	Owner string

	// Repo is the second path segment for git-host schemes
	// (`<owner>/<repo>`). For `hub://` it is the first segment of the
	// publisher's namespace.
	Repo string

	// Subpath is the remaining path segments locating the connector within
	// the repo (or Hub namespace). Empty for single-artifact repos.
	// Stored without leading or trailing slashes; segments are joined with
	// `/` for emission.
	Subpath string
}

// String renders the FQN back to its canonical string form.
func (f FQN) String() string {
	if f.Raw != "" {
		return f.Raw
	}
	path := f.Owner + "/" + f.Repo
	if f.Subpath != "" {
		path += "/" + f.Subpath
	}
	return f.Scheme + "://" + path
}

// Authority returns the publication authority portion of the FQN — the
// `<scheme>://<owner>/<repo>` triple — used by signature verification to
// look up trusted keys per ADR-0002.
func (f FQN) Authority() string {
	return f.Scheme + "://" + f.Owner + "/" + f.Repo
}

// fqnSchemes is the closed v1 set per ADR-0004. Adding a scheme requires
// implementing the resolver's four operations (resolve URL, fetch, verify
// signature, list versions) and an ADR amendment.
var fqnSchemes = map[string]bool{
	"github": true,
	"gitlab": true,
	"hub":    true,
}

// semverRe matches strict SemVer 2.0.0 per ADR-0002.
var semverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// ParseFQN parses an FQN string. Returns an error when the scheme is
// unknown, the owner/repo segments are missing, or the URI is malformed.
// Does NOT accept `@<version>` suffixes — those are stripped by ParseRef.
func ParseFQN(s string) (FQN, error) {
	if s == "" {
		return FQN{}, fmt.Errorf("empty FQN")
	}
	if strings.Contains(s, "@") {
		return FQN{}, fmt.Errorf("FQN must not contain @<version> suffix; use ParseRef")
	}
	u, err := url.Parse(s)
	if err != nil {
		return FQN{}, fmt.Errorf("invalid URI: %s", err.Error())
	}
	if !fqnSchemes[u.Scheme] {
		schemes := make([]string, 0, len(fqnSchemes))
		for k := range fqnSchemes {
			schemes = append(schemes, k+"://")
		}
		return FQN{}, fmt.Errorf("scheme %q is not recognized (expected one of %s)", u.Scheme, strings.Join(schemes, ", "))
	}
	owner := u.Host
	if owner == "" {
		return FQN{}, fmt.Errorf("missing owner segment")
	}
	path := strings.TrimPrefix(u.Path, "/")
	if path == "" {
		return FQN{}, fmt.Errorf("missing repo segment")
	}
	parts := strings.Split(path, "/")
	repo := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = strings.Join(parts[1:], "/")
	}
	return FQN{
		Raw:     s,
		Scheme:  u.Scheme,
		Owner:   owner,
		Repo:    repo,
		Subpath: subpath,
	}, nil
}

// Ref is an FQN paired with a strict SemVer version: the canonical
// `aileron connector install <FQN>@<version>` form. The `@` separator is
// the documented compact form per ADR-0002.
type Ref struct {
	FQN     FQN
	Version string
}

// String renders the ref back to canonical compact form.
func (r Ref) String() string {
	return r.FQN.String() + "@" + r.Version
}

// ParseRef parses an `<FQN>@<version>` reference string. Both halves must
// be valid; the version must be strict SemVer. Returns a structured error
// when the input is malformed.
func ParseRef(s string) (Ref, error) {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return Ref{}, fmt.Errorf("missing @<version> suffix")
	}
	rawFQN, version := s[:at], s[at+1:]
	if version == "" {
		return Ref{}, fmt.Errorf("empty version")
	}
	if !semverRe.MatchString(version) {
		return Ref{}, fmt.Errorf("version %q must be strict SemVer", version)
	}
	fqn, err := ParseFQN(rawFQN)
	if err != nil {
		return Ref{}, err
	}
	return Ref{FQN: fqn, Version: version}, nil
}
