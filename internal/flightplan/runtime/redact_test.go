package runtime

import (
	"reflect"
	"testing"
)

func TestApplyRedaction_Drop(t *testing.T) {
	in := map[string]any{"keep": "yes", "secret": "drop-me"}
	out := applyRedaction(in, []RedactionRule{{Field: "secret", Rule: RedactDrop}})
	if _, present := out["secret"]; present {
		t.Error("drop must remove the field")
	}
	if out["keep"] != "yes" {
		t.Error("drop must leave other fields")
	}
	// Original untouched.
	if _, present := in["secret"]; !present {
		t.Error("redaction must not mutate the original result")
	}
}

func TestApplyRedaction_Mask(t *testing.T) {
	out := applyRedaction(map[string]any{"assignee": map[string]any{"email": "a@b.com"}},
		[]RedactionRule{{Field: "assignee.email", Rule: RedactMask}})
	email := out["assignee"].(map[string]any)["email"]
	if email != "***" {
		t.Errorf("mask = %v, want ***", email)
	}
}

func TestApplyRedaction_HashStable(t *testing.T) {
	rules := []RedactionRule{{Field: "id", Rule: RedactHash}}
	a := applyRedaction(map[string]any{"id": "abc"}, rules)
	b := applyRedaction(map[string]any{"id": "abc"}, rules)
	if a["id"] != b["id"] {
		t.Error("hash must be stable for the same input")
	}
	if a["id"] == "abc" {
		t.Error("hash must replace the value")
	}
}

func TestApplyRedaction_ArrayWildcard(t *testing.T) {
	in := map[string]any{
		"series": []any{
			map[string]any{"labels": map[string]any{"account_id": "111"}},
			map[string]any{"labels": map[string]any{"account_id": "222"}},
		},
	}
	out := applyRedaction(in, []RedactionRule{{Field: "series[].labels.account_id", Rule: RedactHash}})
	arr := out["series"].([]any)
	for _, el := range arr {
		got := el.(map[string]any)["labels"].(map[string]any)["account_id"]
		if got == "111" || got == "222" {
			t.Errorf("array element account_id not redacted: %v", got)
		}
	}
}

func TestApplyRedaction_MissingFieldIsNoOp(t *testing.T) {
	in := map[string]any{"a": 1}
	out := applyRedaction(in, []RedactionRule{{Field: "nope.deeper", Rule: RedactDrop}})
	if !reflect.DeepEqual(out, map[string]any{"a": 1}) {
		t.Errorf("a non-matching path must be a no-op, got %v", out)
	}
}
