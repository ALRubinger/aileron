package runtime

import (
	"bytes"
	"fmt"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"gopkg.in/yaml.v3"
)

// DecodeError reports a strict-decode refusal. A decode error means the plan
// is structurally invalid and the runtime refuses to run any step. The
// message names the offending element so the author can fix it.
type DecodeError struct {
	Reason string
}

func (e *DecodeError) Error() string { return "flightplan decode: " + e.Reason }

func decodeErrf(format string, args ...any) error {
	return &DecodeError{Reason: fmt.Sprintf(format, args...)}
}

// Decode builds a typed, validated Plan from a parsed manifest. It is strict:
// an unknown step kind, a malformed binding, a duplicate id, a binding that
// references an undeclared input or absent step, a materializesOutput naming
// an undeclared output, or a cycle in the steps.* dependency graph is a hard
// refusal returned as a *DecodeError. No step runs unless Decode succeeds.
//
// An instruction-only manifest (no aileron block) has no composition to run
// and is refused: Launch executes a step graph, and there is none.
//
// Decode takes no image pins: a `kind: tool` step (#1829) runs inside the
// plan's single pinned environment, so the pins are a boot concern (the
// load path threads them to LoadedPlan.ResolvedImages), never a decode
// input.
func Decode(m *manifest.Manifest) (*Plan, error) {
	if m == nil {
		return nil, decodeErrf("nil manifest")
	}
	if m.InstructionOnly {
		return nil, decodeErrf("manifest is instruction-only; there is no step graph to launch")
	}

	p := &Plan{
		Name:    m.Name,
		Actions: map[string]Action{},
		Outputs: map[string]Output{},
	}

	if err := decodeActions(m, p); err != nil {
		return nil, err
	}
	if err := decodeInputs(m, p); err != nil {
		return nil, err
	}
	if err := decodeOutputs(m, p); err != nil {
		return nil, err
	}
	if err := decodeSteps(m, p); err != nil {
		return nil, err
	}
	return p, nil
}

