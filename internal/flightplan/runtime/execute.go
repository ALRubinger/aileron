package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
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
	// reaches records the per-step network reach captured at each tool-step
	// execution, in execution order (#1784, enforced by #1829). Each record
	// carries the hosts the step ran under and whether they were the sealed,
	// step-scope-enforced reach from the verified lock.
	reaches []reachRecord
	// outputs records each materialized artifact together with the step that
	// produced it, in execution order. Unlike artifacts (kept for writing and
	// the run result), outputs carries the originating step's id/kind/transform
	// so the audit can emit one `output.materialized` record per artifact with
	// full provenance. It is populated for BOTH action-call and transform
	// materializing steps, since the materialize block keys off
	// step.MaterializesOutput (kind-agnostic).
	outputs []materializedOutput
	// toolCommands records, per tool step id, the INSTANTIATED argv the runner
	// exec'd and the argv indices that were derived from `{{ inputs.<name> }}`
	// interpolation (#1958). runToolStep populates it before the exec; the
	// materialize block reads it so a tool-materialized output audits the
	// resolved command (not the template) plus the input-derived marker. A
	// token-free tool step records the literal argv with no derived indices, so
	// existing behavior is unchanged.
	toolCommands map[string]toolCommandResult
}

// toolCommandResult is one tool step's instantiated argv and the argv indices
// that were derived from input interpolation (#1958).
type toolCommandResult struct {
	// resolved is the argv the runner exec'd, with every `{{ inputs.<name> }}`
	// token substituted.
	resolved []string
	// derivedIdx are the argv indices whose element carried a token, sorted by
	// appearance. Empty for a token-free command.
	derivedIdx []int
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
	// Command is the executed argv, set ONLY when the producing step is a
	// `kind: tool` step (#1829). It is nil for every non-tool step. The argv
	// is the executed-command identity the per-output audit record emits as
	// `aileron.step.command`; the environment identity is the plan's single
	// composed pin, already on the launch record, so no per-step image is
	// recorded here. For a tool step with command interpolation (#1958) this is
	// the INSTANTIATED argv (tokens resolved), not the sealed template.
	Command []string
	// CommandDerived are the argv indices instantiated from `{{ inputs.<name> }}`
	// interpolation (#1958), sorted by appearance. Set only for a tool step
	// whose command carried at least one token; nil for a token-free tool step
	// and every non-tool step. The per-output audit record emits it as
	// `aileron.step.command_derived`.
	CommandDerived []int
}

