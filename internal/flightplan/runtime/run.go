package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/google/uuid"
)

// Options configures a launch. The Store + Name + Version select the frozen
// unit; the SPI seams wire the runtime to the daemon-backed boundary; the
// Clock and TransformRegistry default to deterministic, LLM-free
// implementations.
type Options struct {
	// Store is the canonical skill store the frozen unit loads from.
	Store *store.Store
	// Name is the frozen skill name.
	Name string
	// Version is the frozen version id (the store directory id).
	Version string

	// Inputs are the literal input overrides supplied at launch.
	Inputs LaunchArgs

	// Dispatcher is the action boundary seam (required to run any action-call
	// or source input).
	Dispatcher ActionDispatcher
	// Approver routes effect-gated actions (required when any non-read action
	// runs).
	Approver Approver
	// Audit receives the customer-owned audit records. Nil emits no audit.
	Audit AuditSink
	// Seam is a marked LLM seam. Nil (the v1 default) makes any llm-seam step
	// error on the non-suspendable path, so a default launch reaches no LLM. A
	// plan may declare more than one seam (#2100); each is served by this one
	// provider.
	Seam LLMSeam

	// Suspendable opts this run into the generic suspend/resume path (#2100).
	// When true, the runtime SUSPENDS (returns a nil error with
	// RunResult.Pending set) at the first step it cannot complete in-band: an
	// unfulfilled llm-seam (no memo entry, no wired Seam value) or a gated
	// action-call whose Approver returned Decision.Pending. When false (the
	// default), behavior is exactly as before: a nil Seam is a hard error and a
	// pending Approver decision is impossible (a synchronous approver never
	// returns pending). A non-nil ResumeOutputs also implies a suspendable run,
	// so a resume never needs both flags set.
	Suspendable bool
	// ResumeOutputs is the caller-supplied memo (stepId → its named outputs) from
	// a prior suspend, replayed on resume. Every step whose id is present here is
	// injected WITHOUT re-execution (exactly-once for effects) and re-audits
	// nothing. Nil on a fresh launch. A non-nil value implies Suspendable.
	ResumeOutputs map[string]map[string]any
	// RunID is stable across a suspend/resume sequence. The runtime mints one
	// when empty and echoes it on the suspend result, so every call in a sequence
	// shares the same id.
	RunID string
	// ImageRunner boots the verified pinned environment image and runs the
	// plan inside it. When the loaded plan carries a resolved image pin and this
	// seam is wired, Run delegates to it; when the plan pins no image, Run stays
	// on the in-process path and never touches this seam. A plan that pins an
	// image with no ImageRunner configured is an explicit error, never a silent
	// in-process fallback (a declared environment must be entered to honor the
	// attestation).
	ImageRunner ImageRunner
	// ImageDigestResolver re-checks, at boot time, that a composed-tools pin's
	// LocalTag still resolves in the local daemon to the host platform's attested
	// config content digest (#1863). It is consulted ONLY on the composed boot
	// path (pin.LocalTag != ""): a composed image is booted by its mutable local
	// tag (its recorded content digest is a locally-built image Id, not a registry
	// digest, so `ref@digest` would not resolve), and nothing else re-checks that
	// the daemon image behind that tag is still the attested one. When this seam is
	// wired and the pin is composed, the resolved digest MUST equal the host
	// platform's entry from the pin's per-arch configDigests set or the boot fails
	// closed (no ImageRunner.Run call); a plan not built for this host's platform is
	// likewise fail-closed, as is a resolve error (the
	// attested image is absent). Nil (the zero value) skips the guard and boots as
	// before (backward-compatible), mirroring the ImageRunner nil-guard discipline.
	ImageDigestResolver LocalImageDigestResolver
	// RegistryImageResolver pulls and verifies the published image for a plan
	// installed by OCI reference (#1903). It is consulted ONLY when the loaded
	// plan carries a registry origin (LoadedPlan.ImageOrigin.Present): such a
	// plan has no local build, so runInImage pulls the published image from the
	// recorded registry, verifies it against the signed lock pin per the pin's
	// binding kind, and boots the returned reference. When the plan needs this
	// seam (a registry origin) but it is nil, the boot is a fail-closed error,
	// never a silent fall-through to the local-tag path (a plan installed from a
	// registry has no local tag to boot). A locally-frozen plan (no origin)
	// never touches this seam and boots by its local tag as before. The CLI
	// wires the production impl over pull.PullImage and nils it on the image-boot
	// re-entry (the sentinel routes in-process before any boot).
	RegistryImageResolver RegistryImageResolver
	// ToolRunner executes a `kind: tool` step as a deterministic subprocess in
	// the current pinned environment (#1829). Unlike ImageRunner, the plan
	// orchestration stays in-process (runPlan); the tool step never dispatches
	// a sibling container. When a loaded plan carries a tool step and this
	// seam is unset, that step is an explicit error, never a silent skip
	// (mirrors the ImageRunner nil-guard discipline).
	ToolRunner ToolStepRunner
	// PublisherVerifier is the host-side publisher-trust gate (#1900). When it
	// is wired and the loaded plan declares a publisher in its verified lock,
	// Run resolves the plan's verified signing key against the operator's
	// keyring for that publisher and refuses to run when the publisher is not
	// trusted (fail-closed), before any boot or step. Nil skips the gate; a
	// plan that declares no publisher also skips it. The CLI wires the
	// keyring-backed impl for a host launch and wires nil on the image-boot
	// re-entry (the host already gated before boot, and no keyring is mounted
	// into the sealed container).
	PublisherVerifier PublisherVerifier
	// InPinnedImage marks this run as already executing INSIDE the verified
	// pinned environment image, the image-boot re-entry (#1731). It routes
	// a whole-plan-pinned unit onto the in-process path instead of booting the
	// pin again: inside the container, in-process IS the certified
	// environment, and re-booting would recurse (the image carries no nested
	// container runtime, and with one it would never terminate). The CLI sets
	// this from the AILERON_SKILL_IMAGE_BOOTED sentinel the image runner
	// injects into the boot env.
	InPinnedImage bool

	// Clock supplies the single launch-time read for dynamic inputs. Nil uses
	// SystemClock.
	Clock Clock
	// Transforms is the deterministic transform registry. Nil uses the
	// default registry.
	Transforms *TransformRegistry

	// InputPrompter resolves a missing required literal input interactively at
	// the launch boundary (a literal input with no `--input` override and no
	// declared default). When it is wired, resolveInputs asks it for the value
	// instead of failing; when it is nil (the default), the runtime keeps
	// today's fail-fast error, so a piped or CI launch never blocks on input.
	// The CLI wires it only when stdin is a TTY. A prompted value is a string
	// treated identically to a `--input name=value` override: it is deep-copied
	// into the frozen resolved set and validated by the same final constraint
	// pass.
	//
	// On the interactive path the InputWalker (below) resolves every literal into
	// Inputs before either branch runs, so a still-wired InputPrompter is never
	// consulted there; the seam is retained for the non-walk path and its own
	// tests.
	InputPrompter InputPrompter

	// InputWalker runs a host-side interactive pass over every declared literal
	// input BEFORE the image-boot / in-process branch (#2063), collecting a value
	// for each into Inputs. It is the only interactive seam that reaches the
	// sealed-image mainline: the in-container prompter is never consulted for a
	// whole-plan-pinned unit (Run short-circuits to the boot before resolveInputs
	// runs), so wiring only InputPrompter would ship the guided walk inert on the
	// primary path. Run consults it only when it is non-nil AND this run is NOT
	// the in-container image-boot re-entry (InPinnedImage): the re-entry runs
	// non-TTY and must never re-walk, so gating on InPinnedImage makes
	// no-recursion structural rather than dependent on the container's TTY state.
	// Nil (the default) skips the walk and keeps today's silent-default one-shot
	// launch. The CLI wires it only on an interactive TTY without
	// `--accept-defaults`.
	InputWalker InputWalker

	// OutDir is the directory file-target artifacts are written to. Empty
	// skips writing (artifacts are still recorded in the result).
	OutDir string
}