// remarshal round-trips a YAML-decoded `any` (already a map[string]any from
// the manifest decode) back through YAML into a strict typed struct. Decode is
// strict (KnownFields): the DTOs model the schema's full closed shapes, so an
// unexpected field is a decode error rather than a silently-dropped typo. This
// is the runtime's structural defense-in-depth over the loosely-typed manifest
// []any blocks.
func remarshal(v any, dst any) error {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func decodeActions(m *manifest.Manifest, p *Plan) error {
	for _, ar := range m.Aileron.Requires.Actions {
		var tc trustContractDTO
		if err := remarshal(ar.TrustContract, &tc); err != nil {
			return decodeErrf("action %q: parse trust contract: %v", ar.Ref, err)
		}
		act, err := tc.toAction(ar.Ref)
		if err != nil {
			return err
		}
		if _, dup := p.Actions[ar.Ref]; dup {
			return decodeErrf("duplicate action ref %q", ar.Ref)
		}
		p.Actions[ar.Ref] = act
	}
	return nil
}

func decodeInputs(m *manifest.Manifest, p *Plan) error {
	seen := map[string]bool{}
	for i, raw := range m.Aileron.Inputs {
		var dto inputDTO
		if err := remarshal(raw, &dto); err != nil {
			return decodeErrf("input %d: %v", i, err)
		}
		in, err := dto.toInput()
		if err != nil {
			return err
		}
		if seen[in.Name] {
			return decodeErrf("duplicate input name %q", in.Name)
		}
		seen[in.Name] = true
		p.Inputs = append(p.Inputs, in)
	}
	return nil
}

func decodeOutputs(m *manifest.Manifest, p *Plan) error {
	for i, raw := range m.Aileron.Outputs {
		var dto outputDTO
		if err := remarshal(raw, &dto); err != nil {
			return decodeErrf("output %d: %v", i, err)
		}
		out, err := dto.toOutput()
		if err != nil {
			return err
		}
		if _, dup := p.Outputs[out.Name]; dup {
			return decodeErrf("duplicate output name %q", out.Name)
		}
		p.Outputs[out.Name] = out
	}
	return nil
}

func decodeSteps(m *manifest.Manifest, p *Plan) error {
	if len(m.Aileron.Steps) == 0 {
		return decodeErrf("manifest declares no steps; there is no step graph to launch")
	}
	inputNames := map[string]bool{}
	for _, in := range p.Inputs {
		inputNames[in.Name] = true
	}

	stepIndex := map[string]int{}
	for i, raw := range m.Aileron.Steps {
		var dto stepDTO
		if err := remarshal(raw, &dto); err != nil {
			return decodeErrf("step %d: %v", i, err)
		}
		step, err := dto.toStep()
		if err != nil {
			return err
		}
		if _, dup := stepIndex[step.ID]; dup {
			return decodeErrf("duplicate step id %q", step.ID)
		}
		stepIndex[step.ID] = i
		p.Steps = append(p.Steps, step)
	}

	if err := validateReferences(p, inputNames, stepIndex); err != nil {
		return err
	}
	order, err := topoSort(p, stepIndex)
	if err != nil {
		return err
	}
	p.Order = order
	return nil
}

// validateReferences checks every step binding and materializesOutput against
// the declared inputs, the prior-step outputs, and the declared outputs. A
// binding to an undeclared input or an absent step/output is a hard refusal.
// An action-call whose ref is not in requires.actions is refused (the access
// scope is the boundary).
func validateReferences(p *Plan, inputNames map[string]bool, stepIndex map[string]int) error {
	// stepOutputs lets a binding confirm the referenced step declares the
	// named output. Built across all steps first so a forward reference is
	// caught as a graph-shape error by topoSort, not mistaken for a missing
	// output here.
	stepOutputs := map[string]map[string]bool{}
	for _, s := range p.Steps {
		outs := map[string]bool{}
		for _, o := range s.Outputs {
			outs[o] = true
		}
		stepOutputs[s.ID] = outs
	}

	for _, s := range p.Steps {
		if s.Kind == KindActionCall {
			if _, ok := p.Actions[s.ActionRef]; !ok {
				return decodeErrf("step %q calls action %q not declared in requires.actions", s.ID, s.ActionRef)
			}
		}
		for argName, b := range s.binds() {
			switch b.Kind {
			case BindInput:
				if !inputNames[b.Name] {
					return decodeErrf("step %q binding %q references undeclared input %q", s.ID, argName, b.Name)
				}
			case BindStep:
				outs, ok := stepOutputs[b.StepID]
				if !ok {
					return decodeErrf("step %q binding %q references unknown step %q", s.ID, argName, b.StepID)
				}
				if !outs[b.Output] {
					return decodeErrf("step %q binding %q references output %q not produced by step %q", s.ID, argName, b.Output, b.StepID)
				}
				if b.StepID == s.ID {
					return decodeErrf("step %q binding %q references its own output", s.ID, argName)
				}
			}
		}
		if s.MaterializesOutput != "" {
			if _, ok := p.Outputs[s.MaterializesOutput]; !ok {
				return decodeErrf("step %q materializes undeclared output %q", s.ID, s.MaterializesOutput)
			}
		}
		// Each `{{ inputs.<name> }}` command token must name a declared input
		// (#1958), mirroring the BindInput undeclared-input check above. The
		// grammar itself was validated at populateToolStep; here the resolved
		// refs are checked against the declared set so an unknown input is
		// refused at launch load, not only at freeze. The constraint-presence
		// guard stays freeze-only per the plan.
		for _, elem := range s.Command {
			refs, err := commandInputRefs(elem)
			if err != nil {
				// The grammar was already validated at decode of the step; a
				// re-scan error here is defensive and reported the same way.
				return decodeErrf("step %q command element %q: %v", s.ID, elem, err)
			}
			for _, ref := range refs {
				if !inputNames[ref] {
					return decodeErrf("step %q command references undeclared input %q", s.ID, ref)
				}
			}
		}
		// Each `{{ inputs.<name> }}` token in a tool step's trustContract host
		// must name a declared input (#1959), mirroring the command check above.
		// The grammar was validated at populateToolStep; here the resolved refs
		// are checked against the declared set so an unknown input is refused at
		// launch load, not only at freeze. Host templates are instantiated only
		// on tool steps (a per-action contract's hosts are never substituted),
		// so this is scoped to a tool step's own contract. The constraint-
		// presence guard stays freeze-only per the plan.
		if s.TrustContract != nil {
			for _, host := range s.TrustContract.Hosts {
				refs, err := commandInputRefs(host)
				if err != nil {
					return decodeErrf("step %q trustContract host %q: %v", s.ID, host, err)
				}
				for _, ref := range refs {
					if !inputNames[ref] {
						return decodeErrf("step %q trustContract host references undeclared input %q", s.ID, ref)
					}
				}
			}
		}
	}
	return nil
}

// topoSort returns a deterministic topological order of step indices using
// steps.* edges ONLY. Input-source reads are not steps and not graph edges,
// so a `source` input that calls the same action a step calls never trips the
// cycle check (#1523 boundary). A cycle is a hard refusal.
//
// Determinism: ties are broken by declaration order (the manifest step
// order), so the same plan always yields the same walk order.
func topoSort(p *Plan, stepIndex map[string]int) ([]int, error) {
	n := len(p.Steps)
	// deps[i] is the set of step indices step i depends on (its steps.*
	// bindings). indegree counts unresolved deps.
	deps := make([]map[int]bool, n)
	indegree := make([]int, n)
	// dependents[j] lists steps that depend on j, so resolving j decrements
	// their indegree.
	dependents := make([][]int, n)

	for i, s := range p.Steps {
		deps[i] = map[int]bool{}
		for _, b := range s.binds() {
			if b.Kind != BindStep {
				continue
			}
			j := stepIndex[b.StepID]
			if !deps[i][j] {
				deps[i][j] = true
				indegree[i]++
				dependents[j] = append(dependents[j], i)
			}
		}
	}

	// Kahn's algorithm with a declaration-order ready set so the output is
	// deterministic.
	order := make([]int, 0, n)
	resolved := make([]bool, n)
	for len(order) < n {
		next := -1
		for i := 0; i < n; i++ {
			if !resolved[i] && indegree[i] == 0 {
				next = i
				break
			}
		}
		if next == -1 {
			// No indegree-zero step remains but steps are unresolved: a cycle.
			return nil, decodeErrf("step graph has a cycle among steps.* edges; %s", cycleMembers(resolved, p))
		}
		resolved[next] = true
		order = append(order, next)
		for _, d := range dependents[next] {
			indegree[d]--
		}
	}
	return order, nil
}

// cycleMembers names the unresolved steps for a cycle error message.
func cycleMembers(resolved []bool, p *Plan) string {
	var members []string
	for i, r := range resolved {
		if !r {
			members = append(members, p.Steps[i].ID)
		}
	}
	return fmt.Sprintf("unresolved steps: %v", members)
}
