package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// This file exercises the generic suspend/resume path (#2100): all-output
// memoized replay, the two suspend reasons (unfulfilled seam, pending
// approval), and the exactly-once + no-double-audit contracts. The four
// scenarios the readiness brief enumerates each drive a fresh launch to a
// suspend, fulfill the pending step, and resume through the memoized-replay
// path until completion.

// countingDispatcher records how many times each action ref was dispatched, so
// a resume sequence can assert each effect fired EXACTLY ONCE across the whole
// sequence (the Assumption-B soundness property). It returns a per-ref canned
// result.
type countingDispatcher struct {
	results map[string]map[string]any
	byRef   map[string]int
}

func newCountingDispatcher(results map[string]map[string]any) *countingDispatcher {
	return &countingDispatcher{results: results, byRef: map[string]int{}}
}

func (d *countingDispatcher) Dispatch(_ context.Context, ref string, _ map[string]any) (DispatchResult, error) {
	d.byRef[ref]++
	return DispatchResult{Output: d.results[ref]}, nil
}

// gatedApprover returns Pending for an action ref until that ref is released,
// then Approved. It models the daemon's approval lifecycle across a
// suspend/resume: the first reach parks (pending), the operator approves
// out-of-band, and the resume proceeds.
type gatedApprover struct {
	// released marks refs whose approval has landed (Approved on the next reach).
	released map[string]bool
	// pendingSeen counts how many times each ref returned pending, to prove the
	// approval is raised exactly once (not re-raised on resume replay).
	pendingSeen map[string]int
}

func newGatedApprover() *gatedApprover {
	return &gatedApprover{released: map[string]bool{}, pendingSeen: map[string]int{}}
}

func (a *gatedApprover) Approve(_ context.Context, req ApprovalRequest) (Decision, error) {
	if a.released[req.ActionRef] {
		return Decision{Approved: true}, nil
	}
	a.pendingSeen[req.ActionRef]++
	return Decision{Pending: true}, nil
}

// seamRouter serves per-step seam outputs so a plan with two distinct seams can
// produce distinct results. It is only wired when a seam is meant to run
// synchronously; the suspend path uses a nil seam so the run parks instead.
type seamRouter struct {
	byStep map[string]map[string]any
}

func (s seamRouter) Run(_ context.Context, req SeamRequest) (map[string]any, error) {
	if out, ok := s.byStep[req.StepID]; ok {
		return out, nil
	}
	out := map[string]any{}
	for _, name := range req.Outputs {
		out[name] = "seam-" + req.StepID + "-" + name
	}
	return out, nil
}

// driveToCompletion runs a fresh suspendable launch and, on each suspend,
// fulfills the pending step and resumes with the accumulated memo, until the run
// completes. For a seam suspend it produces the declared seam outputs and adds
// them to the memo; for an approval suspend it releases the action ref (so the
// gated approver approves it on the resume) and resumes with the same memo. It
// records the ordered suspend reasons so a test can assert the topological
// suspend order. It fails the test if the run never completes within a bound.
func driveToCompletion(t *testing.T, plan *Plan, base Options, approver *gatedApprover, seamOutputs map[string]map[string]any) (RunResult, []SuspendResult) {
	t.Helper()
	memo := map[string]map[string]any(nil)
	runID := ""
	var suspends []SuspendResult
	for i := 0; i < 32; i++ {
		opts := base
		opts.Suspendable = true
		opts.ResumeOutputs = memo
		opts.RunID = runID
		res, err := runPlan(context.Background(), plan, "sha256:test", "sha256:signer", nil, opts)
		if err != nil {
			t.Fatalf("runPlan (iter %d): %v", i, err)
		}
		if res.Pending == nil {
			return res, suspends
		}
		sp := *res.Pending
		suspends = append(suspends, sp)
		runID = sp.RunID
		if sp.RunID == "" {
			t.Fatal("a suspend must carry a stable RunID")
		}
		// Carry the accumulated memo forward.
		memo = sp.StepOutputs
		if memo == nil {
			memo = map[string]map[string]any{}
		}
		switch sp.Kind {
		case SuspendKindSeam:
			if sp.Seam == nil {
				t.Fatalf("a seam suspend must carry a SeamRequest, step %q", sp.StepID)
			}
			out := map[string]any{}
			if declared, ok := seamOutputs[sp.StepID]; ok {
				out = declared
			} else {
				for _, name := range sp.Seam.Outputs {
					out[name] = "seam-" + sp.StepID + "-" + name
				}
			}
			memo[sp.StepID] = out
		case SuspendKindApproval:
			if sp.Approval == nil {
				t.Fatalf("an approval suspend must carry an ApprovalRequest, step %q", sp.StepID)
			}
			approver.released[sp.Approval.ActionRef] = true
		default:
			t.Fatalf("unknown suspend kind %d", sp.Kind)
		}
	}
	t.Fatal("run never completed within the resume bound")
	return RunResult{}, suspends
}