// RunResult is the outcome of a launch: the resolved inputs, the step outputs,
// the materialized artifacts, and the emitted audit record ids.
type RunResult struct {
	// ContentHash is the verified content hash of the frozen unit that ran.
	ContentHash string
	// ResolvedInputs is the frozen resolved-input set (Phase A output).
	ResolvedInputs map[string]any
	// StepOutputs maps steps.<id> → its named outputs.
	StepOutputs map[string]map[string]any
	// Artifacts are the materialized output artifacts.
	Artifacts []Artifact
	// AuditIDs are the audit record ids emitted to the sink.
	AuditIDs []string
	// Pending is set (non-nil) when the run SUSPENDED instead of completing
	// (#2100): a suspendable run hit an unfulfilled seam or a pending approval.
	// A completed run has Pending == nil. When Pending is set, StepOutputs /
	// Artifacts / AuditIDs reflect only what completed THIS call, and the caller
	// fulfills the pending step (SuspendResult) and resumes with the carried
	// memo. Run returns a nil error alongside a non-nil Pending: a suspend is not
	// a failure.
	Pending *SuspendResult
}

// IsSuspended reports whether the run suspended rather than completing (#2100).
// A caller that only cares about the completed-vs-suspended distinction reads
// this instead of nil-checking Pending directly.
func (r RunResult) IsSuspended() bool { return r.Pending != nil }

