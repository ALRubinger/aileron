package runtime

import (
	"context"
	"testing"
)

// fixturePlan builds an in-memory plan that mirrors the worked example's
// shape: a read action-call, a transform that materializes a CSV, an llm-seam,
// and a write action-call that materializes JSON. It is the integration
// fixture for execute/run/audit tests, decoupled from on-disk freeze so the
// executor's behavior can be exercised directly.
func fixturePlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name: "weekly-metrics-digest",
		Actions: map[string]Action{
			"aileron:metrics.query_series": {
				Ref: "aileron:metrics.query_series",
				TrustContract: TrustContract{
					Effect: EffectRead, Hosts: []string{"api.example.com"},
					Idempotency: Idempotency{SafeToRetry: true},
					Audit:       AuditStructure{Fields: []string{"operation-effect", "approval-decision", "result"}, Sink: "audit/reads"},
				},
			},
			"aileron:tracker.create_issue": {
				Ref: "aileron:tracker.create_issue",
				TrustContract: TrustContract{
					Effect: EffectWrite, Hosts: []string{"tracker.example.com"},
					Idempotency: Idempotency{SafeToRetry: false, IdempotencyKey: true},
					Audit:       AuditStructure{Fields: []string{"operation-effect", "approval-decision", "result"}, Sink: "audit/writes"},
				},
			},
		},
		Outputs: map[string]Output{
			"digest.csv":       {Name: "digest.csv", MimeType: "text/csv", Encoding: EncodingUTF8, Target: PublishFile, Path: "digest.csv"},
			"filed_issue.json": {Name: "filed_issue.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishFile, Path: "filed_issue.json"},
		},
		Inputs: []Input{
			{Name: "window_days", Type: "number", Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: 7}},
		},
		Steps: []Step{
			{ID: "query_metrics", Kind: KindActionCall, ActionRef: "aileron:metrics.query_series",
				Args: map[string]Binding{"window_days": mb("inputs.window_days")}, Outputs: []string{"series"}},
			{ID: "render_csv", Kind: KindTransform,
				Bindings: map[string]Binding{"series": mb("steps.query_metrics.series")},
				Outputs:  []string{"file"}, MaterializesOutput: "digest.csv"},
			{ID: "summarize", Kind: KindLLMSeam,
				Bindings: map[string]Binding{"series": mb("steps.query_metrics.series")}, Outputs: []string{"issue_body"}},
			{ID: "file_issue", Kind: KindActionCall, ActionRef: "aileron:tracker.create_issue",
				Args:    map[string]Binding{"body": mb("steps.summarize.issue_body")},
				Outputs: []string{"file"}, MaterializesOutput: "filed_issue.json"},
		},
	}
	order, err := topoSort(p, map[string]int{"query_metrics": 0, "render_csv": 1, "summarize": 2, "file_issue": 3})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

// csvTransform emits a file-map for the digest.csv output deterministically.
func csvTransformRegistry() *TransformRegistry {
	reg := NewTransformRegistry()
	return reg
}

// runFixture wires the fixture plan with fakes that produce the file-maps the
// materializing steps need. The transform that materializes digest.csv and the
// action-call that materializes filed_issue.json both emit file-map carriers.
func runFixture(t *testing.T, opts Options) (RunResult, *dispatchRouter, *recordingSink) {
	t.Helper()
	p := fixturePlan()

	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {
			"path": "filed_issue.json", "mimeType": "application/json", "encoding": "utf-8",
			"content": `{"url":"https://tracker.example.com/issues/1"}`,
		},
	}}
	sink := &recordingSink{}
	reg := csvTransformRegistry()
	// The render_csv transform forwards its single binding; to materialize a
	// CSV file-map deterministically, register a transform keyed for the step.
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{
			"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "name\ncpu\n",
		}}, nil
	})

	o := Options{
		Dispatcher: disp,
		Approver:   &fakeApprover{decision: Decision{Approved: true}},
		Audit:      sink,
		Seam:       fakeSeam{out: map[string]any{"issue_body": "A short digest."}},
		Clock:      FixedClock{},
		Transforms: reg,
	}
	if opts.OutDir != "" {
		o.OutDir = opts.OutDir
	}
	if opts.Inputs != nil {
		o.Inputs = opts.Inputs
	}
	if opts.Seam != nil {
		o.Seam = opts.Seam
	}
	res, err := runPlan(context.Background(), p, "sha256:test", "sha256:signer", nil, o)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	return res, disp, sink
}

// dispatchRouter returns a per-ref canned result, so a multi-action plan gets
// distinct results per action.
type dispatchRouter struct {
	fakeDispatcher
	results map[string]map[string]any
}

