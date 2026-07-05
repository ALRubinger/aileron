package runtime

import (
	"context"
	"regexp"
	"testing"
	"time"
)

func planWithInputs(inputs []Input, actions map[string]Action) *Plan {
	if actions == nil {
		actions = map[string]Action{}
	}
	return &Plan{Name: "t", Inputs: inputs, Actions: actions, Outputs: map[string]Output{}}
}

func TestResolveInputs_LiteralDefaultAndOverride(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "window", Type: "number", Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: 7}},
	}, nil)

	// Default applies when no override.
	ri, err := resolveInputs(context.Background(), p, nil, FixedClock{}, &enforcer{})
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if ri.Values["window"] != 7 {
		t.Errorf("default not applied: %v", ri.Values["window"])
	}

	// Override wins.
	ri, err = resolveInputs(context.Background(), p, LaunchArgs{"window": 30}, FixedClock{}, &enforcer{})
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if ri.Values["window"] != 30 {
		t.Errorf("override not applied: %v", ri.Values["window"])
	}
}

func TestResolveInputs_MissingRequiredLiteralErrors(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "req", Type: "string", Resolution: Resolution{Rule: ResolutionLiteral}},
	}, nil)
	if _, err := resolveInputs(context.Background(), p, nil, FixedClock{}, &enforcer{}); err == nil {
		t.Fatal("a required literal with no default and no override must error")
	}
}

func TestResolveInputs_DynamicResolvedOnce(t *testing.T) {
	fixed := time.Date(2026, 6, 24, 15, 4, 5, 0, time.UTC)
	p := planWithInputs([]Input{
		{Name: "now_ts", Type: "timestamp", Resolution: Resolution{Rule: ResolutionDynamic, DynamicValue: "now"}},
		{Name: "today", Type: "string", Resolution: Resolution{Rule: ResolutionDynamic, DynamicValue: "today"}},
	}, nil)
	ri, err := resolveInputs(context.Background(), p, nil, FixedClock{T: fixed}, &enforcer{})
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if ri.Values["now_ts"] != "2026-06-24T15:04:05Z" {
		t.Errorf("now = %v", ri.Values["now_ts"])
	}
	if ri.Values["today"] != "2026-06-24" {
		t.Errorf("today = %v", ri.Values["today"])
	}
}

// countingClock counts Now() calls so we can prove the runtime reads the clock
// once at the launch boundary (#1523), not per-input or per-step.
type countingClock struct {
	t     time.Time
	calls int
}

func (c *countingClock) Now() time.Time {
	c.calls++
	return c.t
}

func TestResolveInputs_ClockReadOnce(t *testing.T) {
	clk := &countingClock{t: time.Unix(0, 0).UTC()}
	p := planWithInputs([]Input{
		{Name: "a", Type: "timestamp", Resolution: Resolution{Rule: ResolutionDynamic, DynamicValue: "now"}},
		{Name: "b", Type: "timestamp", Resolution: Resolution{Rule: ResolutionDynamic, DynamicValue: "now"}},
		{Name: "c", Type: "string", Resolution: Resolution{Rule: ResolutionDynamic, DynamicValue: "today"}},
	}, nil)
	if _, err := resolveInputs(context.Background(), p, nil, clk, &enforcer{}); err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if clk.calls != 1 {
		t.Errorf("clock read %d times, want exactly 1 (launch-boundary single read)", clk.calls)
	}
}

func TestResolveInputs_SourceRecordedByBindingNotDataset(t *testing.T) {
	ref := "aileron:metrics.query_series"
	actions := map[string]Action{ref: {Ref: ref, TrustContract: TrustContract{Effect: EffectRead, Hosts: []string{"h"}}}}
	p := planWithInputs([]Input{
		{Name: "live", Type: "array", Resolution: Resolution{Rule: ResolutionSource, SourceActionRef: ref, SourceSelect: "series[].name"}},
	}, actions)

	disp := &fakeDispatcher{result: map[string]any{
		"series": []any{
			map[string]any{"name": "cpu"},
			map[string]any{"name": "mem"},
		},
	}}
	enf := &enforcer{dispatcher: disp, approver: &fakeApprover{}}
	ri, err := resolveInputs(context.Background(), p, nil, FixedClock{}, enf)
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	// The select extracted the series names.
	got, ok := ri.Values["live"].([]any)
	if !ok || len(got) != 2 || got[0] != "cpu" || got[1] != "mem" {
		t.Errorf("select result = %v", ri.Values["live"])
	}
	// The audit binding records the action + select, never the dataset.
	sb, ok := ri.SourceBindings["live"]
	if !ok || sb.ActionRef != ref || sb.Select != "series[].name" {
		t.Errorf("source binding = %+v", sb)
	}
}

