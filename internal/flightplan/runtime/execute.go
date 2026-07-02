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
	// reaches records the declared per-step network reach captured at each
	// rung-3 dispatch, in execution order. It is audit-only: the runtime never
	// enforces the declared reach (egress stays passthrough), it only surfaces
	// the declaration as a `flightplan.launch.reach` record marked
	// `enforced:false` (#1784).
	reaches []reachRecord
	// outputs records each materialized artifact together with the step that
	// produced it, in execution order. Unlike artifacts (kept for writing and
	// the run result), outputs carries the originating step's id/kind/transform
	// so the audit can emit one `output.materialized` record per artifact with
	// full provenance. It is populated for BOTH action-call and transform
	// materializing steps, since the materialize block keys off
	// step.MaterializesOutput (kind-agnostic).
	outputs []materializedOutput
}

// materializedOutput pairs one materialized artifact with the provenance of the
// step that produced it, so the per-output audit record can name the
// originating step and (for a transform) the transform applied.
type materializedOutput struct {
	// StepID is the id of the step that materialized the artifact.
	StepID string
	// StepKind is the step's kind (action-call or transform).
	StepKind StepKind
	// Transform is the transform name, set only for a KindTransform step.
	Transform string
	// Artifact is the materialized artifact (name, mime, content, digest).
	Artifact Artifact
	// Binds is the producing step's binding reference map (input name →
	// Binding), captured at materialize time. It is the reference half of the
	// input walk-back the per-output audit record emits (issue #1753): each
	// Binding names where the value came from (an input or a prior step
	// output) without inlining the value.
	Binds map[string]Binding
	// Resolved is the producing step's resolved binding values (input name →
	// value), captured at materialize time. Paired with Binds it lets the
	// audit record hash each bound value (content_hash) and lift a
	// QueryExecutionId when present, so a materialized output walks back to
	// the exact inputs that produced it — by hash, never by inlining the
	// dataset.
	Resolved map[string]any
	// ToolImage is the pinned `ref@sha256:<hex>` tool image, set ONLY when the
	// producing step dispatched a rung-3 tool image (step.ToolDispatch != nil).
	// It is empty for every non-tool (in-process action-call or transform)
	// step. Its presence is the discriminator the audit layer uses to flip
	// `aileron.step.kind` to the literal "tool" and to record
	// `aileron.step.image` with this pin. The content-addressed pin fully and
	// verifiably identifies what ran (the image's baked-in entrypoint), so it
	// carries the executed-command identity a rung-3 dispatch has (there is no
	// separate command in the rung-3 model, issue #1762).
	ToolImage string
}

// reachRecord captures one rung-3 dispatch's declared network reach for the
// audit: the step id and the trust contract's Effect and Hosts. It is
// audit-only, recorded regardless of dispatch outcome, and NEVER an enforcement
// seam — no host is bound, blocked, or correlated from it (#1784, descope #1783).
type reachRecord struct {
	StepID string
	Effect Effect
	Hosts  []string
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

	// The following carry the non-secret actor provenance the dispatcher
	// surfaced for this call (issue #1753): the connector build and the
	// identity/binding it used, plus the consent posture. They let a
	// materialized output's audit record attribute the produced artifact to
	// the connector version+hash and identity that produced it, whether the
	// materializing step is this action-call itself or a downstream transform
	// that binds this step's output. Zero when the daemon did not populate
	// them (a credential-less action, an unresolved binding, a deny).
	ConnectorVersion  string
	ConnectorHash     string
	IdentityLabel     string
	CredentialBinding string
	ConsentDecision   string
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
	// toolRunner dispatches a rung-3 per-step tool image (mount → run →
	// collect). Nil when no tool runner is configured; a step that carries a
	// ToolDispatch with a nil toolRunner is an explicit error, never a silent
	// in-process fallback (mirrors the seam/image-runner nil-guard discipline).
	toolRunner ToolImageRunner
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
		switch {
		case step.ToolDispatch != nil:
			// A rung-3 step shells out to its pinned sibling tool image with
			// mount → run → collect I/O, orthogonal to Kind. It is checked first
			// so the in-process kind branches never run for a tool-dispatch step.
			outputs, err = x.runToolDispatch(ctx, step, resolved, &st)
		case step.Kind == KindActionCall:
			outputs, err = x.runActionCall(ctx, step, resolved, &st)
		case step.Kind == KindTransform:
			outputs, err = x.transform.run(step, resolved)
		case step.Kind == KindLLMSeam:
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
			// Capture the artifact with its originating step's provenance. This
			// block fires for both action-call and transform materializing
			// steps (it keys off step.MaterializesOutput, not step.Kind), so a
			// transform-materialized output is captured with no new branch.
			st.outputs = append(st.outputs, materializedOutput{
				StepID:    step.ID,
				StepKind:  step.Kind,
				Transform: step.Transform,
				Artifact:  art,
				// Capture the producing step's binding references and their
				// resolved values so the per-output audit record can walk back
				// to the exact inputs (issue #1753). `resolved` is the same
				// name→value map the step just executed against, so the audited
				// inputs are precisely what produced this artifact.
				Binds:    step.binds(),
				Resolved: resolved,
				// Carry the pinned tool image ONLY when this step dispatched a
				// rung-3 tool (issue #1762). Empty for every non-tool step; its
				// presence flips the audit record's step.kind to "tool" and adds
				// aileron.step.image. The rung-3 model carries no separate
				// command, so the content-addressed pin is the executed-command
				// identity.
				ToolImage: toolImage(step),
			})
		}
	}
	return st, nil
}

