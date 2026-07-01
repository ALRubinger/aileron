package runtime

import (
	"context"
	"testing"
)

func TestBuildActionRecord_OnlyDeclaredFields(t *testing.T) {
	d := actionDispatch{
		ActionRef:         "aileron:m.read",
		Effect:            EffectRead,
		ApprovalRequested: false,
		Approved:          true,
		Result:            map[string]any{"series": []any{1}, "meta": "x"},
		AuditFields:       []string{"operation-effect", "result"},
		Sink:              "audit/reads",
	}
	rec := buildActionRecord(d)
	if rec.Sink != "audit/reads" {
		t.Errorf("sink = %q", rec.Sink)
	}
	// Exactly the declared fields, no more.
	if len(rec.Fields) != 2 {
		t.Fatalf("want 2 declared fields, got %d: %v", len(rec.Fields), rec.Fields)
	}
	if rec.Fields["operation-effect"] != "read" {
		t.Errorf("operation-effect = %v", rec.Fields["operation-effect"])
	}
	// A field the runtime cannot populate (credential-binding) is omitted, not guessed.
	if _, present := rec.Fields["credential-binding"]; present {
		t.Error("an undeclared field must not appear")
	}
}

func TestAuditResult_ReferencesShapeNotData(t *testing.T) {
	d := actionDispatch{
		Effect:      EffectRead,
		Result:      map[string]any{"series": []any{map[string]any{"secret": "value"}}},
		AuditFields: []string{"result"},
	}
	rec := buildActionRecord(d)
	summary, ok := rec.Fields["result"].(map[string]any)
	if !ok {
		t.Fatalf("result field = %T", rec.Fields["result"])
	}
	// The summary records the field shape (keys), never the dataset values.
	fields, ok := summary["fields"].([]string)
	if !ok || len(fields) != 1 || fields[0] != "series" {
		t.Errorf("result summary must reference top-level keys, got %v", summary)
	}
	// The secret value must NOT appear anywhere in the summary.
	if containsValue(summary, "value") {
		t.Error("the audit result summary must not carry the dataset inline")
	}
}

func TestApprovalDecisionAudit(t *testing.T) {
	cases := []struct {
		name      string
		requested bool
		approved  bool
		want      string
	}{
		{"unattended read", false, true, "unattended"},
		{"approved write", true, true, "approved"},
		{"denied write", true, false, "denied"},
	}
	for _, c := range cases {
		d := actionDispatch{ApprovalRequested: c.requested, Approved: c.approved, AuditFields: []string{"approval-decision"}}
		rec := buildActionRecord(d)
		if rec.Fields["approval-decision"] != c.want {
			t.Errorf("%s: approval-decision = %v, want %q", c.name, rec.Fields["approval-decision"], c.want)
		}
	}
}

func TestBuildActionRecord_KindIsAction(t *testing.T) {
	// The action record carries the explicit action kind so the CLI sink maps it
	// to flightplan.launch.action without re-inferring from ActionRef.
	rec := buildActionRecord(actionDispatch{ActionRef: "aileron:m.read", Effect: EffectRead})
	if rec.Kind != RecordKindAction {
		t.Errorf("action record Kind = %v, want RecordKindAction", rec.Kind)
	}
}

func TestBuildLaunchRecord_KeepsSourcesDropsOutputsArray(t *testing.T) {
	// The per-launch summary now keeps only the resolved input source bindings;
	// per-materialized-output provenance moved to individual output.materialized
	// records (#1752), so the summary no longer lumps outputs into an array.
	st := execState{
		inputs: ResolvedInputs{SourceBindings: map[string]SourceBinding{
			"series": {ActionRef: "aileron:metrics.query_series", Select: "series"},
		}},
		artifacts: []Artifact{
			{Name: "chains.json", Path: "chains.json", Content: []byte(`{"a":1}`), Digest: "sha256:deadbeef", Written: true},
		},
	}
	rec := buildLaunchRecord(st)
	if rec.Kind != RecordKindLaunch {
		t.Errorf("launch record Kind = %v, want RecordKindLaunch", rec.Kind)
	}
	if _, present := rec.Fields["materializedOutputs"]; present {
		t.Error("launch summary must no longer lump outputs into materializedOutputs")
	}
	sources, ok := rec.Fields["sourceInputBindings"].(map[string]any)
	if !ok {
		t.Fatalf("sourceInputBindings = %T, want map[string]any", rec.Fields["sourceInputBindings"])
	}
	sb, ok := sources["series"].(map[string]any)
	if !ok || sb["actionRef"] != "aileron:metrics.query_series" || sb["select"] != "series" {
		t.Errorf("sourceInputBindings[series] = %v", sources["series"])
	}
}