// reachRecord captures one tool step's network reach for the audit: the step
// id, the declared operation Effect, the Hosts the step ran under, and
// whether that reach was enforced. Enforced means the hosts are the verified
// lock's sealed stepTrust reach and the step's subprocess ran under a
// step-scoped proxy credential restricted to exactly them (#1829); a
// contracted step with no sealed entry (only reachable on directly
// constructed plans — the verified load path refuses that shape) records its
// frontmatter hosts with enforced:false, so the record never overclaims.
type reachRecord struct {
	StepID   string
	Effect   Effect
	Hosts    []string
	Enforced bool
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

// executor walks the topologically-ordered step graph. It has EXACTLY four
// kind branches (action-call, transform, tool, llm-seam). The action-call,
// transform, and tool branches hold no reference to the seam type, so no
// deterministic step can reach an LLM. Only the llm-seam branch calls
// runSeam, and that errors by default in v1.
type executor struct {
	plan      *Plan
	enforcer  *enforcer
	transform *TransformRegistry
	seam      LLMSeam
	// toolRunner executes a `kind: tool` step as a subprocess in the current
	// pinned environment (#1829). Nil when no tool runner is configured; a
	// tool step with a nil toolRunner is an explicit error, never a silent
	// skip (mirrors the seam/image-runner nil-guard discipline).
	toolRunner ToolStepRunner
	// stepTrust is the verified lock's sealed per-step reach
	// (freeze.VerifiedFrozen.StepTrust), keyed by tool step id. It is the
	// ONLY source of the reach the runtime enforces: the frontmatter
	// trust-contract copy on the Step is audit context, never the
	// enforcement input. Nil/absent entries mean the step declares no reach.
	stepTrust map[string]freeze.StepReach

	// suspendable opts this walk into the generic suspend/resume path (#2100).
	// When true, an unfulfilled llm-seam (nil seam and no memo entry) and a
	// pending approval both SUSPEND the walk (return a suspendSignal) instead of
	// erroring/blocking. When false, behavior is exactly as before: a nil seam is
	// a hard error and a pending approval is impossible.
	suspendable bool
	// resumeOutputs is the caller-supplied memo (stepId → its outputs) replayed
	// on resume (#2100). A step whose id is present is INJECTED without
	// re-execution (exactly-once for effects) and re-audits nothing. Nil on a
	// fresh launch.
	resumeOutputs map[string]stepResult
}

// execute runs Phase B: it walks p.Order, executes each step against the
// resolved inputs and accumulated step outputs, materializes declared
// artifacts, and returns the final state. A step error aborts the walk (no
// later step runs) and is returned to the caller.
//
// On the suspendable path (#2100), execute also injects already-memoized step
// outputs (x.resumeOutputs) WITHOUT re-executing or re-auditing them, and
// SUSPENDS at the first step it cannot complete in-band — an unfulfilled seam
// or a pending approval — by returning a non-nil *suspendSignal (with a nil
// error). A nil signal and nil error means the walk completed.
func (x *executor) execute(ctx context.Context, inputs ResolvedInputs) (execState, *suspendSignal, error) {
	st := execState{
		inputs:       inputs,
		stepOutput:   map[string]stepResult{},
		toolCommands: map[string]toolCommandResult{},
	}
	for _, idx := range x.plan.Order {
		step := x.plan.Steps[idx]

		// Memoized-replay (#2100): a step whose id is in the resume memo is
		// INJECTED, not executed. Its output flows to downstream bindings, its
		// materializing artifact is rebuilt (materialize is pure — it reads the
		// memoized outputs), but no effect fires and no audit record is emitted
		// (the call that actually ran the step already audited it). This is the
		// soundness fix from residual #2107: because topoSort orders by data
		// edges only, an effectful step can sit before a gated action, so seam-
		// only memoization would re-run it on resume. Memoizing ALL outputs makes
		// every step run exactly once across the whole suspend/resume sequence.
		if memo, ok := x.resumeOutputs[step.ID]; ok {
			if err := x.injectMemoized(step, memo, &st); err != nil {
				return st, nil, err
			}
			continue
		}

		resolved, err := x.resolveBindings(step, st)
		if err != nil {
			return st, nil, err
		}

		// Suspend BEFORE executing when this step cannot complete in-band.
		if sig, suspend := x.suspendFor(ctx, step, resolved); suspend {
			return st, sig, nil
		}

		var outputs map[string]any
		switch {
		case step.Kind == KindTool:
			// A tool step runs its argv as a deterministic subprocess inside
			// the plan's pinned environment with mount → run → collect I/O
			// (#1829), scoped to its sealed reach.
			outputs, err = x.runToolStep(ctx, step, resolved, &st)
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
			// On the suspendable path a pending approval surfaces as a sentinel
			// error from runActionCall's dispatch; convert it to a suspend rather
			// than a run failure. The effect never fired (dispatch short-circuits
			// before Dispatch).
			var pending *PendingApprovalError
			if x.suspendable && errors.As(err, &pending) {
				return st, &suspendSignal{
					kind:     SuspendKindApproval,
					stepID:   step.ID,
					approval: &ApprovalRequest{ActionRef: pending.ActionRef, Effect: pending.Effect, Args: pending.Args},
				}, nil
			}
			return st, nil, err
		}

		// Every declared output must be produced so downstream bindings
		// resolve. (runSeam already checks this for the seam; action-call and
		// transform are checked here.)
		for _, name := range step.Outputs {
			if _, ok := outputs[name]; !ok {
				return st, nil, fmt.Errorf("flightplan: step %q did not produce declared output %q", step.ID, name)
			}
		}
		st.stepOutput[step.ID] = stepResult(outputs)

		// Materialize a declared output artifact when the step wires one.
		if step.MaterializesOutput != "" {
			art, err := materialize(x.plan, step, outputs)
			if err != nil {
				return st, nil, err
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
				// Carry the executed argv ONLY when this step is a tool step
				// (#1829): the command is the executed identity the per-output
				// audit record emits as aileron.step.command. For an
				// interpolated command (#1958) this is the INSTANTIATED argv
				// runToolStep stashed, plus the input-derived index marker. Nil
				// for every non-tool step. The environment identity is the
				// plan's single composed pin, already on the launch record.
				Command:        toolCommandArgv(step, &st),
				CommandDerived: toolCommandDerived(step, &st),
			})
		}
	}
	return st, nil, nil
}

// injectMemoized replays a memoized step's output on resume (#2100): it seeds
// st.stepOutput from the caller-supplied memo, runs the declared-output presence
// check against the memoized value, and rebuilds a materializing step's artifact
// (materialize is pure). It executes NOTHING (no effect fires) and audits
// NOTHING: it does not append to st.dispatches, st.reaches, or st.outputs, so
// emitAudit emits no record for an injected step and the exactly-once /
// no-double-audit contract holds. The artifact IS appended to st.artifacts so
// the final RunResult.Artifacts is whole after the completing resume; the audit
// provenance list (st.outputs) is deliberately NOT appended, because the
// output.materialized record was already emitted on the call that executed the
// step.
func (x *executor) injectMemoized(step Step, memo stepResult, st *execState) error {
	for _, name := range step.Outputs {
		if _, ok := memo[name]; !ok {
			return fmt.Errorf("flightplan: resumed step %q memo is missing declared output %q", step.ID, name)
		}
	}
	st.stepOutput[step.ID] = memo
	if step.MaterializesOutput != "" {
		art, err := materialize(x.plan, step, map[string]any(memo))
		if err != nil {
			return err
		}
		st.artifacts = append(st.artifacts, art)
	}
	return nil
}

