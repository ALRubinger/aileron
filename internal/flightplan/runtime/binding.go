package runtime

import (
	"fmt"
	"regexp"
)

// BindingKind discriminates the two reference forms in the closed binding
// grammar: a resolved input or a prior step output.
type BindingKind int

const (
	// BindInput references a declared input resolved at the launch boundary:
	// inputs.<name>.
	BindInput BindingKind = iota
	// BindStep references a named output of a prior step: steps.<id>.<out>.
	BindStep
)

// Binding is a parsed data-flow binding. A binding is a REFERENCE, never a
// value (schema $defs.binding). The closed grammar is the structural
// guarantee that step args are wiring, not embedded secrets.
type Binding struct {
	Kind BindingKind
	// Raw is the original binding string, retained for error messages and
	// audit summaries.
	Raw string
	// Name is the input name for BindInput.
	Name string
	// StepID and Output identify a prior step output for BindStep.
	StepID string
	Output string
}

// bindingPattern is the closed binding grammar from the schema
// ($defs.binding): inputs.<name> or steps.<id>.<output>. Names use the
// schema's [a-zA-Z0-9_-]+ character class. Anchored so a binding may carry
// nothing outside the grammar.
var bindingPattern = regexp.MustCompile(
	`^(?:inputs\.([a-zA-Z0-9_-]+)|steps\.([a-zA-Z0-9_-]+)\.([a-zA-Z0-9_-]+))$`)

// ParseBinding parses a binding string against the closed grammar. A string
// outside the grammar is a hard decode error: it could smuggle a literal
// value where only a reference is permitted.
func ParseBinding(raw string) (Binding, error) {
	m := bindingPattern.FindStringSubmatch(raw)
	if m == nil {
		return Binding{}, fmt.Errorf("binding %q is not a valid reference (want inputs.<name> or steps.<id>.<output>)", raw)
	}
	if m[1] != "" {
		return Binding{Kind: BindInput, Raw: raw, Name: m[1]}, nil
	}
	return Binding{Kind: BindStep, Raw: raw, StepID: m[2], Output: m[3]}, nil
}
