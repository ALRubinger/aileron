package runtime

import (
	"bytes"
	"fmt"
	"regexp"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"gopkg.in/yaml.v3"
)

// rung3EnvKey is the executionEnvironment key naming the rung-3 per-step
// sibling-image dispatch block.
const rung3EnvKey = "rung3PerStepImages"

// imageDigestPattern is the content-addressed digest shape a rung-3 pin's
// digest must match. It mirrors the freeze-side digest guard so a tag-shaped
// value can never survive into a per-step dispatch.
var imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

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
// Decode carries no rung-3 image pins, so a manifest that declares a rung-3
// per-step execution environment is refused here (its steps have no verified
// pin to dispatch to). The runtime load path uses DecodeWithImages to thread
// the verified pins; Decode is the pin-free entry point tests and non-rung-3
// callers use.
func Decode(m *manifest.Manifest) (*Plan, error) {
	return DecodeWithImages(m, nil)
}

// DecodeWithImages builds a typed Plan and, when the manifest declares a rung-3
// per-step execution environment, attaches each step's pinned sibling-tool
// dispatch (image + mount + collect) from the verified image pins (#1733). The
// pins are the verified `ref@digest` set from the signed lock; a rung-3 step
// whose pin is absent/unpinned is a hard *DecodeError so no tag-shaped or
// floating ref is ever dispatched. Non-rung-3 plans ignore pins entirely.
func DecodeWithImages(m *manifest.Manifest, pins []freeze.ImagePin) (*Plan, error) {
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
	if err := decodeToolDispatch(m, p, pins); err != nil {
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

// decodeToolDispatch attaches a rung-3 per-step tool dispatch to each declaring
// graph step from the verified image pins. It is a no-op when the manifest
// declares no rung-3 execution environment. For a manifest that does:
//
//   - each rung3PerStepImages.steps[] entry MUST declare an `id` that names a
//     graph step (the dispatch has to attach to a specific step in the graph);
//   - each entry's verified pin is looked up by StepID (the pin freeze stamped
//     from the same declared id), so association keys on id, never on the
//     mutable image tag (the #1739 collision);
//   - a rung-3 step whose pin is absent/unpinned is a hard *DecodeError, so no
//     tag-shaped or floating ref is ever dispatched (acceptance);
//   - the attached Image is the pin's `ref@digest` (content-addressed), and the
//     optional mount/collect paths thread through from the declaration.
func decodeToolDispatch(m *manifest.Manifest, p *Plan, pins []freeze.ImagePin) error {
	raw, ok := m.Aileron.Requires.ExecutionEnvironment[rung3EnvKey]
	if !ok {
		return nil
	}

	var env rung3EnvDTO
	if err := remarshal(raw, &env); err != nil {
		return decodeErrf("rung3PerStepImages: %v", err)
	}
	if len(env.Steps) == 0 {
		return decodeErrf("rung3PerStepImages declares no steps")
	}

	// Index the graph steps by id so a rung-3 declaration attaches to exactly
	// one step.
	stepByID := map[string]*Step{}
	for i := range p.Steps {
		stepByID[p.Steps[i].ID] = &p.Steps[i]
	}

	// Index the verified pins by StepID. A pin with no StepID is a rung-1/rung-2
	// whole-plan pin and never participates in per-step association.
	pinByStepID := map[string]freeze.ImagePin{}
	for _, pin := range pins {
		if pin.StepID != "" {
			pinByStepID[pin.StepID] = pin
		}
	}

	seen := map[string]bool{}
	for i, sd := range env.Steps {
		if sd.Image == "" {
			return decodeErrf("rung3PerStepImages step %d: missing image", i)
		}
		if sd.ID == "" {
			return decodeErrf("rung3PerStepImages step %d (image %q): a per-step tool dispatch must declare an id naming the graph step it attaches to", i, sd.Image)
		}
		if seen[sd.ID] {
			return decodeErrf("rung3PerStepImages declares duplicate step id %q", sd.ID)
		}
		seen[sd.ID] = true

		step, ok := stepByID[sd.ID]
		if !ok {
			return decodeErrf("rung3PerStepImages step %q names no step in the graph", sd.ID)
		}
		// A tool dispatch replaces the step's in-process execution. Attaching one
		// to an action-call would bypass the sealed action boundary (approval,
		// idempotency, redaction, audit), so an action-call step must NOT carry a
		// rung-3 dispatch: those effects go through requires.actions, not a tool
		// image. Refuse rather than silently skip the boundary.
		if step.Kind == KindActionCall {
			return decodeErrf("rung3PerStepImages step %q attaches a tool dispatch to an action-call step; action effects go through the sealed action boundary, not a tool image", sd.ID)
		}

		pin, ok := pinByStepID[sd.ID]
		if !ok {
			return decodeErrf("rung3PerStepImages step %q references an unpinned image %q; freeze must pin every rung-3 image by digest before launch", sd.ID, sd.Image)
		}
		// Defense in depth: freeze already refuses a non-digest pin, but re-check
		// here so a tag-shaped digest can never survive into a dispatch.
		if !imageDigestPattern.MatchString(pin.Digest) {
			return decodeErrf("rung3PerStepImages step %q pin %q is not a sha256: digest; only digest-pinned images may be dispatched", sd.ID, pin.Digest)
		}

		// A mount/collect block declared with an empty path is malformed: the
		// schema requires minLength:1, so an empty path here means the manifest
		// was tampered past the schema gate. Refuse rather than mount nothing.
		mountPath := sd.mountPath()
		if sd.Mount != nil && mountPath == "" {
			return decodeErrf("rung3PerStepImages step %q declares a mount with an empty path", sd.ID)
		}
		collectPath := sd.collectPath()
		if sd.Collect != nil && collectPath == "" {
			return decodeErrf("rung3PerStepImages step %q declares a collect with an empty path", sd.ID)
		}

		step.ToolDispatch = &ToolDispatch{
			Image:       imageRef(pin.Ref, pin.Digest),
			MountPath:   mountPath,
			CollectPath: collectPath,
		}
	}
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
