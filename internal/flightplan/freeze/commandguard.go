package freeze

import (
	"fmt"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// lintCommandInterpolation is the freeze-time guard over `{{ inputs.<name> }}`
// tokens embedded in a tool step's `command` argv. Freeze seals the TEMPLATE
// argv (it rides the signed frontmatter bytes) and launch execs the
// INSTANTIATED argv, so a token in a sealed exec position is an injection
// surface unless the input it names is CONSTRAINED (an enum allow-set or a
// pattern). This guard fails the freeze closed on:
//
//   - a malformed token grammar (unbalanced braces, a non-inputs body): the
//     same rejection the launch-time scanner makes, so freeze never deems a
//     token inert that launch would interpolate;
//   - a token naming an input UNKNOWN at freeze (mirrors the launch-time
//     undeclared-input refusal);
//   - a token naming a KNOWN but UNCONSTRAINED input: the injection-surface
//     guard, freeze-only per the plan. An unconstrained value flowing into a
//     signed argv could smuggle arbitrary arguments into the sealed exec.
//
// It reads the RAW manifest maps (freeze operates on manifest.Manifest, not the
// runtime's typed Plan) and shares the exact token grammar the runtime uses via
// the leaf manifest package.
func lintCommandInterpolation(m *manifest.Manifest) error {
	declared, constrained := inputConstraintSets(m)
	for i, raw := range m.Aileron.Steps {
		step, ok := raw.(map[string]any)
		if !ok {
			// Lint's earlier pass already rejects a non-mapping step; keep the
			// backstop rather than panic on a direct construct.
			return &LintError{StepID: stepIDOf(raw), Reason: fmt.Sprintf("step %d is not a mapping", i)}
		}
		if scalarString(step["kind"]) != "tool" {
			continue
		}
		id := scalarString(step["id"])
		cmd, ok := step["command"].([]any)
		if !ok {
			// The schema requires a command array on a tool step; its shape is
			// validated at manifest parse, so a non-array here is not this
			// guard's concern.
			continue
		}
		for _, el := range cmd {
			elem, ok := el.(string)
			if !ok {
				continue
			}
			refs, err := manifest.CommandInputRefs(elem)
			if err != nil {
				return &LintError{StepID: id, Reason: fmt.Sprintf("command element %q: %v", elem, err)}
			}
			for _, ref := range refs {
				if !declared[ref] {
					return &LintError{StepID: id, Reason: fmt.Sprintf("command references undeclared input %q", ref)}
				}
				if !constrained[ref] {
					return &LintError{StepID: id, Reason: fmt.Sprintf("command references input %q which declares no constraint; an unconstrained input in a sealed command position is an injection surface (add an enum or pattern constraint)", ref)}
				}
			}
		}
	}
	return nil
}

// lintHostInterpolation is the freeze-time guard over `{{ inputs.<name> }}`
// tokens embedded in a tool step's `trustContract.hosts` entries (#1959). It is
// the exact mirror of lintCommandInterpolation for the host axis: freeze seals
// the TEMPLATE host into lock.stepTrust (covered by the content hash + detached
// signature) and launch instantiates the CONCRETE host before minting the
// step-scope proxy credential, so a token in a sealed host position is an
// injection surface unless the input it names is CONSTRAINED (an enum allow-set
// or a pattern). This guard fails the freeze closed on:
//
//   - a malformed token grammar (unbalanced braces, a non-inputs body): the
//     same rejection the launch-time scanner makes, so freeze never deems a
//     host token inert that launch would interpolate;
//   - a token naming an input UNKNOWN at freeze (mirrors the launch-time
//     undeclared-input refusal in runtime/decode.go);
//   - a token naming a KNOWN but UNCONSTRAINED input: the injection-surface
//     guard, freeze-only per the plan. An unconstrained value flowing into a
//     signed host could steer the step's egress to an attacker-chosen host.
//
// Scope: only `kind: tool` steps are walked. A per-action (connector
// action-ref) trustContract is never instantiated (no plan-input substitution
// path reaches it), so an input token in one of its hosts would ride in inert
// as the literal `{{ ... }}` string: a footgun, not a constrained-input axis.
// lintActionHostLiterals guards that separately by hard-rejecting any token in
// a per-action host. It reads the RAW manifest maps and shares the exact token
// grammar the runtime uses via the leaf manifest package.
func lintHostInterpolation(m *manifest.Manifest) error {
	declared, constrained := inputConstraintSets(m)
	for i, raw := range m.Aileron.Steps {
		step, ok := raw.(map[string]any)
		if !ok {
			// Lint's earlier pass already rejects a non-mapping step; keep the
			// backstop rather than panic on a direct construct.
			return &LintError{StepID: stepIDOf(raw), Reason: fmt.Sprintf("step %d is not a mapping", i)}
		}
		if scalarString(step["kind"]) != "tool" {
			continue
		}
		id := scalarString(step["id"])
		tc, ok := step["trustContract"].(map[string]any)
		if !ok {
			// A tool step with no trustContract (or a malformed one) declares no
			// host reach for this guard; the schema and steptrust sealing reject
			// a malformed contract shape, so a non-mapping here is not this
			// guard's concern.
			continue
		}
		hosts, ok := tc["hosts"].([]any)
		if !ok {
			// Missing/non-array hosts are a schema and steptrust-seal error,
			// caught there; this guard only inspects present host strings.
			continue
		}
		for _, h := range hosts {
			host, ok := h.(string)
			if !ok {
				continue
			}
			refs, err := manifest.CommandInputRefs(host)
			if err != nil {
				return &LintError{StepID: id, Reason: fmt.Sprintf("trustContract host %q: %v", host, err)}
			}
			for _, ref := range refs {
				if !declared[ref] {
					return &LintError{StepID: id, Reason: fmt.Sprintf("trustContract host references undeclared input %q", ref)}
				}
				if !constrained[ref] {
					return &LintError{StepID: id, Reason: fmt.Sprintf("trustContract host references input %q which declares no constraint; an unconstrained input in a sealed host position is an injection surface (add an enum or pattern constraint)", ref)}
				}
			}
		}
	}
	return nil
}

// lintActionHostLiterals is the freeze-time guard over `{{ inputs.<name> }}`
// tokens embedded in a PER-ACTION (requires.actions[]) trustContract host
// (#1965). Unlike a tool step's hosts, a per-action host is never instantiated:
// no plan-input substitution path reaches it, so a token there rides in inert
// as the literal `{{ ... }}` string and the declared reach it appears to grant
// is silently wrong. The shared-def schema relaxation that lets tool-step hosts
// carry a token also makes the token syntactically valid here, so freeze must
// reject it explicitly rather than seal a footgun. Per-action hosts must be
// literal.
//
// It reads the typed manifest.ActionRequirement.TrustContract map (freeze runs
// on manifest.Manifest, not the runtime's typed Plan) and shares the exact
// token grammar the runtime uses via the leaf manifest package.
func lintActionHostLiterals(m *manifest.Manifest) error {
	for _, a := range m.Aileron.Requires.Actions {
		if a.TrustContract == nil {
			continue
		}
		hosts, ok := a.TrustContract["hosts"].([]any)
		if !ok {
			// Missing/non-array hosts are a schema error caught at parse; this
			// guard only inspects present host strings.
			continue
		}
		for _, h := range hosts {
			host, ok := h.(string)
			if !ok {
				continue
			}
			refs, err := manifest.CommandInputRefs(host)
			if err != nil {
				return &LintError{Reason: fmt.Sprintf("requires.actions[%q] trustContract host %q: %v", a.Ref, host, err)}
			}
			if len(refs) > 0 {
				return &LintError{Reason: fmt.Sprintf("trustContract host references input token %q, but host interpolation is tool-step-only (per-action requires.actions[] trustContract hosts must be literal)", host)}
			}
		}
	}
	return nil
}

// inputConstraintSets walks the raw manifest inputs into the set of declared
// input names and the subset that materially declare a constraint (a non-empty
// enum or a non-whitespace pattern). It reads untyped maps because freeze runs
// before the runtime's typed decode; the runtime's toConstraint is the
// authoritative validator, so this only needs the presence signal.
func inputConstraintSets(m *manifest.Manifest) (declared, constrained map[string]bool) {
	declared = map[string]bool{}
	constrained = map[string]bool{}
	for _, raw := range m.Aileron.Inputs {
		in, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := scalarString(in["name"])
		if name == "" {
			continue
		}
		declared[name] = true
		if inputDeclaresConstraint(in) {
			constrained[name] = true
		}
	}
	return declared, constrained
}

// inputDeclaresConstraint reports whether a raw input map carries a materially
// non-empty constraint: an enum with at least one entry or a pattern that is
// more than whitespace. It mirrors the runtime toConstraint presence test so
// freeze and launch agree on which inputs count as constrained.
func inputDeclaresConstraint(in map[string]any) bool {
	c, ok := in["constraint"].(map[string]any)
	if !ok {
		return false
	}
	if enum, ok := c["enum"].([]any); ok && len(enum) > 0 {
		return true
	}
	if strings.TrimSpace(scalarString(c["pattern"])) != "" {
		return true
	}
	return false
}
