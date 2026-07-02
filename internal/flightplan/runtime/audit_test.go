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
	rec := buildOutputRecord(o, prov, nil, nil)
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
	rec := buildOutputRecord(o, launchProvenance{}, nil, nil)
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

func TestBuildOutputRecord_TransformResolvesUpstreamActorAndInputs(t *testing.T) {
	// The illustrative record (#1753): a transform-materialized output
	// (render_dashboard) carries the actor identity resolved from its upstream
	// query action-call, a step.inputs walk-back with content_hash and a
	// query_execution_id for the query input, resolved_inputs limited to
	// launch config (literal/dynamic), and the consent decision.
	prov := launchProvenance{
		Skill: "weekly-metrics-digest", ContentHash: "sha256:plan", SignedBy: "sha256:key",
		SignatureStatus: "verified", InvocationID: "inv-123",
	}
	// The upstream query dispatch carries the actor provenance the daemon
	// surfaced (a single-connector, single-identity read).
	queryDispatch := actionDispatch{
		StepID:            "query_metrics",
		ActionRef:         "aileron:athena.query",
		IdentityLabel:     "work",
		CredentialBinding: "aws_sigv4/athena/work",
		ConnectorVersion:  "2.3.1",
		ConnectorHash:     "sha256:conn",
		ConsentDecision:   "unattended",
	}
	dispatchByStep := map[string]actionDispatch{"query_metrics": queryDispatch}

	// The query result the transform bound: an Athena-style object carrying a
	// QueryExecutionId, which the record lifts (not synthesizes).
	rowsValue := map[string]any{
		"QueryExecutionId": "qeid-123",
		"ResultSet":        []any{map[string]any{"name": "cpu"}},
	}
	o := materializedOutput{
		StepID:    "render_dashboard",
		StepKind:  KindTransform,
		Transform: "render",
		Artifact:  Artifact{Name: "dashboard.html", MimeType: "text/html", Content: []byte("<html/>"), Digest: "sha256:out"},
		Binds: map[string]Binding{
			"rows": {Kind: BindStep, Raw: "steps.query_metrics.rows", StepID: "query_metrics", Output: "rows"},
		},
		Resolved: map[string]any{"rows": rowsValue},
	}
	resolvedInputs := map[string]any{"window_days": 7}

	rec := buildOutputRecord(o, prov, dispatchByStep, resolvedInputs)
	f := rec.Fields

	// Actor resolved from the upstream query dispatch.
	if f["aileron.actor.identity_label"] != "work" {
		t.Errorf("actor.identity_label = %v, want work (from upstream query)", f["aileron.actor.identity_label"])
	}
	if f["aileron.actor.credential_binding"] != "aws_sigv4/athena/work" {
		t.Errorf("actor.credential_binding = %v", f["aileron.actor.credential_binding"])
	}
	if f["aileron.actor.connector_version"] != "2.3.1" || f["aileron.actor.connector_hash"] != "sha256:conn" {
		t.Errorf("actor connector build = %v/%v", f["aileron.actor.connector_version"], f["aileron.actor.connector_hash"])
	}
	if f["aileron.consent.decision"] != "unattended" {
		t.Errorf("consent.decision = %v, want unattended", f["aileron.consent.decision"])
	}

	// step.inputs: one entry, binding+source+content_hash+query_execution_id.
	inputs, ok := f["aileron.step.inputs"].([]map[string]any)
	if !ok || len(inputs) != 1 {
		t.Fatalf("step.inputs = %v, want one entry", f["aileron.step.inputs"])
	}
	entry := inputs[0]
	if entry["binding"] != "rows" || entry["source"] != "steps.query_metrics.rows" {
		t.Errorf("step.inputs[0] binding/source = %v/%v", entry["binding"], entry["source"])
	}
	wantHash, err := canonicalValueDigest(rowsValue)
	if err != nil {
		t.Fatalf("canonicalValueDigest: %v", err)
	}
	if entry["content_hash"] != wantHash {
		t.Errorf("step.inputs[0] content_hash = %v, want %v", entry["content_hash"], wantHash)
	}
	if entry["query_execution_id"] != "qeid-123" {
		t.Errorf("step.inputs[0] query_execution_id = %v, want qeid-123", entry["query_execution_id"])
	}

	// resolved_inputs carries only the launch config.
	ri, ok := f["aileron.resolved_inputs"].(map[string]any)
	if !ok || ri["window_days"] != 7 {
		t.Errorf("resolved_inputs = %v, want {window_days:7}", f["aileron.resolved_inputs"])
	}

	// The full walk-back chain is present on one record: output digest →
	// producing step → input content_hash/query_execution_id → connector
	// version+hash+identity → plan content_hash + signer.
	if f["aileron.output.content_hash"] != "sha256:out" || f["aileron.step.id"] != "render_dashboard" ||
		f["aileron.plan.content_hash"] != "sha256:plan" || f["aileron.plan.signed_by"] != "sha256:key" {
		t.Errorf("walk-back chain incomplete: %v", f)
	}
}