func TestBuildOutputRecord_TransformCarriesFullProvenance(t *testing.T) {
	// A transform-materialized output surfaces as its own event with the step's
	// kind/transform, the content hash equal to Artifact.Digest verbatim, the
	// byte count, and the full plan/invocation identity.
	prov := launchProvenance{
		Skill: "weekly-metrics-digest", ContentHash: "sha256:plan", SignedBy: "sha256:key",
		SignatureStatus: "verified", InvocationID: "inv-123",
	}
	content := []byte("name\ncpu\n")
	o := materializedOutput{
		StepID:    "render_csv",
		StepKind:  KindTransform,
		Transform: "html-render",
		Artifact:  Artifact{Name: "digest.csv", Path: "digest.csv", MimeType: "text/csv", Content: content, Digest: "sha256:abc"},
	}
	rec := buildOutputRecord(o, prov)
	if rec.Kind != RecordKindOutput {
		t.Fatalf("Kind = %v, want RecordKindOutput", rec.Kind)
	}
	f := rec.Fields
	if f["aileron.output.name"] != "digest.csv" || f["aileron.output.mime"] != "text/csv" {
		t.Errorf("output identity fields = %v", f)
	}
	if f["aileron.output.path"] != "digest.csv" {
		t.Errorf("output.path = %v, want the artifact's on-disk path", f["aileron.output.path"])
	}
	if f["aileron.output.content_hash"] != "sha256:abc" {
		t.Errorf("content_hash = %v, want the Artifact.Digest verbatim", f["aileron.output.content_hash"])
	}
	if f["aileron.output.bytes"] != len(content) {
		t.Errorf("bytes = %v, want %d", f["aileron.output.bytes"], len(content))
	}
	if f["aileron.step.id"] != "render_csv" || f["aileron.step.kind"] != "transform" {
		t.Errorf("step provenance = %v", f)
	}
	if f["aileron.step.transform"] != "html-render" {
		t.Errorf("step.transform = %v, want html-render", f["aileron.step.transform"])
	}
	if f["aileron.plan.skill"] != "weekly-metrics-digest" || f["aileron.plan.content_hash"] != "sha256:plan" ||
		f["aileron.plan.signed_by"] != "sha256:key" || f["aileron.plan.signature_status"] != "verified" ||
		f["aileron.invocation.id"] != "inv-123" {
		t.Errorf("plan/invocation provenance = %v", f)
	}
}

func TestBuildOutputRecord_ActionCallOmitsTransform(t *testing.T) {
	// An action-call materialized output carries step.kind=action-call and does
	// NOT carry a step.transform key (there is no transform for an action-call).
	o := materializedOutput{
		StepID:   "file_issue",
		StepKind: KindActionCall,
		Artifact: Artifact{Name: "filed_issue.json", MimeType: "application/json", Content: []byte("{}"), Digest: "sha256:d"},
	}
	rec := buildOutputRecord(o, launchProvenance{})
	if rec.Fields["aileron.step.kind"] != "action-call" {
		t.Errorf("step.kind = %v, want action-call", rec.Fields["aileron.step.kind"])
	}
	if _, present := rec.Fields["aileron.step.transform"]; present {
		t.Error("an action-call output record must not carry aileron.step.transform")
	}
}

func TestEmitAudit_OneOutputRecordPerArtifact(t *testing.T) {
	// emitAudit emits exactly one RecordKindOutput per st.outputs entry, then the
	// launch summary. This includes a transform-only materializing output with no
	// action dispatch present (the transform case the issue targets).
	st := execState{
		inputs: ResolvedInputs{SourceBindings: map[string]SourceBinding{}},
		outputs: []materializedOutput{
			{StepID: "render_csv", StepKind: KindTransform, Transform: "html-render",
				Artifact: Artifact{Name: "digest.csv", Content: []byte("x"), Digest: "sha256:1"}},
		},
	}
	sink := &recordingSink{}
	ids := emitAudit(context.Background(), sink, st, launchProvenance{InvocationID: "inv"})

	var outputRecords, launchRecords int
	for _, r := range sink.records {
		switch r.Kind {
		case RecordKindOutput:
			outputRecords++
		case RecordKindLaunch:
			launchRecords++
		}
	}
	if outputRecords != 1 {
		t.Errorf("output records = %d, want exactly 1 (one per materialized artifact)", outputRecords)
	}
	if launchRecords != 1 {
		t.Errorf("launch summary records = %d, want exactly 1", launchRecords)
	}
	if len(ids) != len(sink.records) {
		t.Errorf("emitAudit returned %d ids for %d records", len(ids), len(sink.records))
	}
}

func TestEmitAudit_NilSinkEmitsNothing(t *testing.T) {
	// A nil sink emits nothing and returns no ids (the audit is a companion, not
	// a hard dependency).
	st := execState{outputs: []materializedOutput{{StepID: "s", StepKind: KindTransform}}}
	if ids := emitAudit(context.Background(), nil, st, launchProvenance{}); ids != nil {
		t.Errorf("nil sink returned ids = %v, want nil", ids)
	}
}

// containsValue deep-searches a JSON-shaped value for a string leaf.
func containsValue(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return t == want
	case map[string]any:
		for _, child := range t {
			if containsValue(child, want) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if containsValue(child, want) {
				return true
			}
		}
	case []string:
		for _, child := range t {
			if child == want {
				return true
			}
		}
	}
	return false
}