func (d *dispatchRouter) Dispatch(ctx context.Context, ref string, args map[string]any) (DispatchResult, error) {
	d.calls = append(d.calls, dispatchCall{ref: ref, args: args})
	return DispatchResult{Output: d.results[ref]}, nil
}

// recordingSink records every audit record it receives.
type recordingSink struct {
	records []AuditRecord
}

func (s *recordingSink) Record(_ context.Context, rec AuditRecord) string {
	s.records = append(s.records, rec)
	return "audit-" + rec.ActionRef
}

func TestExecute_WorkedExampleEndToEnd(t *testing.T) {
	res, disp, _ := runFixture(t, Options{})
	// Both action-calls dispatched, in topo order.
	if len(disp.calls) != 2 {
		t.Fatalf("want 2 dispatches, got %d", len(disp.calls))
	}
	if disp.calls[0].ref != "aileron:metrics.query_series" {
		t.Errorf("first dispatch = %q", disp.calls[0].ref)
	}
	if disp.calls[1].ref != "aileron:tracker.create_issue" {
		t.Errorf("second dispatch = %q", disp.calls[1].ref)
	}
	// Two artifacts materialized.
	if len(res.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts, got %d", len(res.Artifacts))
	}
}

func TestExecute_WriteThreadsIdempotencyKey(t *testing.T) {
	_, disp, _ := runFixture(t, Options{})
	writeArgs := disp.calls[1].args
	if _, ok := writeArgs[idempotencyKeyField]; !ok {
		t.Error("the write action (idempotencyKey:true) must thread a stable key")
	}
}

func TestExecute_CapturesMaterializedOutputProvenance(t *testing.T) {
	// The fixture materializes two outputs: digest.csv from a transform step and
	// filed_issue.json from an action-call step. execute must capture one
	// st.outputs entry per materialized artifact, each carrying the originating
	// step's id/kind (and the transform name for the transform step). This is the
	// kind-agnostic capture that makes the transform output auditable (#1752).
	p := fixturePlan()
	// Name the transform on the materializing transform step so the captured
	// provenance carries a concrete transform name (mirrors `transform:
	// html-render`). render_csv is the transform step (index 1).
	for i := range p.Steps {
		if p.Steps[i].ID == "render_csv" {
			p.Steps[i].Transform = "html-render"
		}
	}
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {
			"path": "filed_issue.json", "mimeType": "application/json", "encoding": "utf-8",
			"content": `{"url":"https://tracker.example.com/issues/1"}`,
		},
	}}
	reg := NewTransformRegistry()
	reg.Register("html-render", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{
			"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "name\ncpu\n",
		}}, nil
	})
	x := &executor{
		plan:      p,
		enforcer:  &enforcer{dispatcher: disp, approver: &fakeApprover{decision: Decision{Approved: true}}},
		transform: reg,
		seam:      fakeSeam{out: map[string]any{"issue_body": "A short digest."}},
	}
	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"window_days": 7}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.outputs) != 2 {
		t.Fatalf("captured %d outputs, want 2 (transform + action-call)", len(st.outputs))
	}
	byStep := map[string]materializedOutput{}
	for _, o := range st.outputs {
		byStep[o.StepID] = o
	}
	csv, ok := byStep["render_csv"]
	if !ok {
		t.Fatal("transform materializing step render_csv was not captured")
	}
	if csv.StepKind != KindTransform || csv.Transform != "html-render" {
		t.Errorf("transform output = kind %q transform %q, want transform/html-render", csv.StepKind, csv.Transform)
	}
	if csv.Artifact.Name != "digest.csv" {
		t.Errorf("transform artifact name = %q", csv.Artifact.Name)
	}
	issue, ok := byStep["file_issue"]
	if !ok {
		t.Fatal("action-call materializing step file_issue was not captured")
	}
	if issue.StepKind != KindActionCall {
		t.Errorf("action-call output kind = %q, want action-call", issue.StepKind)
	}
}