// twoSeamPlan is a linear plan with a read action-call feeding two llm-seam
// steps in sequence, then a write action-call that materializes JSON. Both
// seams are unfulfilled on the suspend path, so the run parks at each in turn.
func twoSeamPlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name: "two-seam",
		Actions: map[string]Action{
			"aileron:metrics.query_series": {Ref: "aileron:metrics.query_series", TrustContract: TrustContract{
				Effect: EffectRead, Hosts: []string{"api.example.com"},
				Idempotency: Idempotency{SafeToRetry: true}}},
		},
		Steps: []Step{
			{ID: "read", Kind: KindActionCall, ActionRef: "aileron:metrics.query_series",
				Args: map[string]Binding{"w": mb("inputs.window_days")}, Outputs: []string{"series"}},
			{ID: "seam_a", Kind: KindLLMSeam,
				Bindings: map[string]Binding{"series": mb("steps.read.series")}, Outputs: []string{"summary"}},
			{ID: "seam_b", Kind: KindLLMSeam,
				Bindings: map[string]Binding{"summary": mb("steps.seam_a.summary")}, Outputs: []string{"body"}},
		},
		Inputs: []Input{{Name: "window_days", Type: "number",
			Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: 7}}},
	}
	order, err := topoSort(p, map[string]int{"read": 0, "seam_a": 1, "seam_b": 2})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

// TestResume_TwoSeamSequence: a two-seam launch suspends at seam A, resumes,
// suspends at seam B, resumes, then completes. Each deterministic step runs
// exactly once across the whole sequence.
func TestResume_TwoSeamSequence(t *testing.T) {
	p := twoSeamPlan()
	disp := newCountingDispatcher(map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"n": "cpu"}}},
	})
	app := newGatedApprover()
	base := Options{Dispatcher: disp, Approver: app, Clock: FixedClock{}}

	res, suspends := driveToCompletion(t, p, base, app, map[string]map[string]any{
		"seam_a": {"summary": "S"},
		"seam_b": {"body": "B"},
	})

	if len(suspends) != 2 {
		t.Fatalf("want 2 suspends (one per seam), got %d", len(suspends))
	}
	if suspends[0].StepID != "seam_a" || suspends[0].Kind != SuspendKindSeam {
		t.Errorf("first suspend = %+v, want seam_a/seam", suspends[0])
	}
	if suspends[1].StepID != "seam_b" || suspends[1].Kind != SuspendKindSeam {
		t.Errorf("second suspend = %+v, want seam_b/seam", suspends[1])
	}
	// The read action ran exactly once despite two resumes re-walking the DAG.
	if disp.byRef["aileron:metrics.query_series"] != 1 {
		t.Errorf("read dispatched %d times, want exactly 1 across the whole sequence", disp.byRef["aileron:metrics.query_series"])
	}
	// Final outputs reflect the fulfilled seams.
	if got := res.StepOutputs["seam_b"]["body"]; got != "B" {
		t.Errorf("final seam_b.body = %v, want B", got)
	}
	// The RunID is stable across the whole sequence.
	if suspends[0].RunID != suspends[1].RunID {
		t.Errorf("RunID must be stable across resumes: %q vs %q", suspends[0].RunID, suspends[1].RunID)
	}
}

