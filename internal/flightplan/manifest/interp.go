package manifest

import (
	"fmt"
	"regexp"
	"strings"
)

// This file is the single, dependency-free home for the embedded command
// interpolation grammar: a `{{ inputs.<name> }}` token that may appear inside a
// tool step's `command` argv element. It is DISTINCT from the closed
// whole-string binding grammar (runtime/binding.go), which parses an argument
// that is ENTIRELY a reference. An embedded token is text substitution WITHIN a
// larger literal string, so it needs its own scanner.
//
// The grammar lives in the leaf `manifest` package (beside the schema that
// defines the argv shape) so both the freeze-time guard (freeze/commandguard.go)
// and the launch-time instantiation (runtime/interp.go) share the EXACT same
// token recognition. If freeze deemed a token inert that launch would
// interpolate (or vice versa), the freeze-time injection guard would have a gap;
// a single grammar closes it. `runtime` imports `freeze` imports `manifest`, so
// this leaf placement is the only one both consumers can reach.
//
// A token is: `{{` + optional surrounding whitespace + `inputs.<name>` +
// optional surrounding whitespace + `}}`, where `<name>` is the schema's input
// name character class `[a-zA-Z0-9_-]+`. A string may hold any number of tokens
// interleaved with literal text, or none at all.

// tokenBodyPattern matches a token's inner body (already brace-stripped and
// whitespace-trimmed): `inputs.<name>`, capturing the input name. Anchored so a
// body carrying anything outside the grammar (a `steps.…` reference, arbitrary
// text, trailing junk) does not match and is reported as a malformed token.
var tokenBodyPattern = regexp.MustCompile(`^inputs\.([a-zA-Z0-9_-]+)$`)

// segment is one span of a scanned command element: either a literal run of
// text or a resolved token naming an input.
type segment struct {
	// literal is the verbatim text for a non-token span.
	literal string
	// name is the input name for a token span.
	name string
	// isToken discriminates a token span (name meaningful) from a literal span
	// (literal meaningful).
	isToken bool
}

// scan walks a command element into ordered literal/token segments, validating
// the brace grammar as it goes. It fails closed on any malformed brace shape:
//
//   - a `}}` with no preceding `{{` (a stray closer);
//   - a `{{` with no following `}}` (an unbalanced opener);
//   - a token whose body is not `inputs.<name>` (a non-inputs reference or
//     arbitrary text).
//
// A bare regexp replace would silently swallow these; scanning and rejecting
// them keeps the freeze-time guard honest (a malformed token never slips through
// as inert literal text).
func scan(s string) ([]segment, error) {
	var segs []segment
	i := 0
	for i < len(s) {
		open := strings.Index(s[i:], "{{")
		closer := strings.Index(s[i:], "}}")
		// A `}}` reached before the next `{{` (or with no `{{` at all) is a
		// stray closer: unbalanced braces.
		if closer != -1 && (open == -1 || closer < open) {
			return nil, fmt.Errorf("malformed interpolation in %q: '}}' has no matching '{{'", s)
		}
		if open == -1 {
			// No further token opener and (checked above) no stray closer: the
			// rest is literal.
			segs = append(segs, segment{literal: s[i:]})
			break
		}
		openAbs := i + open
		if openAbs > i {
			segs = append(segs, segment{literal: s[i:openAbs]})
		}
		rest := s[openAbs+2:]
		c := strings.Index(rest, "}}")
		if c == -1 {
			return nil, fmt.Errorf("malformed interpolation in %q: '{{' has no matching '}}'", s)
		}
		body := rest[:c]
		// A `{{` inside the body means nested/overlapping openers: malformed.
		if strings.Contains(body, "{{") {
			return nil, fmt.Errorf("malformed interpolation in %q: nested '{{' inside a token", s)
		}
		name, err := parseTokenBody(body, s)
		if err != nil {
			return nil, err
		}
		segs = append(segs, segment{name: name, isToken: true})
		i = openAbs + 2 + c + 2
	}
	return segs, nil
}

// parseTokenBody validates a brace-stripped token body and returns the
// referenced input name. The body is whitespace-trimmed first (a token may
// carry surrounding whitespace: `{{ inputs.x }}`), then must match
// `inputs.<name>` exactly. whole is the full element, for the error message.
func parseTokenBody(body, whole string) (string, error) {
	trimmed := strings.TrimSpace(body)
	m := tokenBodyPattern.FindStringSubmatch(trimmed)
	if m == nil {
		return "", fmt.Errorf("malformed interpolation in %q: token body %q is not inputs.<name>", whole, trimmed)
	}
	return m[1], nil
}

// CommandInputRefs extracts the input names referenced by `{{ inputs.<name> }}`
// tokens in a command element, in order of appearance, validating the token
// grammar. A string with no tokens is valid and returns no refs. A malformed
// brace shape or a non-inputs token body is a hard error.
func CommandInputRefs(s string) ([]string, error) {
	segs, err := scan(s)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, seg := range segs {
		if seg.isToken {
			refs = append(refs, seg.name)
		}
	}
	return refs, nil
}

// SubstituteInputs replaces every `{{ inputs.<name> }}` token in a command
// element with the value lookup returns for that name, leaving literal text
// unchanged. It errors on a malformed token grammar (via scan) or when lookup
// reports a name it cannot resolve. Literal text is copied verbatim, so
// substitution never splits, word-expands, or shell-interprets the element: the
// argv-array shape is preserved.
func SubstituteInputs(s string, lookup func(name string) (string, bool)) (string, error) {
	segs, err := scan(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, seg := range segs {
		if !seg.isToken {
			b.WriteString(seg.literal)
			continue
		}
		v, ok := lookup(seg.name)
		if !ok {
			return "", fmt.Errorf("interpolation in %q references input %q, which has no resolved value", s, seg.name)
		}
		b.WriteString(v)
	}
	return b.String(), nil
}