func TestResolveInputs_SourceUndeclaredActionErrors(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "live", Type: "array", Resolution: Resolution{Rule: ResolutionSource, SourceActionRef: "aileron:ghost.read"}},
	}, nil)
	enf := &enforcer{dispatcher: &fakeDispatcher{}, approver: &fakeApprover{}}
	if _, err := resolveInputs(context.Background(), p, nil, FixedClock{}, enf); err == nil {
		t.Fatal("a source input naming an undeclared action must error")
	}
}

func TestResolveInputs_FrozenFromLaunchArgMutation(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "obj", Type: "object", Resolution: Resolution{Rule: ResolutionLiteral}},
	}, nil)
	arg := map[string]any{"k": "v"}
	ri, err := resolveInputs(context.Background(), p, LaunchArgs{"obj": arg}, FixedClock{}, &enforcer{})
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	// Mutating the launch arg after resolution must not change the frozen set.
	arg["k"] = "tampered"
	got := ri.Values["obj"].(map[string]any)
	if got["k"] != "v" {
		t.Error("resolved inputs must be frozen against later launch-arg mutation")
	}
}

func TestResolveInputs_EnumConstraintInBoundPasses(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "env", Type: "string",
			Resolution: Resolution{Rule: ResolutionLiteral},
			Constraint: &Constraint{Enum: []string{"prod", "staging"}}},
	}, nil)
	ri, err := resolveInputs(context.Background(), p, LaunchArgs{"env": "prod"}, FixedClock{}, &enforcer{})
	if err != nil {
		t.Fatalf("an in-constraint enum value must resolve: %v", err)
	}
	if ri.Values["env"] != "prod" {
		t.Errorf("env = %v", ri.Values["env"])
	}
}

func TestResolveInputs_EnumConstraintOutOfBoundFailsClosed(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "env", Type: "string",
			Resolution: Resolution{Rule: ResolutionLiteral},
			Constraint: &Constraint{Enum: []string{"prod", "staging"}}},
	}, nil)
	if _, err := resolveInputs(context.Background(), p, LaunchArgs{"env": "dev"}, FixedClock{}, &enforcer{}); err == nil {
		t.Fatal("an out-of-constraint enum value must fail the launch closed")
	}
}

func TestResolveInputs_PatternConstraintInBoundPasses(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "region", Type: "string",
			Resolution: Resolution{Rule: ResolutionLiteral},
			Constraint: &Constraint{Pattern: regexp.MustCompile("^us-[a-z]+-[0-9]$")}},
	}, nil)
	ri, err := resolveInputs(context.Background(), p, LaunchArgs{"region": "us-east-1"}, FixedClock{}, &enforcer{})
	if err != nil {
		t.Fatalf("an in-constraint pattern value must resolve: %v", err)
	}
	if ri.Values["region"] != "us-east-1" {
		t.Errorf("region = %v", ri.Values["region"])
	}
}

func TestResolveInputs_PatternConstraintOutOfBoundFailsClosed(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "region", Type: "string",
			Resolution: Resolution{Rule: ResolutionLiteral},
			Constraint: &Constraint{Pattern: regexp.MustCompile("^us-[a-z]+-[0-9]$")}},
	}, nil)
	if _, err := resolveInputs(context.Background(), p, LaunchArgs{"region": "moon-base-7"}, FixedClock{}, &enforcer{}); err == nil {
		t.Fatal("an out-of-constraint pattern value must fail the launch closed")
	}
}