// toolImage returns the step's pinned `ref@sha256:<hex>` tool image when the
// step dispatched a rung-3 tool, or "" for any in-process (action-call or
// transform) step. It is the tool-materialized discriminator the audit layer
// reads to distinguish a tool-produced output from a connector/transform one.
func toolImage(step Step) string {
	if step.ToolDispatch == nil {
		return ""
	}
	return step.ToolDispatch.Image
}

// runActionCall dispatches an action-call through the enforced boundary and
// records the dispatch for the audit. The redacted result becomes the step's
// outputs: each declared output name reads from the redacted result by name.
func (x *executor) runActionCall(ctx context.Context, step Step, args map[string]any, st *execState) (map[string]any, error) {
	action := x.plan.Actions[step.ActionRef]
	outcome, err := x.enforcer.dispatch(ctx, "step:"+step.ID, action, args, 1)
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
		// Carry the actor provenance the dispatcher surfaced (issue #1753) so
		// a materialized output produced by this step — or by a downstream
		// transform binding it — can attribute the connector build and
		// identity that produced it.
		ConnectorVersion:  outcome.ConnectorVersion,
		ConnectorHash:     outcome.ConnectorHash,
		IdentityLabel:     outcome.IdentityLabel,
		CredentialBinding: outcome.CredentialBinding,
		ConsentDecision:   outcome.ConsentDecision,
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
	// A multi-output action-call reads each declared output from the redacted
	// result by name. A missing key is a hard error so a nil never slips
	// through to bypass the downstream presence check.
	for _, name := range step.Outputs {
		v, ok := outcome.Result[name]
		if !ok {
			return nil, fmt.Errorf("flightplan: action %q (step %q) result has no declared output %q", step.ActionRef, step.ID, name)
		}
		outputs[name] = v
	}
	return outputs, nil
}

// runToolDispatch dispatches a rung-3 step to its pinned sibling tool image
// with mount → run → collect I/O (ADR-0027 rung three, #1733). It mounts the
// step's resolved binding input at the declared mount path, runs the pinned
// tool, and reads back the collected output. The collected value becomes the
// step's single declared output so downstream steps binding steps.<id>.<output>
// receive it through the existing resolveBindings path with no new dataflow
// mechanism.
//
// A tool-dispatch step with no configured tool runner is an explicit error,
// never a silent skip: a declared, pinned dispatch must be entered to honor the
// attestation (mirrors the ImageRunner/LLMSeam nil-guard discipline).
func (x *executor) runToolDispatch(ctx context.Context, step Step, resolved map[string]any, st *execState) (map[string]any, error) {
	td := step.ToolDispatch
	// Record the step's declared network reach before the dispatch, so the
	// declaration is captured regardless of the dispatch outcome (a nil runner,
	// a runner error). This is audit-only: the reach is never enforced. A step
	// that declared no trust contract records nothing (never a synthesized empty
	// contract).
	if td.TrustContract != nil {
		st.reaches = append(st.reaches, reachRecord{
			StepID: step.ID,
			Effect: td.TrustContract.Effect,
			Hosts:  td.TrustContract.Hosts,
		})
	}
	if x.toolRunner == nil {
		return nil, fmt.Errorf("flightplan: step %q dispatches pinned tool image %q but no tool image runner is configured", step.ID, td.Image)
	}
	// The mount input is the step's resolved bindings. It is a binding-resolved
	// value only, never a credential.
	spec := ToolRunSpec{
		Image:       td.Image,
		StepID:      step.ID,
		MountPath:   td.MountPath,
		Input:       resolved,
		CollectPath: td.CollectPath,
	}
	res, err := x.toolRunner.Run(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("flightplan: step %q tool dispatch to %q: %w", step.ID, td.Image, err)
	}

	// The collected output maps to the step's single declared output. A rung-3
	// dispatch produces one collected value; a step declaring multiple outputs
	// has no unambiguous mapping for a single collected blob, so that is refused.
	// A rung-3 dispatch collects exactly one blob, so the step must declare
	// exactly one output to carry it. Zero outputs (a collect with nowhere to
	// land) and multiple outputs (an ambiguous mapping for one blob) are both
	// refused rather than silently dropping or duplicating the collected value.
	if len(step.Outputs) != 1 {
		return nil, fmt.Errorf("flightplan: step %q dispatches a tool image and declares %d outputs; a rung-3 tool dispatch produces exactly one collected output", step.ID, len(step.Outputs))
	}
	return map[string]any{step.Outputs[0]: res.Output}, nil
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
