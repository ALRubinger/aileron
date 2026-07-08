package runtime

import (
	"fmt"
	"regexp"

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
// enforcement in EnforceConstraint). It returns the resolved argv plus the
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

// hostShapePattern re-validates an INSTANTIATED host to the host[:port] shape
// (no scheme, no path). It mirrors the daemon-side step-scope registry pattern
// (sandboxProxyStepScopeHostPattern in
// internal/app/handlers_sandbox_proxy_step_scopes.go): the runtime package
// cannot import the app package, so the shape is defined locally and the two
// must stay in sync. It is the FAIL-CLOSED re-check after substitution, so a
// constraint that admitted a value carrying a scheme, path, or other non-host
// byte can never reach the step-scope mint.
var hostShapePattern = regexp.MustCompile(`^[a-zA-Z0-9.-]+(:[0-9]+)?$`)

// instantiateHosts resolves a tool step's sealed host TEMPLATES into the
// concrete hosts the step-scope mint enforces (#1959), substituting each
// `{{ inputs.<name> }}` token with the string form of the resolved plan input
// (fmt.Sprintf("%v", v), matching instantiateCommand and the constraint
// enforcement in EnforceConstraint). Host tokens reference PLAN inputs, so the
// lookup is against values (ResolvedInputs.Values), the same source
// instantiateCommand uses.
//
// After substitution each host is re-validated to the host[:port] shape; a
// violation is a hard error so a bad instantiation never reaches the mint:
// instantiate first, then exact-match, never wildcard. A token naming an input
// with no resolved value is a hard error (decode rejects an undeclared input
// and every declared input is resolved at the launch boundary, so this only
// fires on a directly-constructed plan and fails closed).
//
// A token-free host instantiates to itself and still passes the shape re-check,
// so existing non-templated plans are unchanged. A nil/empty input yields nil,
// so a step with no sealed reach mints no hosts exactly as before.
func instantiateHosts(hosts []string, values map[string]any) ([]string, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	out := make([]string, len(hosts))
	for i, h := range hosts {
		inst, err := manifest.SubstituteInputs(h, func(name string) (string, bool) {
			v, ok := values[name]
			if !ok {
				return "", false
			}
			return fmt.Sprintf("%v", v), true
		})
		if err != nil {
			return nil, err
		}
		if !hostShapePattern.MatchString(inst) {
			return nil, fmt.Errorf("instantiated host %q (from template %q) is not a valid host[:port] (no scheme, no path)", inst, h)
		}
		out[i] = inst
	}
	return out, nil
}
