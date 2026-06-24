package runtime

import (
	"context"
	"fmt"
)

// stepResult is the named-output set one step produced, keyed by output name.
type stepResult map[string]any

// execState carries the accumulating graph state through the walk.
type execState struct {
	inputs     ResolvedInputs
	stepOutput map[string]stepResult // steps.<id> → its outputs
	// dispatches records the per-action-call outcome (for audit + the run
	// result), in execution order.
	dispatches []actionDispatch
	// artifacts records materialized output artifacts, in execution order.
	artifacts []Artifact
}

// actionDispatch records one action-call's enforced outcome for the audit.
type actionDispatch struct {
	StepID            string
	ActionRef         string
	Effect            Effect
	ApprovalRequested bool
	Approved          bool
	Result            map[string]any
	AuditFields       []string
	Sink              string
}

// executor walks the topologically-ordered step graph. It has EXACTLY three
// kind branches (action-call, transform, llm-seam). The action-call and
// transform branches hold no reference to the seam type, so no deterministic
// step can reach an LLM. Only the llm-seam branch calls runSeam, and that
// errors by default in v1.
type executor struct {
	plan      *Plan
	enforcer  *enforcer
	transform *TransformRegistry
	seam      LLMSeam
}

// execute runs Phase B: it walks p.Order, executes each step against the
// resolved inputs and accumulated step outputs, materializes declared
// artifacts, and returns the final state. A step error aborts the walk (no
// later step runs) and is returned to the caller.
func (x *executor) execute(ctx context.Context, inputs ResolvedInputs) (execState, error) {
	st := execState{
		inputs:     inputs,
		stepOutput: map[string]stepResult{},
	}
	for _, idx := range x.plan.Order {
		step := x.plan.Steps[idx]
		resolved, err := x.resolveBindings(step, st)
		if err != nil {
			return st, err
		}

		var outputs map[string]any
		switch step.Kind {
		case KindActionCall:
			outputs, err = x.runActionCall(ctx, step, resolved, &st)
		case KindTransform:
			outputs, err = x.transform.run(step, resolved)
		case KindLLMSeam:
			outputs, err = runSeam(ctx, x.seam, step, resolved)
		default:
			// Unreachable: decode validated the closed kind enum.
			err = fmt.Errorf("flightplan: step %q has unhandled kind %q", step.ID, step.Kind)
		}
		if err != nil {
			return st, err
		}

		// Every declared output must be produced so downstream bindings
		// resolve. (runSeam already checks this for the seam; action-call and
		// transform are checked here.)
		for _, name := range step.Outputs {
			if _, ok := outputs[name]; !ok {
				return st, fmt.Errorf("flightplan: step %q did not produce declared output %q", step.ID, name)
			}
		}
		st.stepOutput[step.ID] = stepResult(outputs)

		// Materialize a declared output artifact when the step wires one.
		if step.MaterializesOutput != "" {
			art, err := materialize(x.plan, step, outputs)
			if err != nil {
				return st, err
			}
			st.artifacts = append(st.artifacts, art)
		}
	}
	return st, nil
}

// runActionCall dispatches an action-call through the enforced boundary and
// records the dispatch for the audit. The redacted result becomes the step's
// outputs: each declared output name reads from the redacted result by name.
func (x *executor) runActionCall(ctx context.Context, step Step, args map[string]any, st *execState) (map[string]any, error) {
	action := x.plan.Actions[step.ActionRef]
	outcome, err := x.enforcer.dispatch(ctx, action, args, 1)
	// Record the dispatch even on a deny so the audit reflects the denial.
	rec := actionDispatch{
		StepID:            step.ID,
		ActionRef:         step.ActionRef,
		Effect:            action.TrustContract.Effect,
		ApprovalRequested: outcome.ApprovalRequested,
		Approved:          outcome.Approved,
		Result:            outcome.Result,
		AuditFields:       action.TrustContract.Audit.Fields,
		Sink:              action.TrustContract.Audit.Sink,
	}
	st.dispatches = append(st.dispatches, rec)
	if err != nil {
		return nil, err
	}

	// Map the redacted result to the step's declared outputs. A single
	// declared output reads the whole redacted result; otherwise each output
	// reads the same-named field from the result.
	outputs := make(map[string]any, len(step.Outputs))
	if len(step.Outputs) == 1 {
		outputs[step.Outputs[0]] = outcome.Result
		return outputs, nil
	}
	for _, name := range step.Outputs {
		outputs[name] = outcome.Result[name]
	}
	return outputs, nil
}

// resolveBindings resolves a step's binding map into concrete values against
// the resolved inputs and accumulated prior step outputs. A binding can only
// reference an already-resolved input or an already-executed step (guaranteed
// by the topological order), so a missing value here is an internal invariant
// violation, not user error.
func (x *executor) resolveBindings(step Step, st execState) (map[string]any, error) {
	binds := step.binds()
	if len(binds) == 0 {
		return map[string]any{}, nil
	}
	out := make(map[string]any, len(binds))
	for name, b := range binds {
		switch b.Kind {
		case BindInput:
			v, ok := st.inputs.Values[b.Name]
			if !ok {
				return nil, fmt.Errorf("flightplan: step %q binding %q: input %q was not resolved", step.ID, name, b.Name)
			}
			out[name] = v
		case BindStep:
			sr, ok := st.stepOutput[b.StepID]
			if !ok {
				return nil, fmt.Errorf("flightplan: step %q binding %q: step %q output not available (walk order invariant)", step.ID, name, b.StepID)
			}
			v, ok := sr[b.Output]
			if !ok {
				return nil, fmt.Errorf("flightplan: step %q binding %q: step %q has no output %q", step.ID, name, b.StepID, b.Output)
			}
			out[name] = v
		}
	}
	return out, nil
}