// suspendFor decides whether the walk must SUSPEND at this step before executing
// it (#2100), on the suspendable path only. It suspends at an llm-seam step that
// has no wired seam value able to produce its output (a memo miss reached this
// point, so the seam is unfulfilled). The pending-approval suspend is detected
// AFTER dispatch (the approver is what returns pending), so it is handled at the
// call site via the PendingApprovalError sentinel, not here. On the
// non-suspendable path this returns (nil, false) so today's behavior — a nil
// seam is a hard error inside runSeam — is preserved exactly.
func (x *executor) suspendFor(_ context.Context, step Step, resolved map[string]any) (*suspendSignal, bool) {
	if !x.suspendable {
		return nil, false
	}
	if step.Kind == KindLLMSeam && x.seam == nil {
		return &suspendSignal{
			kind:   SuspendKindSeam,
			stepID: step.ID,
			seam:   &SeamRequest{StepID: step.ID, Bindings: resolved, Outputs: step.Outputs},
		}, true
	}
	return nil, false
}

// toolCommandArgv returns the INSTANTIATED argv a tool step exec'd (the
// command runToolStep stashed after interpolation), or nil for any other kind.
// It is the tool-materialized discriminator the audit layer reads to
// distinguish a tool-produced output from a connector/transform one. A tool
// step with no stashed command (a directly-constructed plan that bypassed
// runToolStep) falls back to the template argv so the record is never empty.
func toolCommandArgv(step Step, st *execState) []string {
	if step.Kind != KindTool {
		return nil
	}
	if tc, ok := st.toolCommands[step.ID]; ok {
		return tc.resolved
	}
	return step.Command
}

// toolCommandDerived returns the argv indices a tool step instantiated from
// `{{ inputs.<name> }}` interpolation (#1958), or nil for a token-free tool
// step and every non-tool step.
func toolCommandDerived(step Step, st *execState) []int {
	if step.Kind != KindTool {
		return nil
	}
	if tc, ok := st.toolCommands[step.ID]; ok {
		return tc.derivedIdx
	}
	return nil
}

