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

// promptBodyPattern matches a token's inner body against the plan's FULL
// binding grammar, which a prompt (unlike a command/host) uses: either
// `inputs.<name>` OR `steps.<id>.<output>`. It is deliberately SEPARATE from
// tokenBodyPattern so the command/host grammar keeps rejecting `steps.…`; the
// two grammars stay independent. Character classes mirror runtime/binding.go's
// bindingPattern and the schema's $defs.binding (`[a-zA-Z0-9_-]+`). Group 1 is
// the input name for the inputs form; groups 2 and 3 are the step id and output
// for the steps form.
var promptBodyPattern = regexp.MustCompile(
	`^(?:inputs\.([a-zA-Z0-9_-]+)|steps\.([a-zA-Z0-9_-]+)\.([a-zA-Z0-9_-]+))$`)

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

// scanTokens walks a string's `{{ … }}` tokens with the same brace grammar as
// scan (same fail-closed rules for stray closers, unbalanced openers, and
// nested `{{`), invoking onBody with each raw (untrimmed) token body. Literal
// spans are skipped. It is the grammar-agnostic core the prompt extractor
// reuses: onBody applies the caller's per-token body grammar and returns an
// error to fail the scan closed. scan (command/host) keeps its own segment
// accumulation because it also needs literal spans for SubstituteInputs.
func scanTokens(s string, onBody func(body string) error) error {
	i := 0
	for i < len(s) {
		open := strings.Index(s[i:], "{{")
		closer := strings.Index(s[i:], "}}")
		// A `}}` reached before the next `{{` (or with no `{{` at all) is a
		// stray closer: unbalanced braces.
		if closer != -1 && (open == -1 || closer < open) {
			return fmt.Errorf("malformed interpolation in %q: '}}' has no matching '{{'", s)
		}
		if open == -1 {
			// No further token opener and (checked above) no stray closer: the
			// rest is literal.
			break
		}
		openAbs := i + open
		rest := s[openAbs+2:]
		c := strings.Index(rest, "}}")
		if c == -1 {
			return fmt.Errorf("malformed interpolation in %q: '{{' has no matching '}}'", s)
		}
		body := rest[:c]
		// A `{{` inside the body means nested/overlapping openers: malformed.
		if strings.Contains(body, "{{") {
			return fmt.Errorf("malformed interpolation in %q: nested '{{' inside a token", s)
		}
		if err := onBody(body); err != nil {
			return err
		}
		i = openAbs + 2 + c + 2
	}
	return nil
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

// PromptRefKind discriminates the two binding forms a prompt token may carry.
type PromptRefKind int

const (
	// PromptRefInput is a `{{ inputs.<name> }}` token.
	PromptRefInput PromptRefKind = iota
	// PromptRefStep is a `{{ steps.<id>.<output> }}` token.
	PromptRefStep
)

// PromptRef is one binding reference extracted from a prompt template. Kind
// discriminates which fields are meaningful: Input for PromptRefInput; StepID
// and Output for PromptRefStep. Raw carries the whitespace-trimmed token body
// as written, for error messages.
type PromptRef struct {
	Kind   PromptRefKind
	Input  string
	StepID string
	Output string
	Raw    string
}

// PromptBindingRefs extracts the binding references in a prompt template, in
// order of appearance, validating the token grammar. Unlike CommandInputRefs
// (which admits ONLY `inputs.<name>`), a prompt uses the plan's FULL binding
// grammar: `{{ inputs.<name> }}` AND `{{ steps.<id>.<output> }}`. A string with
// no tokens is valid and returns no refs. A malformed brace shape or a token
// body that is neither binding form is a hard error.
//
// It shares the brace-walking scanner with CommandInputRefs so freeze and any
// future runtime prompt resolution recognize tokens identically; only the
// per-token body grammar differs (promptBodyPattern vs tokenBodyPattern).
func PromptBindingRefs(s string) ([]PromptRef, error) {
	var refs []PromptRef
	err := scanTokens(s, func(body string) error {
		trimmed := strings.TrimSpace(body)
		m := promptBodyPattern.FindStringSubmatch(trimmed)
		if m == nil {
			return fmt.Errorf("malformed interpolation in %q: token body %q is not inputs.<name> or steps.<id>.<output>", s, trimmed)
		}
		if m[1] != "" {
			refs = append(refs, PromptRef{Kind: PromptRefInput, Input: m[1], Raw: trimmed})
			return nil
		}
		refs = append(refs, PromptRef{Kind: PromptRefStep, StepID: m[2], Output: m[3], Raw: trimmed})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
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