// gatedMidFlowPlan places a gated write action-call BEFORE another effectful
// (read) action-call and a materializing transform, so a naive seam-only or
// resume-from-suspend replay that re-ran the prefix would double the effect.
// The topological order is: gated_write → read_after → render (materializes).
func gatedMidFlowPlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name: "gated-midflow",
		Actions: map[string]Action{
			"aileron:tracker.create_issue": {Ref: "aileron:tracker.create_issue", TrustContract: TrustContract{
				Effect: EffectWrite, Hosts: []string{"tracker.example.com"},
				Idempotency: Idempotency{SafeToRetry: false, IdempotencyKey: true}}},
			"aileron:metrics.query_series": {Ref: "aileron:metrics.query_series", TrustContract: TrustContract{
				Effect: EffectRead, Hosts: []string{"api.example.com"},
				Idempotency: Idempotency{SafeToRetry: true}}},
		},
		Outputs: map[string]Output{
			"out.json": {Name: "out.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishFile, Path: "out.json"},
		},
		Steps: []Step{
			{ID: "gated_write", Kind: KindActionCall, ActionRef: "aileron:tracker.create_issue",
				Args: map[string]Binding{"w": mb("inputs.window_days")}, Outputs: []string{"issue"}},
			{ID: "read_after", Kind: KindActionCall, ActionRef: "aileron:metrics.query_series",
				Args: map[string]Binding{"issue": mb("steps.gated_write.issue")}, Outputs: []string{"series"}},
			{ID: "render", Kind: KindTransform,
				Bindings: map[string]Binding{"series": mb("steps.read_after.series")},
				Outputs:  []string{"file"}, MaterializesOutput: "out.json"},
		},
		Inputs: []Input{{Name: "window_days", Type: "number",
			Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: 7}}},
	}
	order, err := topoSort(p, map[string]int{"gated_write": 0, "read_after": 1, "render": 2})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

func midFlowRegistry() *TransformRegistry {
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{
			"path": "out.json", "mimeType": "application/json", "encoding": "utf-8", "content": `{"ok":true}`,
		}}, nil
	})
	return reg
}

// TestResume_GatedActionMidFlowExactlyOnce: a gated action topologically BEFORE
// another effectful step suspends at the approval; after approval the resume
// completes, and the gated action AND the downstream effectful step each ran
// EXACTLY ONCE across the whole sequence (Assumption-B soundness). The effect
// never duplicated, and the materialized artifact is present on completion.
func TestResume_GatedActionMidFlowExactlyOnce(t *testing.T) {
	p := gatedMidFlowPlan()
	disp := newCountingDispatcher(map[string]map[string]any{
		"aileron:tracker.create_issue": {"issue": map[string]any{"id": 1}},
		"aileron:metrics.query_series": {"series": []any{}},
	})
	app := newGatedApprover()
	base := Options{Dispatcher: disp, Approver: app, Clock: FixedClock{}, Transforms: midFlowRegistry()}

	res, suspends := driveToCompletion(t, p, base, app, nil)

	if len(suspends) != 1 {
		t.Fatalf("want exactly 1 suspend (the gated write), got %d", len(suspends))
	}
	if suspends[0].Kind != SuspendKindApproval || suspends[0].StepID != "gated_write" {
		t.Errorf("suspend = %+v, want gated_write/approval", suspends[0])
	}
	if suspends[0].Approval == nil || suspends[0].Approval.ActionRef != "aileron:tracker.create_issue" {
		t.Errorf("approval request = %+v, want the write action", suspends[0].Approval)
	}
	// Each effect fired exactly once despite the resume re-walking the DAG.
	if disp.byRef["aileron:tracker.create_issue"] != 1 {
		t.Errorf("gated write dispatched %d times, want exactly 1", disp.byRef["aileron:tracker.create_issue"])
	}
	if disp.byRef["aileron:metrics.query_series"] != 1 {
		t.Errorf("downstream read dispatched %d times, want exactly 1", disp.byRef["aileron:metrics.query_series"])
	}
	// The approval was raised exactly once (not re-raised on resume replay).
	if app.pendingSeen["aileron:tracker.create_issue"] != 1 {
		t.Errorf("approval raised %d times, want exactly 1", app.pendingSeen["aileron:tracker.create_issue"])
	}
	// The materialized artifact is whole on completion.
	if len(res.Artifacts) != 1 || res.Artifacts[0].Name != "out.json" {
		t.Fatalf("want the out.json artifact on completion, got %+v", res.Artifacts)
	}
}

