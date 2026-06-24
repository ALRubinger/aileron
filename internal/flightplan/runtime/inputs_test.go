package runtime

import (
	"context"
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