func TestExecute_CapturesMaterializedOutputInputs(t *testing.T) {
	// A materializing step captures the binding references and resolved values
	// it executed against, for BOTH an action-call and a transform step, so the
	// per-output audit record can walk back to the exact inputs (#1753).
	p := fixturePlan()
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {
			"path": "filed_issue.json", "mimeType": "application/json", "encoding": "utf-8",
			"content": `{"url":"https://tracker.example.com/issues/1"}`,
		},
	}}
	reg := NewTransformRegistry()
	reg.Register("identity", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{
			"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "name\ncpu\n",
		}}, nil
	})
	x := &executor{
		plan:      p,
		enforcer:  &enforcer{dispatcher: disp, approver: &fakeApprover{decision: Decision{Approved: true}}},
		transform: reg,
		seam:      fakeSeam{out: map[string]any{"issue_body": "A short digest."}},
	}
	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"window_days": 7}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	byStep := map[string]materializedOutput{}
	for _, o := range st.outputs {
		byStep[o.StepID] = o
	}

	// Transform step render_csv binds series → steps.query_metrics.series.
	csv := byStep["render_csv"]
	if got := csv.Binds["series"].Raw; got != "steps.query_metrics.series" {
		t.Errorf("render_csv binding source = %q, want steps.query_metrics.series", got)
	}
	if _, ok := csv.Resolved["series"]; !ok {
		t.Error("render_csv resolved inputs must carry the resolved series value")
	}

	// Action-call step file_issue binds body → steps.summarize.issue_body.
	issue := byStep["file_issue"]
	if got := issue.Binds["body"].Raw; got != "steps.summarize.issue_body" {
		t.Errorf("file_issue binding source = %q, want steps.summarize.issue_body", got)
	}
	if issue.Resolved["body"] != "A short digest." {
		t.Errorf("file_issue resolved body = %v, want the seam output", issue.Resolved["body"])
	}
}

// TestExecute_MemoizedStepNotReExecuted proves a step whose output is in the
// resume memo is INJECTED, not executed: its dispatcher is never invoked and its
// output flows to downstream bindings (#2100). It also confirms a memoized
// materializing step still yields its artifact (materialize-on-replay).
func TestExecute_MemoizedStepNotReExecuted(t *testing.T) {
	p := gatedMidFlowPlan()
	disp := newCountingDispatcher(map[string]map[string]any{
		"aileron:tracker.create_issue": {"issue": map[string]any{"id": 1}},
		"aileron:metrics.query_series": {"series": []any{}},
	})
	x := &executor{
		plan:      p,
		enforcer:  &enforcer{dispatcher: disp, approver: &fakeApprover{decision: Decision{Approved: true}}},
		transform: midFlowRegistry(),
		// Seed the gated write's output so it is injected, never dispatched.
		resumeOutputs: map[string]stepResult{
			"gated_write": {"issue": map[string]any{"id": 1}},
		},
		suspendable: true,
	}
	st, sig, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"window_days": 7}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sig != nil {
		t.Fatalf("run must complete, not suspend: %+v", sig)
	}
	// The gated write was injected, never dispatched.
	if disp.byRef["aileron:tracker.create_issue"] != 0 {
		t.Errorf("memoized gated write dispatched %d times, want 0 (injected)", disp.byRef["aileron:tracker.create_issue"])
	}
	// The downstream read that binds the memoized output still ran.
	if disp.byRef["aileron:metrics.query_series"] != 1 {
		t.Errorf("downstream read dispatched %d times, want 1", disp.byRef["aileron:metrics.query_series"])
	}
	// The injected step emitted no audit rows.
	for _, d := range st.dispatches {
		if d.StepID == "gated_write" {
			t.Error("an injected step must append no dispatch audit row")
		}
	}
	// The materializing render step produced its artifact.
	if len(st.artifacts) != 1 || st.artifacts[0].Name != "out.json" {
		t.Fatalf("want the out.json artifact, got %+v", st.artifacts)
	}
}

// TestExecute_MemoizedMaterializingStepRebuildsArtifact proves a memoized
// materializing step rebuilds its artifact on replay (materialize is pure) but
// appends NO audit-provenance row (no double-audit), so a resumed run's final
// Artifacts is whole without re-emitting the output.materialized record.
func TestExecute_MemoizedMaterializingStepRebuildsArtifact(t *testing.T) {
	p := gatedMidFlowPlan()
	disp := newCountingDispatcher(map[string]map[string]any{})
	x := &executor{
		plan:      p,
		enforcer:  &enforcer{dispatcher: disp, approver: &fakeApprover{decision: Decision{Approved: true}}},
		transform: midFlowRegistry(),
		resumeOutputs: map[string]stepResult{
			"gated_write": {"issue": map[string]any{"id": 1}},
			"read_after":  {"series": []any{}},
			"render":      {"file": map[string]any{"path": "out.json", "mimeType": "application/json", "encoding": "utf-8", "content": `{"ok":true}`}},
		},
		suspendable: true,
	}
	st, sig, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"window_days": 7}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sig != nil {
		t.Fatalf("fully-memoized run must complete, got suspend %+v", sig)
	}
	// The artifact was rebuilt from the memoized output.
	if len(st.artifacts) != 1 || st.artifacts[0].Name != "out.json" {
		t.Fatalf("want the rebuilt out.json artifact, got %+v", st.artifacts)
	}
	// No effect fired (everything injected).
	if len(disp.byRef) != 0 {
		t.Errorf("a fully-memoized replay must dispatch nothing, got %+v", disp.byRef)
	}
	// No output-provenance rows: the record was emitted on the original run.
	if len(st.outputs) != 0 {
		t.Errorf("a memoized materializing step must append no output-provenance row, got %d", len(st.outputs))
	}
}