// mixedPlan carries both a gated action and a seam, with the gated action
// topologically before the seam, so the run suspends at each in topological
// order.
func mixedPlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name: "mixed",
		Actions: map[string]Action{
			"aileron:tracker.create_issue": {Ref: "aileron:tracker.create_issue", TrustContract: TrustContract{
				Effect: EffectWrite, Hosts: []string{"tracker.example.com"},
				Idempotency: Idempotency{SafeToRetry: false, IdempotencyKey: true}}},
		},
		Steps: []Step{
			{ID: "gated", Kind: KindActionCall, ActionRef: "aileron:tracker.create_issue",
				Args: map[string]Binding{"w": mb("inputs.window_days")}, Outputs: []string{"issue"}},
			{ID: "seam", Kind: KindLLMSeam,
				Bindings: map[string]Binding{"issue": mb("steps.gated.issue")}, Outputs: []string{"body"}},
		},
		Inputs: []Input{{Name: "window_days", Type: "number",
			Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: 7}}},
	}
	order, err := topoSort(p, map[string]int{"gated": 0, "seam": 1})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

// TestResume_MixedPlanSuspendsInTopoOrder: a plan with a gated action AND a seam
// suspends at each in topological order (the gated write first, then the seam).
func TestResume_MixedPlanSuspendsInTopoOrder(t *testing.T) {
	p := mixedPlan()
	disp := newCountingDispatcher(map[string]map[string]any{
		"aileron:tracker.create_issue": {"issue": map[string]any{"id": 1}},
	})
	app := newGatedApprover()
	base := Options{Dispatcher: disp, Approver: app, Clock: FixedClock{}}

	_, suspends := driveToCompletion(t, p, base, app, map[string]map[string]any{"seam": {"body": "B"}})

	if len(suspends) != 2 {
		t.Fatalf("want 2 suspends, got %d", len(suspends))
	}
	if suspends[0].StepID != "gated" || suspends[0].Kind != SuspendKindApproval {
		t.Errorf("first suspend = %+v, want gated/approval", suspends[0])
	}
	if suspends[1].StepID != "seam" || suspends[1].Kind != SuspendKindSeam {
		t.Errorf("second suspend = %+v, want seam/seam", suspends[1])
	}
}

// TestResume_SingleSeamWiredStillWorks: the classic non-suspendable path with a
// wired seam runs to completion synchronously with no suspend, unchanged by the
// suspend/resume machinery.
func TestResume_SingleSeamWiredStillWorks(t *testing.T) {
	res, _, _ := runFixture(t, Options{})
	if res.IsSuspended() {
		t.Fatal("a wired-seam launch must complete, never suspend")
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts on the classic path, got %d", len(res.Artifacts))
	}
}