func TestBuildStepInputs_ContentHashReproducibleAndNonQueryOmitsQEID(t *testing.T) {
	// The input-binding content_hash is reproducible: the same bound value
	// across two independently-built outputs yields the identical sha256
	// digest. A non-query input (a template literal) omits query_execution_id.
	build := func() materializedOutput {
		return materializedOutput{
			StepID:   "render",
			StepKind: KindTransform,
			Binds: map[string]Binding{
				"rows":     {Kind: BindStep, Raw: "steps.q.rows", StepID: "q", Output: "rows"},
				"template": {Kind: BindInput, Raw: "inputs.template", Name: "template"},
			},
			Resolved: map[string]any{
				"rows":     map[string]any{"QueryExecutionId": "qeid-9", "ResultSet": []any{1, 2, 3}},
				"template": "a literal template string",
			},
		}
	}
	first := buildStepInputs(build())
	second := buildStepInputs(build())

	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("want 2 input entries each, got %d/%d", len(first), len(second))
	}
	// Sorted by binding name: "rows" before "template".
	if first[0]["binding"] != "rows" || first[1]["binding"] != "template" {
		t.Fatalf("entries not sorted by binding: %v", first)
	}
	if first[0]["content_hash"] != second[0]["content_hash"] {
		t.Errorf("rows content_hash not reproducible: %v vs %v", first[0]["content_hash"], second[0]["content_hash"])
	}
	if first[1]["content_hash"] != second[1]["content_hash"] {
		t.Errorf("template content_hash not reproducible")
	}
	// The query input lifts a query_execution_id; the template literal does not.
	if first[0]["query_execution_id"] != "qeid-9" {
		t.Errorf("rows query_execution_id = %v, want qeid-9", first[0]["query_execution_id"])
	}
	if _, present := first[1]["query_execution_id"]; present {
		t.Error("a non-query template input must omit query_execution_id, not guess one")
	}
}

func TestResolveActor_DivergentUpstreamsOmitActor(t *testing.T) {
	// A transform whose data-producing upstreams disagree on identity has no
	// single truthful actor, so the actor keys are omitted — but the record
	// still carries step.inputs (the walk-back to identity resolves there).
	o := materializedOutput{
		StepID:   "merge_dashboard",
		StepKind: KindTransform,
		Artifact: Artifact{Name: "merged.html", Content: []byte("x"), Digest: "sha256:m"},
		Binds: map[string]Binding{
			"a": {Kind: BindStep, Raw: "steps.qa.rows", StepID: "qa", Output: "rows"},
			"b": {Kind: BindStep, Raw: "steps.qb.rows", StepID: "qb", Output: "rows"},
		},
		Resolved: map[string]any{"a": map[string]any{"x": 1}, "b": map[string]any{"y": 2}},
	}
	dispatchByStep := map[string]actionDispatch{
		"qa": {StepID: "qa", IdentityLabel: "work", CredentialBinding: "b/work"},
		"qb": {StepID: "qb", IdentityLabel: "personal", CredentialBinding: "b/personal"},
	}
	rec := buildOutputRecord(o, launchProvenance{}, dispatchByStep, map[string]any{})
	if _, present := rec.Fields["aileron.actor.identity_label"]; present {
		t.Error("divergent-identity transform must omit actor.identity_label")
	}
	if _, present := rec.Fields["aileron.actor.credential_binding"]; present {
		t.Error("divergent-identity transform must omit actor.credential_binding")
	}
	// step.inputs still present for the walk-back.
	if _, present := rec.Fields["aileron.step.inputs"]; !present {
		t.Error("step.inputs must still be recorded even when the actor is omitted")
	}
}

