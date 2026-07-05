package runtime

import (
	"fmt"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// This file is the runtime consumer's thin adapter over the dependency-free
// command interpolation grammar in the leaf `manifest` package. The grammar
// itself lives there (beside the schema, reachable by both freeze and runtime);
// this adapter binds it to the runtime's resolved-input map and argv shape.

// commandInputRefs re-exports the manifest grammar's ref extractor so the
// decode-time defense-in-depth checks (dto.go, decode.go) reach the SAME token
// recognition the launch-time instantiation and the freeze-time guard use,
// without importing the manifest package name at every call site.
func commandInputRefs(s string) ([]string, error) {
	return manifest.CommandInputRefs(s)
}

// instantiateCommand resolves a tool step's TEMPLATE argv into the argv the
// runner execs, substituting each `{{ inputs.<name> }}` token with the string
// form of the resolved plan input (fmt.Sprintf("%v", v), matching the constraint
// enforcement in enforceConstraint). It returns the resolved argv plus the
// sorted-by-appearance indices of the elements that carried a token (the
// input-derived marker the audit records).
//
// Command tokens reference PLAN inputs (`inputs.<name>`), never the step's
// resolved bindings, so the lookup is against values (ResolvedInputs.Values),
// not the step's bound arguments. A token naming an input with no resolved value
// is a hard error: decode rejects an undeclared input, and every declared input
// is resolved at the launch boundary, so this only fires on a directly
// constructed plan and fails closed rather than exec'ing a half-built argv.
//
// A token-free element passes through verbatim and is not marked derived, so a
// plan with no interpolation execs (and audits) exactly the argv it declared.
func instantiateCommand(cmd []string, values map[string]any) (resolved []string, derivedIdx []int, err error) {
	resolved = make([]string, len(cmd))
	for i, elem := range cmd {
		refs, err := manifest.CommandInputRefs(elem)
		if err != nil {
			return nil, nil, err
		}
		if len(refs) == 0 {
			resolved[i] = elem
			continue
		}
		out, err := manifest.SubstituteInputs(elem, func(name string) (string, bool) {
			v, ok := values[name]
			if !ok {
				return "", false
			}
			return fmt.Sprintf("%v", v), true
		})
		if err != nil {
			return nil, nil, err
		}
		resolved[i] = out
		derivedIdx = append(derivedIdx, i)
	}
	return resolved, derivedIdx, nil
}