// Run is the deterministic Launch entry point (#1511). It loads and verifies
// the frozen unit, resolves declared inputs once (#1523), walks the step graph
// in topological order through the sealed action boundary with trust-contract
// enforcement (#1507), materializes declared file artifacts (#1519), writes
// them to OutDir, and emits the customer-owned audit. Any verification failure
// or step error aborts with zero side effects beyond the audit trail.
func Run(ctx context.Context, opts Options) (RunResult, error) {
	lp, err := LoadVerified(opts.Store, opts.Name, opts.Version)
	if err != nil {
		return RunResult{}, err
	}
	// Host-side publisher-trust gate (#1900). It runs after verification (the
	// signing key and declared publisher are proven against the content hash)
	// and before any boot or step, so the host, which holds the keyring,
	// enforces trust before booting a pinned image. It is a no-op when no
	// verifier is wired or the plan declares no publisher (the CLI nils the
	// verifier on the image-boot re-entry, where the host already gated).
	if err := gatePublisher(opts.PublisherVerifier, lp); err != nil {
		return RunResult{}, err
	}
	// A tool step requires the plan's declared environment: its argv is
	// signed against the composed image the lock pinned, so executing it
	// anywhere else (the operator's host, an unpinned in-process run) would
	// enter an environment the attestation never certified. Refuse a
	// tool-step plan with no pinned environment unless this run is ALREADY
	// inside the booted pin (#1829). This fail-closed check precedes the
	// interactive input walk below so a doomed tool-step run refuses BEFORE
	// dragging the operator through a guided walk it can only discard; the
	// refusal never reads opts.Inputs, so ordering it first is behavior-
	// preserving for every run that does proceed.
	if planHasToolSteps(lp.Plan) && len(lp.ResolvedImages) == 0 && !opts.InPinnedImage {
		return RunResult{}, fmt.Errorf(
			"flightplan: plan declares tool steps but pins no environment image; tool steps run only inside the declared environment")
	}
	// Host-side interactive input walk (#2063). It runs BEFORE the image-boot /
	// in-process branch so the guided walk reaches the sealed-image mainline: a
	// whole-plan-pinned unit short-circuits to runInImage before resolveInputs
	// (the InputPrompter caller) ever runs, so the walk is the only interactive
	// surface that can collect inputs on the boot path. The collected values are
	// merged into opts.Inputs, and BOTH downstream branches then consume them
	// identically (the image path threads them onto the container `--input`
	// re-entry; the in-process path resolves them as overrides). Gated on
	// !InPinnedImage so the in-container re-entry never re-walks (structural
	// no-recursion, not dependent on the container's non-TTY state).
	if opts.InputWalker != nil && !opts.InPinnedImage {
		walked, err := opts.InputWalker.Walk(lp.Plan.Inputs, opts.Inputs)
		if err != nil {
			return RunResult{}, err
		}
		opts.Inputs = walked
	}
	// When the verified lock pins a WHOLE-PLAN image, boot that exact image
	// and run the plan inside it (#1731). The image-boot re-entry
	// (InPinnedImage) stays in-process: it is already running inside the
	// booted pin, so in-process IS the certified environment and booting
	// again would recurse. Tool steps (#1829) also stay in-process; the
	// re-entered executor runs each one as a subprocess in the same
	// container, never a sibling boot.
	if hasWholePlanImage(lp.ResolvedImages) && !opts.InPinnedImage {
		return runInImage(ctx, lp, opts)
	}
	// LLM-seam surface gate (#2102). A plan carrying a marked llm-seam step can
	// only run on a surface that can fulfill the seam: an interactive agent
	// context (`aileron launch`) wires a non-nil Seam, and the daemon HTTP door
	// runs suspendable so an unfulfilled seam SUSPENDS for the agent to fulfill
	// via resume. Every other surface (a plain `aileron skill launch` with no
	// agent-backed provider) can neither wire a provider nor suspend, so a seam
	// plan there would reach the mid-run runSeam nil-guard only after earlier
	// deterministic steps already ran. This precondition fails such a launch
	// CLOSED before any step runs, with a message that names the fulfilling
	// surface.
	//
	// The gate keys on three conditions together:
	//   - planHasSeamSteps(lp.Plan): the plan actually carries a seam.
	//   - opts.Seam == nil: no provider is wired (an agent launch wires one, so
	//     the gate is inert there — including the in-container image-boot
	//     re-entry, where the agent passes a real Seam and in-process IS the
	//     certified environment).
	//   - not suspendable: this surface cannot suspend/resume. The daemon HTTP
	//     handler sets Suspendable, so a seam plan there SUSPENDS (SuspendKindSeam)
	//     for the agent, and the gate stays inert — that surface fulfills the seam
	//     out of band, it does not fail closed. "Suspendable" mirrors runPlan's own
	//     definition (opts.Suspendable OR a non-nil resume memo), so a resume can
	//     never be preempted by this gate even if a caller supplies ResumeOutputs
	//     without the explicit Suspendable flag.
	//
	// Placement is deliberate: this guards only the in-process runPlan branch,
	// AFTER the image-boot branch. An image-pinning plan (the committed worked
	// example pins an environment AND carries a seam) still boots (or hits the
	// image nil-guard) exactly as before, so the existing image tests are
	// unaffected. The daemon caller is internal/app/handlers_flightplans.go; it
	// is a governed runtime.Run caller, but because it runs Suspendable the gate
	// is inert for it and a seam plan suspends rather than failing closed.
	suspendable := opts.Suspendable || opts.ResumeOutputs != nil
	if planHasSeamSteps(lp.Plan) && opts.Seam == nil && !suspendable {
		return RunResult{}, fmt.Errorf(
			"flightplan: plan %q contains an llm-seam step; it can only be launched from an interactive agent context (aileron launch), not this surface",
			lp.Plan.Name)
	}
	return runPlan(ctx, lp.Plan, lp.ContentHash, lp.SignerFingerprint, lp.StepTrust, opts)
}

