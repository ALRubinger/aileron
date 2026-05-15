package action

import (
	"reflect"
	"testing"
)

// TestBuildInputFields_HappyPath pins the contract: declared inputs
// render in declared order, label falls back to the bare name, the
// multiline hint round-trips, and scalar values are stringified
// without extra quoting.
func TestBuildInputFields_HappyPath(t *testing.T) {
	m := &Manifest{
		Inputs: []Input{
			{Name: "to", Type: "string", Description: "Recipients", Label: "To"},
			{Name: "subject", Type: "string", Description: "Subject"},
			{Name: "body", Type: "string", Description: "Body", Label: "Body", Multiline: true},
		},
	}
	args := map[string]any{
		"to":      "alr@example.com",
		"subject": "Hello",
		"body":    "Line one\nLine two",
	}
	got := BuildInputFields(m, args)
	want := []InputField{
		{Label: "To", Value: "alr@example.com"},
		{Label: "subject", Value: "Hello"},
		{Label: "Body", Value: "Line one\nLine two", Multiline: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildInputFields() =\n  %#v\nwant\n  %#v", got, want)
	}
}

// TestBuildInputFields_MultilineValuePreservesNewlines is the
// behavioral test for the user's complaint: a string with embedded
// newlines must round-trip through the projection verbatim so the
// renderer (with whitespace-pre-wrap) draws them as real newlines.
// A regression where the projection JSON-encoded or escape-sequenced
// the value would surface here.
func TestBuildInputFields_MultilineValuePreservesNewlines(t *testing.T) {
	m := &Manifest{
		Inputs: []Input{{Name: "body", Type: "string", Description: "Body", Multiline: true}},
	}
	got := BuildInputFields(m, map[string]any{"body": "first\nsecond\n\nfourth"})
	if len(got) != 1 {
		t.Fatalf("got %d fields, want 1", len(got))
	}
	if got[0].Value != "first\nsecond\n\nfourth" {
		t.Errorf("Value = %q, want literal-newline string", got[0].Value)
	}
}

// TestBuildInputFields_MissingRequiredInput pins the contract for
// args the agent failed to supply for a required input: surfaced as
// Missing=true with empty value, matching the preview convention.
func TestBuildInputFields_MissingRequiredInput(t *testing.T) {
	m := &Manifest{
		Inputs: []Input{
			{Name: "to", Type: "string", Description: "Recipients"},
			{Name: "subject", Type: "string", Description: "Subject"},
		},
	}
	got := BuildInputFields(m, map[string]any{"to": "alr@example.com"})
	want := []InputField{
		{Label: "to", Value: "alr@example.com"},
		{Label: "subject", Missing: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildInputFields() =\n  %#v\nwant\n  %#v", got, want)
	}
}

// TestBuildInputFields_OmitsMissingOptionalInputs documents that
// optional inputs the agent did not supply are dropped entirely —
// there's no useful "n/a" affordance for "the agent chose not to
// pass this optional field", and rendering them as missing would
// clutter the approval surface for every optional input.
func TestBuildInputFields_OmitsMissingOptionalInputs(t *testing.T) {
	f := false
	m := &Manifest{
		Inputs: []Input{
			{Name: "to", Type: "string", Description: "Recipients"},
			{Name: "cc", Type: "string", Required: &f, Description: "Cc"},
		},
	}
	got := BuildInputFields(m, map[string]any{"to": "alr@example.com"})
	if len(got) != 1 || got[0].Label != "to" {
		t.Errorf("BuildInputFields() = %#v, want only the `to` field", got)
	}
}

// TestBuildInputFields_AppendsUndeclaredArgs pins the "no silent
// dropping" rule: args the agent passed that are not declared on the
// manifest are appended at the end (sorted) so the user can see
// everything that would be sent.
func TestBuildInputFields_AppendsUndeclaredArgs(t *testing.T) {
	m := &Manifest{
		Inputs: []Input{{Name: "to", Type: "string", Description: "Recipients"}},
	}
	got := BuildInputFields(m, map[string]any{
		"to":      "alr@example.com",
		"zeta":    "last",
		"alpha":   "first extra",
		"omitted": nil, // nil values are treated as missing — extras-with-nil are dropped
	})
	want := []InputField{
		{Label: "to", Value: "alr@example.com"},
		{Label: "alpha", Value: "first extra"},
		{Label: "zeta", Value: "last"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildInputFields() =\n  %#v\nwant\n  %#v", got, want)
	}
}

// TestBuildInputFields_StringifyScalars confirms scalar types render
// without surprise: integers don't pick up a trailing ".0", booleans
// render as "true"/"false", and floats round-trip through %g.
func TestBuildInputFields_StringifyScalars(t *testing.T) {
	m := &Manifest{
		Inputs: []Input{
			{Name: "max_results", Type: "integer", Description: "max"},
			{Name: "ratio", Type: "number", Description: "ratio"},
			{Name: "enabled", Type: "boolean", Description: "flag"},
		},
	}
	args := map[string]any{
		// JSON decodes "10" as float64(10) — pin the integer-clean rendering.
		"max_results": float64(10),
		"ratio":       1.5,
		"enabled":     true,
	}
	got := BuildInputFields(m, args)
	want := []InputField{
		{Label: "max_results", Value: "10"},
		{Label: "ratio", Value: "1.5"},
		{Label: "enabled", Value: "true"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildInputFields() =\n  %#v\nwant\n  %#v", got, want)
	}
}

// TestBuildInputFields_StructuredValuesRenderAsJSONMultiline pins the
// contract for `array` and `object` input types: the agent's payload
// (decoded into Go native []any / map[string]any) renders as a
// pretty-printed JSON block, and the projected field is marked
// Multiline so the approval surface routes it through the scrollable
// affordance rather than a single-line row. The user must be able to
// read the structured payload they are about to approve.
func TestBuildInputFields_StructuredValuesRenderAsJSONMultiline(t *testing.T) {
	m := &Manifest{
		Inputs: []Input{
			{Name: "requests", Type: "array", Description: "batchUpdate requests"},
			{Name: "options", Type: "object", Description: "config options"},
		},
	}
	args := map[string]any{
		"requests": []any{
			map[string]any{"insertText": map[string]any{"text": "hello"}},
		},
		"options": map[string]any{"dryRun": true},
	}
	got := BuildInputFields(m, args)
	if len(got) != 2 {
		t.Fatalf("got %d fields, want 2", len(got))
	}
	if !got[0].Multiline {
		t.Errorf("requests Multiline = false, want true for array type")
	}
	if !got[1].Multiline {
		t.Errorf("options Multiline = false, want true for object type")
	}
	// Spot-check that the rendered value is valid JSON, not Go's default
	// map[...]interface{}{} formatting.
	if got[0].Value == "" || got[0].Value[0] != '[' {
		t.Errorf("requests Value = %q, want a JSON array literal", got[0].Value)
	}
	if got[1].Value == "" || got[1].Value[0] != '{' {
		t.Errorf("options Value = %q, want a JSON object literal", got[1].Value)
	}
}

// TestBuildInputFields_UndeclaredStructuredExtra confirms that when an
// agent passes a structured value for an off-manifest arg, the extras
// path also routes it through the multiline affordance and JSON
// rendering — there's no second path that would render a stray map as
// Go's default fmt-style garbage.
func TestBuildInputFields_UndeclaredStructuredExtra(t *testing.T) {
	m := &Manifest{
		Inputs: []Input{{Name: "to", Type: "string", Description: "Recipients"}},
	}
	got := BuildInputFields(m, map[string]any{
		"to":    "alr@example.com",
		"extra": map[string]any{"a": 1, "b": 2},
	})
	if len(got) != 2 {
		t.Fatalf("got %d fields, want 2", len(got))
	}
	if got[1].Label != "extra" {
		t.Fatalf("got label %q, want extra", got[1].Label)
	}
	if !got[1].Multiline {
		t.Errorf("extra Multiline = false, want true for structured value")
	}
	if got[1].Value == "" || got[1].Value[0] != '{' {
		t.Errorf("extra Value = %q, want a JSON object literal", got[1].Value)
	}
}

// TestBuildInputFields_NilManifest documents the graceful-degradation
// path: a nil manifest returns nil so callers can pass straight
// through without a surrounding guard.
func TestBuildInputFields_NilManifest(t *testing.T) {
	if got := BuildInputFields(nil, map[string]any{"x": 1}); got != nil {
		t.Errorf("BuildInputFields(nil, _) = %#v, want nil", got)
	}
}

// TestBuildInputFields_NoArgsNoInputs documents the empty case: an
// action that declares no inputs and is called with no args yields
// an empty slice rather than nil so the API surface emits an empty
// JSON array instead of omitting the field.
func TestBuildInputFields_NoArgsNoInputs(t *testing.T) {
	got := BuildInputFields(&Manifest{}, nil)
	if got == nil || len(got) != 0 {
		t.Errorf("BuildInputFields(empty, nil) = %#v, want empty slice", got)
	}
}
