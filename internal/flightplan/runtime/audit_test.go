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