// planHasToolSteps reports whether the plan carries any `kind: tool` step.
func planHasToolSteps(plan *Plan) bool {
	for _, s := range plan.Steps {
		if s.Kind == KindTool {
			return true
		}
	}
	return false
}

// planHasSeamSteps reports whether the plan carries any `kind: llm-seam` step.
// It mirrors planHasToolSteps and is the marker the surface gate (#2102) keys
// on: the presence of a seam step is what makes a launch surface-sensitive.
func planHasSeamSteps(plan *Plan) bool {
	for _, s := range plan.Steps {
		if s.Kind == KindLLMSeam {
			return true
		}
	}
	return false
}

// runPlan executes an already-loaded, verified plan. It is factored out so
// tests drive the full Phase A/B/materialize/audit pipeline from an in-memory
// plan without a store on disk, while Run handles the load+verify gate. The
// signerFingerprint is the verified author-key fingerprint threaded onto the
// per-output audit records as the plan's signer identity (#1752). stepTrust
// is the verified lock's sealed per-step reach (#1829): the ONLY source of
// the network reach a tool step is scoped to; nil for a plan with no sealed
// tool-step reach.
func runPlan(ctx context.Context, plan *Plan, contentHash, signerFingerprint string, stepTrust map[string]freeze.StepReach, opts Options) (RunResult, error) {
	clk := opts.Clock
	if clk == nil {
		clk = SystemClock{}
	}
	reg := opts.Transforms
	if reg == nil {
		reg = NewTransformRegistry()
	}
	enf := &enforcer{dispatcher: opts.Dispatcher, approver: opts.Approver}

	// Phase A: resolve declared inputs once at the launch boundary.
	inputs, err := resolveInputs(ctx, plan, opts.Inputs, clk, enf, opts.InputPrompter)
	if err != nil {
		return RunResult{}, err
	}

	// Suspend/resume wiring (#2100). A run is suspendable when the caller opts in
	// explicitly (Options.Suspendable) or supplies a resume memo (ResumeOutputs
	// implies a resume, which is always a suspendable sequence). The memo, when
	// present, injects already-computed step outputs so each step runs exactly
	// once across the whole suspend/resume sequence.
	suspendable := opts.Suspendable || opts.ResumeOutputs != nil
	resumeMemo := resumeMemoFromOptions(opts.ResumeOutputs)

	// Phase B: walk the DAG. The tool-step runner and the sealed per-step
	// reach are threaded so tool steps run as scoped subprocesses; a tool
	// step with no runner configured is an explicit error inside the executor.
	x := &executor{
		plan: plan, enforcer: enf, transform: reg, seam: opts.Seam,
		toolRunner: opts.ToolRunner, stepTrust: stepTrust,
		suspendable: suspendable, resumeOutputs: resumeMemo,
	}
	st, sig, runErr := x.execute(ctx, inputs)

	// Mint a launch-scoped invocation id so every audit record from this launch
	// correlates. Provenance is fixed for the launch: reaching runPlan means the
	// verify gate passed, so the signature status is "verified".
	prov := launchProvenance{
		Skill:           plan.Name,
		ContentHash:     contentHash,
		SignedBy:        signerFingerprint,
		SignatureStatus: signatureStatusVerified,
		InvocationID:    uuid.NewString(),
	}

	if runErr != nil {
		// A hard error still emits the audit so a mid-run denial is recorded (the
		// audit is an append-only companion, not gated on success).
		emitAudit(ctx, opts.Audit, st, prov)
		return RunResult{}, runErr
	}

	// A suspend is not a failure and not a terminal launch (#2100). Emit the
	// per-step audit for steps that actually ran THIS call, but NOT the
	// per-launch summary record (RecordKindLaunch) — the terminal (completing)
	// resume emits that. The suspend result carries the accumulated memo and this
	// call's audit ids so the caller resumes without re-running the completed
	// prefix.
	if sig != nil {
		auditIDs := emitStepAudit(ctx, opts.Audit, st, prov)
		runID := opts.RunID
		if runID == "" {
			runID = uuid.NewString()
		}
		return RunResult{
			ContentHash:    contentHash,
			ResolvedInputs: inputs.Values,
			StepOutputs:    stepOutputsMap(st),
			Artifacts:      st.artifacts,
			AuditIDs:       auditIDs,
			Pending: &SuspendResult{
				RunID:       runID,
				Kind:        sig.kind,
				StepID:      sig.stepID,
				Seam:        sig.seam,
				Approval:    sig.approval,
				StepOutputs: stepOutputsMap(st),
				AuditIDs:    auditIDs,
			},
		}, nil
	}

	// A completed run emits the full audit (per-step records plus the per-launch
	// summary), exactly as before.
	auditIDs := emitAudit(ctx, opts.Audit, st, prov)

	// Write file-target artifacts to OutDir.
	if err := writeArtifacts(opts.OutDir, st.artifacts); err != nil {
		return RunResult{}, err
	}

	return RunResult{
		ContentHash:    contentHash,
		ResolvedInputs: inputs.Values,
		StepOutputs:    stepOutputsMap(st),
		Artifacts:      st.artifacts,
		AuditIDs:       auditIDs,
	}, nil
}