// runActionCall dispatches an action-call through the enforced boundary and
// records the dispatch for the audit. The redacted result becomes the step's
// outputs: each declared output name reads from the redacted result by name.
func (x *executor) runActionCall(ctx context.Context, step Step, args map[string]any, st *execState) (map[string]any, error) {
	action := x.plan.Actions[step.ActionRef]
	outcome, err := x.enforcer.dispatch(ctx, "step:"+step.ID, action, args, 1)
	// A pending approval (#2100) is NOT a decision and NOT an executed dispatch:
	// the run will SUSPEND here and resume later, at which point this step
	// dispatches for real and records exactly one dispatch. Returning before the
	// dispatch record is appended keeps the audit exactly-once across the whole
	// suspend/resume sequence (no pending row now, no double row on resume) and
	// emits no deny record (a pending is neither approved nor denied).
	var pending *PendingApprovalError
	if errors.As(err, &pending) {
		return nil, err
	}
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

// runToolStep executes a `kind: tool` step as a deterministic subprocess in
// the plan's pinned environment with mount → run → collect I/O (#1829). It
// threads the step's SEALED reach (the verified lock's stepTrust entry,
// never the frontmatter copy) into the runner, which runs the subprocess
// under a step-scoped proxy credential restricted to exactly those hosts —
// failing closed when the scope cannot be obtained. The collected value
// becomes the step's single declared output so downstream steps binding
// steps.<id>.<output> receive it through the existing resolveBindings path
// with no new dataflow mechanism.
//
// A tool step with no configured tool runner is an explicit error, never a
// silent skip (mirrors the ImageRunner/LLMSeam nil-guard discipline).
func (x *executor) runToolStep(ctx context.Context, step Step, resolved map[string]any, st *execState) (map[string]any, error) {
	// The reach the step runs under: the sealed lock entry when present.
	// enforced:true is recorded only for a sealed reach, because only a
	// sealed reach drives the runner's step-scope credential (the production
	// runner fails closed rather than run a sealed step unscoped).
	sealed, hasSealed := x.stepTrust[step.ID]

	// Instantiate the sealed reach TEMPLATE into the concrete hosts the
	// step-scope mint enforces (#1959), BEFORE building the reach record or the
	// ToolStepSpec: each `{{ inputs.<name> }}` host token becomes the
	// constrained input's string form (resolved from the plan inputs,
	// st.inputs.Values), and the result is re-checked to the host[:port] shape
	// so a bad instantiation fails the step closed rather than reaching the
	// mint. Freeze sealed the TEMPLATE (covered by the signature); the runtime
	// resolves it here, so the proxy only ever sees the concrete host and the
	// exact-match stays exact-match. A token-free host instantiates to itself,
	// so existing non-templated plans are unchanged; a step with no sealed
	// reach yields nil (no hosts minted), exactly as before.
	instSealedHosts, err := instantiateHosts(sealed.Hosts, st.inputs.Values)
	if err != nil {
		return nil, fmt.Errorf("flightplan: step %q trust-contract host interpolation: %w", step.ID, err)
	}

	// Record the step's network reach before the execution, so the record is
	// captured regardless of the run outcome (a nil runner, a subprocess
	// error). A step that declared no trust contract records nothing (never a
	// synthesized empty contract). The recorded hosts are the INSTANTIATED
	// hosts, so the audit reach record shows the concrete host the step ran
	// under, not the sealed template.
	if step.TrustContract != nil {
		hosts := instSealedHosts
		if !hasSealed {
			// Unenforced fallback (frontmatter reach, enforced:false — only
			// reachable outside the verified load path): instantiate the
			// frontmatter template so the audit still shows the concrete host.
			hosts, err = instantiateHosts(step.TrustContract.Hosts, st.inputs.Values)
			if err != nil {
				return nil, fmt.Errorf("flightplan: step %q trust-contract host interpolation: %w", step.ID, err)
			}
		}
		st.reaches = append(st.reaches, reachRecord{
			StepID:   step.ID,
			Effect:   step.TrustContract.Effect,
			Hosts:    hosts,
			Enforced: hasSealed,
		})
	}
	if x.toolRunner == nil {
		return nil, fmt.Errorf("flightplan: step %q is a tool step but no tool step runner is configured", step.ID)
	}
	// The collected output maps to the step's single declared output. A tool
	// step collects exactly one blob, so the step must declare exactly one
	// output to carry it. Zero outputs (a collect with nowhere to land) and
	// multiple outputs (an ambiguous mapping for one blob) are both refused
	// rather than silently dropping or duplicating the collected value.
	// Checked BEFORE invoking the runner (decode also refuses the shape;
	// this is the direct-construct backstop) so a misconfigured step never
	// triggers an executed — and possibly non-idempotent — subprocess whose
	// result would only be discarded.
	if len(step.Outputs) != 1 {
		return nil, fmt.Errorf("flightplan: step %q is a tool step and declares %d outputs; a tool step produces exactly one collected output", step.ID, len(step.Outputs))
	}
	// Instantiate the TEMPLATE argv against the resolved PLAN inputs (#1958):
	// each `{{ inputs.<name> }}` token becomes the constrained input's string
	// form. Command tokens reference plan inputs, not the step's resolved
	// bindings, so the lookup is st.inputs.Values. The result is still an
	// argv array exec'd with no shell. Stash it (with the input-derived index
	// marker) so the materialize block audits the resolved command. A
	// token-free command instantiates to itself with no derived indices, so
	// existing behavior is unchanged.
	resolvedCmd, derivedIdx, err := instantiateCommand(step.Command, st.inputs.Values)
	if err != nil {
		return nil, fmt.Errorf("flightplan: step %q command interpolation: %w", step.ID, err)
	}
	if st.toolCommands == nil {
		st.toolCommands = map[string]toolCommandResult{}
	}
	st.toolCommands[step.ID] = toolCommandResult{resolved: resolvedCmd, derivedIdx: derivedIdx}

	// The mounted input is the step's resolved bindings. It is a
	// binding-resolved value only, never a credential.
	spec := ToolStepSpec{
		StepID:      step.ID,
		Command:     resolvedCmd,
		MountPath:   step.MountPath,
		Input:       resolved,
		CollectPath: step.CollectPath,
		Hosts:       instSealedHosts,
	}
	// Thread the step's declared credential identity to the runner's mint
	// (#1980): the non-secret kind + identity label from the trust contract,
	// so the daemon learns which credential identity the step's egress
	// belongs to. A step with no trust contract, or one that declares no
	// credential identity, leaves both empty — the mint then sends no
	// credential block and the scope stays unconstrained, exactly as before.
	// This does not alter host instantiation, the reach record, or the
	// sealed-vs-frontmatter logic above.
	if step.TrustContract != nil {
		spec.CredentialKind = step.TrustContract.CredentialKind
		spec.IdentityLabel = step.TrustContract.IdentityLabel
	}
	res, err := x.toolRunner.Run(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("flightplan: step %q tool execution: %w", step.ID, err)
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