// A number input's resolved value is checked by its string form, so a numeric
// default can be enum-constrained. This locks the stringify-then-check contract.
func TestResolveInputs_ConstraintChecksStringForm(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "days", Type: "number",
			Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: 7},
			Constraint: &Constraint{Enum: []string{"7", "30"}}},
	}, nil)
	if _, err := resolveInputs(context.Background(), p, nil, FixedClock{}, &enforcer{}); err != nil {
		t.Fatalf("a number whose string form is allowed must resolve: %v", err)
	}
	if _, err := resolveInputs(context.Background(), p, LaunchArgs{"days": 90}, FixedClock{}, &enforcer{}); err == nil {
		t.Fatal("a number whose string form is not allowed must fail closed")
	}
}

// A dynamic input is constraint-checked the same as a literal, proving the
// enforcement is rule-agnostic rather than limited to literal inputs. The
// `today` rule resolves to a 2006-01-02 date under FixedClock, so an in-bound
// date pattern passes and an out-of-bound pattern fails the launch closed.
func TestResolveInputs_ConstraintEnforcedOnDynamicInput(t *testing.T) {
	fixed := time.Date(2026, 6, 24, 15, 4, 5, 0, time.UTC)

	inBound := planWithInputs([]Input{
		{Name: "today", Type: "string",
			Resolution: Resolution{Rule: ResolutionDynamic, DynamicValue: "today"},
			Constraint: &Constraint{Pattern: regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)}},
	}, nil)
	ri, err := resolveInputs(context.Background(), inBound, nil, FixedClock{T: fixed}, &enforcer{})
	if err != nil {
		t.Fatalf("a dynamic value inside its constraint must resolve: %v", err)
	}
	if ri.Values["today"] != "2026-06-24" {
		t.Errorf("today = %v", ri.Values["today"])
	}

	outOfBound := planWithInputs([]Input{
		{Name: "today", Type: "string",
			Resolution: Resolution{Rule: ResolutionDynamic, DynamicValue: "today"},
			Constraint: &Constraint{Pattern: regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`)}},
	}, nil)
	if _, err := resolveInputs(context.Background(), outOfBound, nil, FixedClock{T: fixed}, &enforcer{}); err == nil {
		t.Fatal("a dynamic value outside its constraint must fail the launch closed")
	}
}

// An input with no declared constraint is never checked (today's behavior).
func TestResolveInputs_NoConstraintUnchecked(t *testing.T) {
	p := planWithInputs([]Input{
		{Name: "free", Type: "string", Resolution: Resolution{Rule: ResolutionLiteral}},
	}, nil)
	ri, err := resolveInputs(context.Background(), p, LaunchArgs{"free": "anything"}, FixedClock{}, &enforcer{})
	if err != nil {
		t.Fatalf("an unconstrained input must resolve any value: %v", err)
	}
	if ri.Values["free"] != "anything" {
		t.Errorf("free = %v", ri.Values["free"])
	}
}

func TestApplySelect_EmptyReturnsWholeResult(t *testing.T) {
	res := map[string]any{"a": 1}
	got := applySelect(res, "")
	if m, ok := got.(map[string]any); !ok || m["a"] != 1 {
		t.Errorf("empty select must return the whole result, got %v", got)
	}
}

func TestApplySelect_MissingPathReturnsNil(t *testing.T) {
	if got := applySelect(map[string]any{"a": 1}, "b.c"); got != nil {
		t.Errorf("a select that matches nothing must return nil, got %v", got)
	}
}

func TestApplySelect_NestedScalar(t *testing.T) {
	res := map[string]any{"meta": map[string]any{"id": "x"}}
	if got := applySelect(res, "meta.id"); got != "x" {
		t.Errorf("nested select = %v, want x", got)
	}
}

func TestResolveInputs_SourceSelectMissingYieldsNil(t *testing.T) {
	ref := "aileron:m.read"
	actions := map[string]Action{ref: {Ref: ref, TrustContract: TrustContract{Effect: EffectRead, Hosts: []string{"h"}}}}
	p := planWithInputs([]Input{
		{Name: "x", Type: "string", Resolution: Resolution{Rule: ResolutionSource, SourceActionRef: ref, SourceSelect: "missing.path"}},
	}, actions)
	enf := &enforcer{dispatcher: &fakeDispatcher{result: map[string]any{"present": 1}}, approver: &fakeApprover{}}
	ri, err := resolveInputs(context.Background(), p, nil, FixedClock{}, enf)
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if ri.Values["x"] != nil {
		t.Errorf("a select that matches nothing must resolve to nil, got %v", ri.Values["x"])
	}
}
