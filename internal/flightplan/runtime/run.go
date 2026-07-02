package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
	// Seam is the single marked LLM seam. Nil (the v1 default) makes any
	// llm-seam step error, so a default launch reaches no LLM.
	Seam LLMSeam
	// ImageRunner boots the verified pinned rung-1/rung-2 image and runs the
	// plan inside it. When the loaded plan carries a resolved image pin and this
	// seam is wired, Run delegates to it; when the plan pins no image, Run stays
	// on the in-process path and never touches this seam. A plan that pins an
	// image with no ImageRunner configured is an explicit error, never a silent
	// in-process fallback (a declared rung must be entered to honor the
	// attestation).
	ImageRunner ImageRunner
	// ToolImageRunner dispatches an individual rung-3 step to its pinned sibling
	// tool image with mount → run → collect I/O (#1733). Unlike ImageRunner, the
	// plan orchestration stays in-process (runPlan); only the per-step tool
	// dispatch shells out. When a loaded plan carries a rung-3 step and this seam
	// is unset, that step is an explicit error, never a silent skip (mirrors the
	// ImageRunner nil-guard discipline: a declared tool dispatch must be entered).
	ToolImageRunner ToolImageRunner
	// InPinnedImage marks this run as already executing INSIDE the verified
	// pinned rung-1/rung-2 image — the image-boot re-entry (#1731). It routes
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
}

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
	// When the verified lock pins a rung-1/rung-2 WHOLE-PLAN image, boot that
	// exact image and run the plan inside it (#1731). Rung-3 pins are per-step
	// sibling-tool dispatches (they carry a StepID) and must NOT be mis-routed
	// into runInImage: the plan orchestration stays in-process and only the
	// individual step shells out to its tool image (#1733). So the boot branch
	// fires only for whole-plan pins (no per-step StepID); rung-3 stays on
	// runPlan, where the executor dispatches each tool step through the seam.
	// The image-boot re-entry (InPinnedImage) also stays in-process: it is
	// already running inside the booted pin, so in-process IS the certified
	// environment and booting again would recurse.
	if hasWholePlanImage(lp.ResolvedImages) && !opts.InPinnedImage {
		return runInImage(ctx, lp, opts)
	}
	return runPlan(ctx, lp.Plan, lp.ContentHash, lp.SignerFingerprint, opts)
}

// runPlan executes an already-loaded, verified plan. It is factored out so
// tests drive the full Phase A/B/materialize/audit pipeline from an in-memory
// plan without a store on disk, while Run handles the load+verify gate. The
// signerFingerprint is the verified author-key fingerprint threaded onto the
// per-output audit records as the plan's signer identity (#1752).
func runPlan(ctx context.Context, plan *Plan, contentHash, signerFingerprint string, opts Options) (RunResult, error) {
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
	inputs, err := resolveInputs(ctx, plan, opts.Inputs, clk, enf)
	if err != nil {
		return RunResult{}, err
	}

	// Phase B: walk the DAG. The tool-image runner is threaded so rung-3 steps
	// dispatch to their pinned sibling image; a rung-3 step with no runner
	// configured is an explicit error inside the executor.
	x := &executor{plan: plan, enforcer: enf, transform: reg, seam: opts.Seam, toolRunner: opts.ToolImageRunner}
	st, runErr := x.execute(ctx, inputs)

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

	// Emit the audit regardless of run outcome so a mid-run denial is recorded
	// (the audit is an append-only companion, not gated on success).
	auditIDs := emitAudit(ctx, opts.Audit, st, prov)

	if runErr != nil {
		return RunResult{}, runErr
	}

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