func TestResolveActor_TransformOmitsActorWhenAnUpstreamIsUnattributable(t *testing.T) {
	// A transform that binds one attributable action-call AND one upstream with
	// no dispatch (e.g. an intermediate transform's output) has no single
	// truthful actor across ALL its data inputs, so the actor keys are omitted
	// — not attributed solely to the one identified upstream. step.inputs still
	// records both bindings for the walk-back.
	o := materializedOutput{
		StepID:   "compose_dashboard",
		StepKind: KindTransform,
		Artifact: Artifact{Name: "dash.html", Content: []byte("x"), Digest: "sha256:c"},
		Binds: map[string]Binding{
			"rows":    {Kind: BindStep, Raw: "steps.query.rows", StepID: "query", Output: "rows"},
			"summary": {Kind: BindStep, Raw: "steps.reshape.summary", StepID: "reshape", Output: "summary"},
		},
		Resolved: map[string]any{"rows": map[string]any{"x": 1}, "summary": map[string]any{"y": 2}},
	}
	dispatchByStep := map[string]actionDispatch{
		// Only the query step has a dispatch; "reshape" is a transform with none.
		"query": {StepID: "query", IdentityLabel: "work", CredentialBinding: "aws_sigv4/athena/work",
			ConnectorVersion: "2.3.1", ConnectorHash: "sha256:conn"},
	}
	rec := buildOutputRecord(o, launchProvenance{}, dispatchByStep, map[string]any{})
	if _, present := rec.Fields["aileron.actor.identity_label"]; present {
		t.Error("a transform with an unattributable upstream must omit the actor identity, not attribute solely to the identified one")
	}
	inputs, ok := rec.Fields["aileron.step.inputs"].([]map[string]any)
	if !ok || len(inputs) != 2 {
		t.Errorf("step.inputs must still record both bindings, got %v", rec.Fields["aileron.step.inputs"])
	}
}

