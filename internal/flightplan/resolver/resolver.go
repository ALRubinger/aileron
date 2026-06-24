// Package resolver decides whether a Flight Plan skill's declared action
// references (requires.actions[].ref) are satisfied by the actions the
// running daemon exposes.
//
// This is the #1574 ref-to-action mapping. It is read-only against the
// existing daemon API (GET /v1/actions): no OpenAPI change, no new
// endpoint, no generated code. The resolver is a pure function over a
// decoded []Action plus a thin daemon fetch seam (client.go), so it is
// independently testable without a live daemon.
//
// A ref has the form `aileron:<connector>.<action>`. Per #1574 it is
// satisfied when some installed action matches on BOTH:
//
//   - connector: the ref's connector segment equals the FQN tail of one of
//     the action's requires.connectors[].name (e.g. ref connector
//     `slack` matches connector FQN `github://aileron/slack`).
//   - action: the ref's action segment equals the action's bare local
//     handle (Action.name), comparing with kebab/underscore normalization
//     (`query_series` and `query-series` are the same handle).
//
// The resolver never touches credential values. It matches names only.
package resolver

import (
	"fmt"
	"strings"
)

// Action is the subset of the daemon's Action object the resolver matches
// on. The client seam decodes only these fields from GET /v1/actions.
type Action struct {
	// Name is the bare local action handle (e.g. "query-series").
	Name string `json:"name"`
	// Requires carries the connector dependency declarations.
	Requires Requires `json:"requires"`
}

// Requires mirrors ActionRequires from the daemon API.
type Requires struct {
	Connectors []Connector `json:"connectors"`
}

// Connector mirrors ActionRequiresConnector; only the FQN name is matched.
type Connector struct {
	// Name is the connector FQN (e.g. "github://aileron/slack").
	Name string `json:"name"`
}

// Ref is a parsed action reference.
type Ref struct {
	// Raw is the original ref string as written in the manifest.
	Raw string
	// Connector is the connector segment (between `aileron:` and the dot).
	Connector string
	// Action is the action segment (after the first dot).
	Action string
}

// ParseRef splits an `aileron:<connector>.<action>` reference into its
// connector and action segments. The action segment may itself contain
// dots (the schema permits `[a-z0-9_.-]` after the first dot); the split is
// on the FIRST dot only, so the connector is unambiguous.
func ParseRef(raw string) (Ref, error) {
	const prefix = "aileron:"
	if !strings.HasPrefix(raw, prefix) {
		return Ref{}, fmt.Errorf("resolver: ref %q missing %q prefix", raw, prefix)
	}
	rest := raw[len(prefix):]
	dot := strings.IndexByte(rest, '.')
	if dot <= 0 || dot == len(rest)-1 {
		return Ref{}, fmt.Errorf("resolver: ref %q is not aileron:<connector>.<action>", raw)
	}
	return Ref{
		Raw:       raw,
		Connector: rest[:dot],
		Action:    rest[dot+1:],
	}, nil
}

// normalize collapses kebab/underscore differences so `query_series` and
// `query-series` compare equal. The daemon's aileron-mcp toolName() maps
// `-`→`_`; the manifest author may write either, so we normalize both
// sides to a single separator before comparing.
func normalize(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

// fqnTail returns the trailing path segment of a connector FQN. For
// `github://aileron/slack` it returns `slack`; for a bare `slack` it
// returns `slack`. The scheme (`github://`) is stripped first so a
// `://`-bearing FQN does not leak its scheme into the tail.
func fqnTail(fqn string) string {
	if i := strings.Index(fqn, "://"); i >= 0 {
		fqn = fqn[i+len("://"):]
	}
	if i := strings.LastIndexByte(fqn, '/'); i >= 0 {
		return fqn[i+1:]
	}
	return fqn
}

// Satisfies reports whether the given action satisfies the ref: the ref's
// connector segment equals the FQN tail of one of the action's connectors
// AND the ref's action segment equals the action's bare name (normalized).
func Satisfies(ref Ref, a Action) bool {
	if normalize(a.Name) != normalize(ref.Action) {
		return false
	}
	for _, c := range a.Requires.Connectors {
		if normalize(fqnTail(c.Name)) == normalize(ref.Connector) {
			return true
		}
	}
	return false
}

// Resolution is the outcome of resolving a set of refs against the
// installed actions.
type Resolution struct {
	// Satisfied lists the refs that matched an installed action.
	Satisfied []string
	// Unsatisfied lists the refs that matched no installed action. An
	// unsatisfied ref is a degrade signal, not an install failure.
	Unsatisfied []string
}

// AllSatisfied reports whether every ref resolved.
func (r Resolution) AllSatisfied() bool { return len(r.Unsatisfied) == 0 }

// Resolve partitions refs into satisfied and unsatisfied against the
// installed actions. A ref that fails to parse is reported as unsatisfied
// (a malformed ref cannot match any action). Order follows the input refs.
func Resolve(refs []string, actions []Action) Resolution {
	var res Resolution
	for _, raw := range refs {
		ref, err := ParseRef(raw)
		if err != nil {
			res.Unsatisfied = append(res.Unsatisfied, raw)
			continue
		}
		matched := false
		for _, a := range actions {
			if Satisfies(ref, a) {
				matched = true
				break
			}
		}
		if matched {
			res.Satisfied = append(res.Satisfied, raw)
		} else {
			res.Unsatisfied = append(res.Unsatisfied, raw)
		}
	}
	return res
}