// resumeMemoFromOptions converts the public resume memo (stepId → its named
// outputs) into the internal stepResult shape the executor injects (#2100). A
// nil input yields a nil memo, so a fresh launch injects nothing.
func resumeMemoFromOptions(in map[string]map[string]any) map[string]stepResult {
	if in == nil {
		return nil
	}
	out := make(map[string]stepResult, len(in))
	for id, outs := range in {
		out[id] = stepResult(outs)
	}
	return out
}

// writeArtifacts writes every file-target artifact under outDir at its declared
// path. An empty outDir skips writing (the artifacts are still in the result).
// Paths are constrained to stay within outDir so a crafted declared path can
// never escape the output directory.
func writeArtifacts(outDir string, artifacts []Artifact) error {
	if outDir == "" {
		return nil
	}
	for _, a := range artifacts {
		if !a.Written {
			continue
		}
		dest, err := safeJoin(outDir, a.Path)
		if err != nil {
			return fmt.Errorf("flightplan: artifact %q: %w", a.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("flightplan: create artifact dir for %q: %w", a.Name, err)
		}
		if err := os.WriteFile(dest, a.Content, 0o644); err != nil {
			return fmt.Errorf("flightplan: write artifact %q: %w", a.Name, err)
		}
	}
	return nil
}

// safeJoin joins base and a relative path, refusing any path that escapes base
// (absolute paths or `..` traversal). This is the materialization boundary: a
// declared output path is operator-authored but still constrained.
func safeJoin(base, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative to the output directory", rel)
	}
	dest := filepath.Join(base, rel)
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	rel2, err := filepath.Rel(absBase, absDest)
	if err != nil {
		return "", err
	}
	if rel2 == ".." || filepath.IsAbs(rel2) || hasDotDotPrefix(rel2) {
		return "", fmt.Errorf("path %q escapes the output directory", rel)
	}
	return dest, nil
}

func hasDotDotPrefix(p string) bool {
	return len(p) >= 3 && p[0] == '.' && p[1] == '.' && (p[2] == filepath.Separator)
}

// stepOutputsMap converts the internal step-output map into the public shape.
func stepOutputsMap(st execState) map[string]map[string]any {
	out := make(map[string]map[string]any, len(st.stepOutput))
	for id, sr := range st.stepOutput {
		out[id] = map[string]any(sr)
	}
	return out
}