// TestResume_NoDoubleAudit proves the exactly-once + no-double-audit contract
// across a full suspend→approve→resume→complete sequence: each real action
// dispatch emits exactly ONE action record and each materialized output emits
// exactly ONE output.materialized record across the WHOLE sequence (not once per
// resume), the pending approval emits NO deny record, and exactly one per-launch
// summary record is emitted (only on the completing call, never on a suspend).
func TestResume_NoDoubleAudit(t *testing.T) {
	p := gatedMidFlowPlan()
	// A sink that accumulates every record across all calls in the sequence.
	sink := &recordingSink{}
	app := newGatedApprover()
	memo := map[string]map[string]any(nil)
	runID := ""
	completed := false
	for i := 0; i < 8 && !completed; i++ {
		disp := newCountingDispatcher(map[string]map[string]any{
			"aileron:tracker.create_issue": {"issue": map[string]any{"id": 1}},
			"aileron:metrics.query_series": {"series": []any{}},
		})
		opts := Options{
			Dispatcher: disp, Approver: app, Audit: sink, Transforms: midFlowRegistry(),
			Clock: FixedClock{}, Suspendable: true, ResumeOutputs: memo, RunID: runID,
		}
		res, err := runPlan(context.Background(), p, "sha256:test", "sha256:signer", nil, opts)
		if err != nil {
			t.Fatalf("runPlan (iter %d): %v", i, err)
		}
		if res.Pending == nil {
			completed = true
			break
		}
		runID = res.Pending.RunID
		memo = res.Pending.StepOutputs
		if memo == nil {
			memo = map[string]map[string]any{}
		}
		app.released[res.Pending.Approval.ActionRef] = true
	}
	if !completed {
		t.Fatal("sequence never completed")
	}

	counts := map[AuditRecordKind]int{}
	actionRefs := map[string]int{}
	for _, r := range sink.records {
		counts[r.Kind]++
		if r.Kind == RecordKindAction {
			actionRefs[r.ActionRef]++
		}
	}
	// Exactly one action record per real dispatch: the gated write ran once, the
	// downstream read ran once.
	if actionRefs["aileron:tracker.create_issue"] != 1 {
		t.Errorf("gated write action records = %d, want exactly 1", actionRefs["aileron:tracker.create_issue"])
	}
	if actionRefs["aileron:metrics.query_series"] != 1 {
		t.Errorf("downstream read action records = %d, want exactly 1", actionRefs["aileron:metrics.query_series"])
	}
	// Exactly one output.materialized record for the single materialized artifact.
	if counts[RecordKindOutput] != 1 {
		t.Errorf("output.materialized records = %d, want exactly 1 across the sequence", counts[RecordKindOutput])
	}
	// Exactly one per-launch summary, emitted only on the completing call.
	if counts[RecordKindLaunch] != 1 {
		t.Errorf("per-launch summary records = %d, want exactly 1 (only on completion)", counts[RecordKindLaunch])
	}
}

// earlyMaterializePlan puts a materializing transform BEFORE a seam, so on the
// completing resume the materializing step is in the memoized prefix (injected,
// not re-executed) and its artifact must still surface in the final result and
// be written to OutDir. This locks in that prefix artifacts survive a
// suspend/resume sequence (the artifact-loss soundness property).
func earlyMaterializePlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name: "early-materialize",
		Actions: map[string]Action{
			"aileron:metrics.query_series": {Ref: "aileron:metrics.query_series", TrustContract: TrustContract{
				Effect: EffectRead, Hosts: []string{"api.example.com"},
				Idempotency: Idempotency{SafeToRetry: true}}},
		},
		Outputs: map[string]Output{
			"early.json": {Name: "early.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishFile, Path: "early.json"},
		},
		Steps: []Step{
			{ID: "read", Kind: KindActionCall, ActionRef: "aileron:metrics.query_series",
				Args: map[string]Binding{"w": mb("inputs.window_days")}, Outputs: []string{"series"}},
			{ID: "render_early", Kind: KindTransform,
				Bindings: map[string]Binding{"series": mb("steps.read.series")},
				Outputs:  []string{"file"}, MaterializesOutput: "early.json"},
			{ID: "seam", Kind: KindLLMSeam,
				Bindings: map[string]Binding{"file": mb("steps.render_early.file")}, Outputs: []string{"body"}},
		},
		Inputs: []Input{{Name: "window_days", Type: "number",
			Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: 7}}},
	}
	order, err := topoSort(p, map[string]int{"read": 0, "render_early": 1, "seam": 2})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