// TestExecute_UnfulfilledSeamSuspends proves an un-memoized seam with a nil seam
// on the suspendable path yields a seam suspend signal carrying the right StepID
// and SeamRequest, with no error.
func TestExecute_UnfulfilledSeamSuspends(t *testing.T) {
	p := twoSeamPlan()
	disp := newCountingDispatcher(map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{}},
	})
	x := &executor{
		plan:        p,
		enforcer:    &enforcer{dispatcher: disp, approver: &fakeApprover{}},
		transform:   NewTransformRegistry(),
		seam:        nil, // unwired on the suspendable path
		suspendable: true,
	}
	_, sig, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"window_days": 7}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sig == nil {
		t.Fatal("an unfulfilled seam on the suspendable path must suspend")
	}
	if sig.kind != SuspendKindSeam || sig.stepID != "seam_a" {
		t.Errorf("suspend = kind %d step %q, want seam/seam_a", sig.kind, sig.stepID)
	}
	if sig.seam == nil || sig.seam.StepID != "seam_a" {
		t.Errorf("suspend must carry the SeamRequest for seam_a, got %+v", sig.seam)
	}
}

// TestExecute_PendingApprovalSuspends proves an un-memoized gated action with a
// pending approver yields an approval suspend signal and the effect never fires.
func TestExecute_PendingApprovalSuspends(t *testing.T) {
	p := gatedMidFlowPlan()
	disp := newCountingDispatcher(map[string]map[string]any{
		"aileron:tracker.create_issue": {"issue": map[string]any{"id": 1}},
	})
	x := &executor{
		plan:        p,
		enforcer:    &enforcer{dispatcher: disp, approver: &fakeApprover{decision: Decision{Pending: true}}},
		transform:   midFlowRegistry(),
		suspendable: true,
	}
	_, sig, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"window_days": 7}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sig == nil || sig.kind != SuspendKindApproval || sig.stepID != "gated_write" {
		t.Fatalf("want an approval suspend at gated_write, got %+v", sig)
	}
	if disp.byRef["aileron:tracker.create_issue"] != 0 {
		t.Error("a pending approval must not fire the effect")
	}
}

// TestExecute_NonSuspendableNilSeamStillErrors proves the contract is preserved:
// off the suspendable path a nil seam is a hard error, not a suspend.
func TestExecute_NonSuspendableNilSeamStillErrors(t *testing.T) {
	p := twoSeamPlan()
	disp := newCountingDispatcher(map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{}},
	})
	x := &executor{
		plan:      p,
		enforcer:  &enforcer{dispatcher: disp, approver: &fakeApprover{}},
		transform: NewTransformRegistry(),
		seam:      nil,
		// suspendable false (the default).
	}
	_, sig, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"window_days": 7}})
	if err == nil {
		t.Fatal("off the suspendable path a nil seam must be a hard error")
	}
	if sig != nil {
		t.Error("a hard error must not also return a suspend signal")
	}
}

func TestExecute_MultiOutputActionMissingFieldErrors(t *testing.T) {
	p := &Plan{
		Name:    "t",
		Actions: map[string]Action{"aileron:m.read": readAction("aileron:m.read")},
		Outputs: map[string]Output{},
		Steps: []Step{
			{ID: "s", Kind: KindActionCall, ActionRef: "aileron:m.read", Outputs: []string{"a", "b"}},
		},
	}
	p.Order = []int{0}
	x := &executor{
		plan:      p,
		enforcer:  &enforcer{dispatcher: &fakeDispatcher{result: map[string]any{"a": 1}}, approver: &fakeApprover{}},
		transform: NewTransformRegistry(),
	}
	if _, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{}}); err == nil {
		t.Fatal("a multi-output action result missing a declared output must error")
	}
}

func TestExecute_TransformMissingDeclaredOutputErrors(t *testing.T) {
	p := &Plan{
		Name: "t", Actions: map[string]Action{}, Outputs: map[string]Output{},
		Steps: []Step{{ID: "s", Kind: KindTransform, Outputs: []string{"a", "b"}}},
	}
	p.Order = []int{0}
	reg := NewTransformRegistry()
	reg.Register("identity", func(_ map[string]any, _ []string) (map[string]any, error) {
		return map[string]any{"a": 1}, nil // omits "b"
	})
	x := &executor{plan: p, enforcer: &enforcer{}, transform: reg}
	if _, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{}}); err == nil {
		t.Fatal("a transform that omits a declared output must error")
	}
}
