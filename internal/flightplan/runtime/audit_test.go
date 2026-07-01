package runtime

import "testing"

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

func TestBuildLaunchRecord_BindsOutputToDigest(t *testing.T) {
	// The per-launch record ties each materialized output name to its content
	// digest and path, a reference (no inline bytes) inside the ADR-0027 audit
	// boundary. This is what makes a past launch independently verifiable.
	st := execState{
		inputs: ResolvedInputs{SourceBindings: map[string]SourceBinding{}},
		artifacts: []Artifact{
			{Name: "chains.json", Path: "chains.json", Content: []byte(`{"a":1}`), Digest: "sha256:deadbeef", Written: true},
			{Name: "kept.json", Content: []byte(`{}`), Digest: "sha256:cafe", Written: false},
		},
	}
	rec := buildLaunchRecord(st)
	outs, ok := rec.Fields["materializedOutputs"].([]map[string]any)
	if !ok {
		t.Fatalf("materializedOutputs = %T, want []map[string]any", rec.Fields["materializedOutputs"])
	}
	if len(outs) != 2 {
		t.Fatalf("want 2 recorded outputs, got %d", len(outs))
	}
	if outs[0]["name"] != "chains.json" || outs[0]["path"] != "chains.json" || outs[0]["sha256"] != "sha256:deadbeef" {
		t.Errorf("output 0 = %v", outs[0])
	}
	// The retained (unwritten) output is still recorded by name + digest.
	if outs[1]["name"] != "kept.json" || outs[1]["sha256"] != "sha256:cafe" {
		t.Errorf("output 1 = %v", outs[1])
	}
	// The record references each output by name/path/sha256 only, never the
	// dataset bytes inline (ADR-0027 boundary).
	for _, o := range outs {
		if len(o) != 3 {
			t.Errorf("recorded output must carry only name/path/sha256, got %v", o)
		}
		if _, hasContent := o["content"]; hasContent {
			t.Error("the launch record must not carry artifact content inline")
		}
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