func TestBuildOutputRecord_ResolvedInputsExcludeSourceDatasets(t *testing.T) {
	// resolved_inputs carries launch config (literal/dynamic) only; a
	// source-resolved dataset is referenced by binding elsewhere and never
	// inlined here (ADR-0027 audit boundary).
	st := execState{
		inputs: ResolvedInputs{
			Values: map[string]any{
				"window_days": 7,
				"dataset":     []any{map[string]any{"secret": "value"}},
			},
			SourceBindings: map[string]SourceBinding{
				"dataset": {ActionRef: "aileron:athena.query", Select: "ResultSet"},
			},
		},
		outputs: []materializedOutput{
			{StepID: "render", StepKind: KindTransform, Transform: "render",
				Artifact: Artifact{Name: "d.html", Content: []byte("x"), Digest: "sha256:1"}},
		},
	}
	sink := &recordingSink{}
	emitAudit(context.Background(), sink, st, launchProvenance{InvocationID: "inv"})

	var outRec *AuditRecord
	for i := range sink.records {
		if sink.records[i].Kind == RecordKindOutput {
			outRec = &sink.records[i]
			break
		}
	}
	if outRec == nil {
		t.Fatal("no output record emitted")
	}
	ri, ok := outRec.Fields["aileron.resolved_inputs"].(map[string]any)
	if !ok {
		t.Fatalf("resolved_inputs = %T, want map", outRec.Fields["aileron.resolved_inputs"])
	}
	if ri["window_days"] != 7 {
		t.Errorf("resolved_inputs must carry the literal window_days, got %v", ri)
	}
	if _, present := ri["dataset"]; present {
		t.Error("resolved_inputs must NOT inline a source-resolved dataset")
	}
	if containsValue(ri, "value") {
		t.Error("the source dataset's values must not appear in resolved_inputs")
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

// --- Unit 3 (#1784): declared-reach record build + emit ---

// TestBuildReachRecord_CarriesDeclaredReachAndNotEnforcedMarker proves
// buildReachRecord produces a RecordKindReach record whose flat aileron.* fields
// carry the step id, effect, hosts, and the fixed `aileron.reach.enforced: false`
// marker that states the declaration is audit-only.
func TestBuildReachRecord_CarriesDeclaredReachAndNotEnforcedMarker(t *testing.T) {
	rec := buildReachRecord(reachRecord{
		StepID: "extract",
		Effect: EffectExternalSend,
		Hosts:  []string{"api.example.com", "cdn.example.com"},
	})
	if rec.Kind != RecordKindReach {
		t.Fatalf("kind = %v, want RecordKindReach", rec.Kind)
	}
	if rec.Fields["aileron.step.id"] != "extract" {
		t.Errorf("step id = %v, want extract", rec.Fields["aileron.step.id"])
	}
	if rec.Fields["aileron.reach.effect"] != "external-send" {
		t.Errorf("effect = %v, want external-send", rec.Fields["aileron.reach.effect"])
	}
	hosts, ok := rec.Fields["aileron.reach.hosts"].([]string)
	if !ok || len(hosts) != 2 || hosts[0] != "api.example.com" || hosts[1] != "cdn.example.com" {
		t.Errorf("hosts = %v, want the two declared hosts", rec.Fields["aileron.reach.hosts"])
	}
	enforced, ok := rec.Fields["aileron.reach.enforced"].(bool)
	if !ok || enforced {
		t.Errorf("enforced = %v, want literal false (the not-enforced marker)", rec.Fields["aileron.reach.enforced"])
	}
}

// TestBuildReachRecord_EmptyHostsStillEnforcedFalse proves the enforced marker is
// always present and false even for a contract with no declared hosts.
func TestBuildReachRecord_EmptyHostsStillEnforcedFalse(t *testing.T) {
	rec := buildReachRecord(reachRecord{StepID: "s", Effect: EffectRead, Hosts: nil})
	if v, ok := rec.Fields["aileron.reach.enforced"].(bool); !ok || v {
		t.Errorf("enforced = %v, want false", rec.Fields["aileron.reach.enforced"])
	}
	if rec.Fields["aileron.reach.effect"] != "read" {
		t.Errorf("effect = %v, want read", rec.Fields["aileron.reach.effect"])
	}
}

// TestEmitAudit_OneReachRecordPerReachEntry proves emitAudit emits exactly one
// RecordKindReach per st.reaches entry, in order, and returns one id per record
// including the dispatch, output, and launch records.
func TestEmitAudit_OneReachRecordPerReachEntry(t *testing.T) {
	st := execState{
		inputs: ResolvedInputs{SourceBindings: map[string]SourceBinding{}},
		dispatches: []actionDispatch{
			{StepID: "call", ActionRef: "aileron:m.read", Effect: EffectRead},
		},
		reaches: []reachRecord{
			{StepID: "extract", Effect: EffectExternalSend, Hosts: []string{"a.example.com"}},
			{StepID: "publish", Effect: EffectRead, Hosts: []string{"b.example.com"}},
		},
		outputs: []materializedOutput{
			{StepID: "render", StepKind: KindTransform, Transform: "html-render",
				Artifact: Artifact{Name: "d.csv", Content: []byte("x"), Digest: "sha256:1"}},
		},
	}
	sink := &recordingSink{}
	ids := emitAudit(context.Background(), sink, st, launchProvenance{InvocationID: "inv"})

	var reachRecords, actionRecords, outputRecords, launchRecords int
	var reachStepOrder []string
	for _, r := range sink.records {
		switch r.Kind {
		case RecordKindReach:
			reachRecords++
			reachStepOrder = append(reachStepOrder, r.Fields["aileron.step.id"].(string))
		case RecordKindAction:
			actionRecords++
		case RecordKindOutput:
			outputRecords++
		case RecordKindLaunch:
			launchRecords++
		}
	}
	if reachRecords != 2 {
		t.Errorf("reach records = %d, want 2 (one per st.reaches entry)", reachRecords)
	}
	if len(reachStepOrder) != 2 || reachStepOrder[0] != "extract" || reachStepOrder[1] != "publish" {
		t.Errorf("reach records out of order: %v, want [extract publish]", reachStepOrder)
	}
	if actionRecords != 1 || outputRecords != 1 || launchRecords != 1 {
		t.Errorf("other records action=%d output=%d launch=%d, want 1/1/1", actionRecords, outputRecords, launchRecords)
	}
	if len(ids) != len(sink.records) {
		t.Errorf("emitAudit returned %d ids for %d records", len(ids), len(sink.records))
	}
}

// TestEmitAudit_NoReachesEmitsNoReachRecords proves an execState with no reaches
// emits no reach records (only the existing dispatch/output/launch records).
func TestEmitAudit_NoReachesEmitsNoReachRecords(t *testing.T) {
	st := execState{
		inputs:  ResolvedInputs{SourceBindings: map[string]SourceBinding{}},
		outputs: []materializedOutput{{StepID: "render", StepKind: KindTransform, Artifact: Artifact{Digest: "sha256:1"}}},
	}
	sink := &recordingSink{}
	emitAudit(context.Background(), sink, st, launchProvenance{})
	for _, r := range sink.records {
		if r.Kind == RecordKindReach {
			t.Fatalf("emitted a reach record for a plan with no reaches")
		}
	}
}

// TestEmitAudit_NilSinkEmitsNothingWithReaches proves the nil-sink short-circuit
// covers reach: a nil sink emits nothing and returns no ids even when reaches
// are present.
func TestEmitAudit_NilSinkEmitsNothingWithReaches(t *testing.T) {
	st := execState{reaches: []reachRecord{{StepID: "extract", Effect: EffectRead}}}
	if ids := emitAudit(context.Background(), nil, st, launchProvenance{}); ids != nil {
		t.Errorf("nil sink returned ids = %v, want nil", ids)
	}
}