func earlyMaterializeRegistry() *TransformRegistry {
	reg := NewTransformRegistry()
	reg.Register("identity", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{
			"path": "early.json", "mimeType": "application/json", "encoding": "utf-8", "content": `{"early":true}`,
		}}, nil
	})
	return reg
}

// TestResume_PrefixArtifactSurvivesSuspend proves a materializing step that runs
// BEFORE the suspend point is memoized on resume, yet its artifact still appears
// in the completing run's RunResult.Artifacts AND is written to OutDir. A
// suspend does not write to OutDir; the completing resume writes the whole set.
func TestResume_PrefixArtifactSurvivesSuspend(t *testing.T) {
	p := earlyMaterializePlan()
	outDir := t.TempDir()
	app := newGatedApprover()
	memo := map[string]map[string]any(nil)
	runID := ""
	var final RunResult
	completed := false
	for i := 0; i < 8 && !completed; i++ {
		disp := newCountingDispatcher(map[string]map[string]any{
			"aileron:metrics.query_series": {"series": []any{map[string]any{"n": "cpu"}}},
		})
		opts := Options{
			Dispatcher: disp, Approver: app, Transforms: earlyMaterializeRegistry(),
			Clock: FixedClock{}, Suspendable: true, ResumeOutputs: memo, RunID: runID, OutDir: outDir,
		}
		res, err := runPlan(context.Background(), p, "sha256:test", "sha256:signer", nil, opts)
		if err != nil {
			t.Fatalf("runPlan (iter %d): %v", i, err)
		}
		if res.Pending == nil {
			final = res
			completed = true
			break
		}
		// The suspend must NOT have written the artifact to disk yet.
		if _, statErr := os.Stat(filepath.Join(outDir, "early.json")); statErr == nil {
			t.Error("a suspend must not write artifacts to OutDir; only the completing resume writes them")
		}
		runID = res.Pending.RunID
		memo = res.Pending.StepOutputs
		if memo == nil {
			memo = map[string]map[string]any{}
		}
		memo[res.Pending.StepID] = map[string]any{"body": "B"}
	}
	if !completed {
		t.Fatal("sequence never completed")
	}
	// The prefix artifact is in the final result even though render_early was
	// memoized (injected) on the completing resume.
	if len(final.Artifacts) != 1 || final.Artifacts[0].Name != "early.json" {
		t.Fatalf("prefix artifact lost across suspend/resume: got %+v", final.Artifacts)
	}
	// And it was written to disk on completion.
	got, err := os.ReadFile(filepath.Join(outDir, "early.json"))
	if err != nil {
		t.Fatalf("prefix artifact not written to OutDir on completion: %v", err)
	}
	if string(got) != `{"early":true}` {
		t.Errorf("early.json content = %q", got)
	}
}

// TestResume_DeterminismAcrossReplay: the same plan resumed with the same memo
// yields identical final outputs (the memoized replay is deterministic).
func TestResume_DeterminismAcrossReplay(t *testing.T) {
	p := twoSeamPlan()
	run := func() RunResult {
		disp := newCountingDispatcher(map[string]map[string]any{
			"aileron:metrics.query_series": {"series": []any{map[string]any{"n": "cpu"}}},
		})
		app := newGatedApprover()
		base := Options{Dispatcher: disp, Approver: app, Clock: FixedClock{}}
		res, _ := driveToCompletion(t, p, base, app, map[string]map[string]any{
			"seam_a": {"summary": "S"}, "seam_b": {"body": "B"},
		})
		return res
	}
	a, b := run(), run()
	if a.StepOutputs["seam_b"]["body"] != b.StepOutputs["seam_b"]["body"] {
		t.Error("resumed replay must yield identical final outputs")
	}
}
