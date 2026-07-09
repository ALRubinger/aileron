package runtime

import (
	"reflect"
	"testing"
)

// seamAuditPlan builds a minimal plan with one llm-seam step ("summarize") whose
// bindings and the plan's inputs are supplied by the caller, so each scenario can
// exercise the NonSourceSeamBindings source/non-source classification directly.
func seamAuditPlan(inputs []Input, seamBindings map[string]Binding) *Plan {
	return &Plan{
		Name:   "seam-audit-fixture",
		Inputs: inputs,
		Steps: []Step{
			{ID: "read", Kind: KindActionCall, Outputs: []string{"series"}},
			{ID: "summarize", Kind: KindLLMSeam, Bindings: seamBindings, Outputs: []string{"summary"}},
		},
	}
}

func TestNonSourceSeamBindings(t *testing.T) {
	sourceInput := Input{
		Name:       "dataset",
		Type:       "object",
		Resolution: Resolution{Rule: ResolutionSource, SourceActionRef: "aileron:metrics.load_dataset", SourceSelect: "rows"},
	}
	literalInput := Input{
		Name:       "window",
		Type:       "number",
		Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: float64(7)},
	}

	tests := []struct {
		name     string
		inputs   []Input
		binds    map[string]Binding
		bindings map[string]any
		want     map[string]any
	}{
		{
			name:     "BindStep binding passes through",
			inputs:   nil,
			binds:    map[string]Binding{"series": {Kind: BindStep, StepID: "read", Output: "series"}},
			bindings: map[string]any{"series": []any{1, 2, 3}},
			want:     map[string]any{"series": []any{1, 2, 3}},
		},
		{
			name:     "BindInput to a literal input passes through",
			inputs:   []Input{literalInput},
			binds:    map[string]Binding{"window": {Kind: BindInput, Name: "window"}},
			bindings: map[string]any{"window": float64(7)},
			want:     map[string]any{"window": float64(7)},
		},
		{
			name:     "BindInput to a source input is excluded",
			inputs:   []Input{sourceInput},
			binds:    map[string]Binding{"dataset": {Kind: BindInput, Name: "dataset"}},
			bindings: map[string]any{"dataset": []any{map[string]any{"secret": "x"}}},
			want:     map[string]any{},
		},
		{
			name:   "mixed: source excluded, step + literal kept",
			inputs: []Input{sourceInput, literalInput},
			binds: map[string]Binding{
				"dataset": {Kind: BindInput, Name: "dataset"},
				"series":  {Kind: BindStep, StepID: "read", Output: "series"},
				"window":  {Kind: BindInput, Name: "window"},
			},
			bindings: map[string]any{
				"dataset": []any{map[string]any{"secret": "x"}},
				"series":  []any{1, 2, 3},
				"window":  float64(7),
			},
			want: map[string]any{
				"series": []any{1, 2, 3},
				"window": float64(7),
			},
		},
		{
			name:     "empty bindings yield empty map",
			inputs:   []Input{sourceInput},
			binds:    map[string]Binding{"dataset": {Kind: BindInput, Name: "dataset"}},
			bindings: map[string]any{},
			want:     map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := seamAuditPlan(tt.inputs, tt.binds)
			got := NonSourceSeamBindings(plan, "summarize", tt.bindings)
			if got == nil {
				t.Fatal("NonSourceSeamBindings returned nil, want a non-nil map")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NonSourceSeamBindings = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNonSourceSeamBindings_UnknownStep: an unknown step id cannot classify any
// binding against the source rule, so the helper fails closed to an empty map
// rather than leaking unclassified values.
func TestNonSourceSeamBindings_UnknownStep(t *testing.T) {
	plan := seamAuditPlan(nil, map[string]Binding{"series": {Kind: BindStep, StepID: "read", Output: "series"}})
	got := NonSourceSeamBindings(plan, "does-not-exist", map[string]any{"series": []any{1}})
	if len(got) != 0 {
		t.Errorf("unknown step id yielded %v, want empty map", got)
	}
}

// TestNonSourceSeamBindings_NilPlan: a nil plan yields an empty, non-nil map.
func TestNonSourceSeamBindings_NilPlan(t *testing.T) {
	got := NonSourceSeamBindings(nil, "summarize", map[string]any{"series": []any{1}})
	if got == nil || len(got) != 0 {
		t.Errorf("nil plan yielded %v, want empty non-nil map", got)
	}
}
