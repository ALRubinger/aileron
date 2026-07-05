package runtime

import (
	"context"
	"strings"
	"testing"
)

// interpToolPlan builds a single tool step whose command interpolates a
// constrained plan input, materializing an output so the audit path is
// exercised.
func interpToolPlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name:    "interp-plan",
		Actions: map[string]Action{},
		Outputs: map[string]Output{
			"out.txt": {Name: "out.txt", MimeType: "text/plain", Encoding: EncodingUTF8, Target: PublishFile, Path: "out.txt"},
		},
		Inputs: []Input{
			{Name: "region", Type: "string", Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: "us-east-1"}},
		},
		Steps: []Step{
			{
				ID:                 "fetch",
				Kind:               KindTool,
				Bindings:           map[string]Binding{"region": mb("inputs.region")},
				Outputs:            []string{"listing"},
				MaterializesOutput: "out.txt",
				Command:            []string{"aws", "s3", "ls", "--region={{ inputs.region }}"},
				MountPath:          "/work/in",
				CollectPath:        "/work/out",
			},
		},
	}
	order, err := topoSort(p, map[string]int{"fetch": 0})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

// TestExecute_ToolStepInterpolatesCommand proves the executor execs the
// INSTANTIATED argv: the runner receives the resolved value in place of the
// token, never the template.
func TestExecute_ToolStepInterpolatesCommand(t *testing.T) {
	p := interpToolPlan()
	runner := &fakeToolStepRunner{outputs: map[string]any{"fetch": toolFileMap("collected\n")}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	_, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"region": "us-west-2"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(runner.specs) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.specs))
	}
	got := strings.Join(runner.specs[0].Command, " ")
	if got != "aws s3 ls --region=us-west-2" {
		t.Errorf("runner got command %q, want the INSTANTIATED argv", got)
	}
	for _, el := range runner.specs[0].Command {
		if strings.Contains(el, "{{") {
			t.Errorf("runner must not receive a template token, got %q", el)
		}
	}
}

// TestExecute_ToolStepMissingInterpInputErrors proves a command token naming an
// input with no resolved value fails the step closed rather than exec'ing a
// half-built argv. The step declares NO binding for the interpolated input
// (command tokens reference plan inputs directly, not step bindings), so the
// failure surfaces from instantiation, not binding resolution.
func TestExecute_ToolStepMissingInterpInputErrors(t *testing.T) {
	p := &Plan{
		Name:    "interp-plan",
		Actions: map[string]Action{},
		Outputs: map[string]Output{},
		Inputs: []Input{
			{Name: "region", Type: "string", Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: "us-east-1"}},
		},
		Steps: []Step{
			{
				ID:      "fetch",
				Kind:    KindTool,
				Outputs: []string{"listing"},
				Command: []string{"aws", "s3", "ls", "--region={{ inputs.region }}"},
			},
		},
	}
	order, _ := topoSort(p, map[string]int{"fetch": 0})
	p.Order = order

	runner := &fakeToolStepRunner{outputs: map[string]any{"fetch": "x"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	// Resolve a DIFFERENT input, leaving region unresolved.
	_, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"other": "v"}})
	if err == nil {
		t.Fatal("a command token with no resolved input must error")
	}
	if !strings.Contains(err.Error(), "interpolation") {
		t.Errorf("error should name the interpolation failure, got: %v", err)
	}
	if len(runner.specs) != 0 {
		t.Error("the interpolation failure must fire before the subprocess runs")
	}
}

// TestEmitAudit_InterpolatedCommandRecordsResolvedAndDerived proves the output
// record's aileron.step.command is the RESOLVED argv and
// aileron.step.command_derived names the interpolated index.
func TestEmitAudit_InterpolatedCommandRecordsResolvedAndDerived(t *testing.T) {
	p := interpToolPlan()
	runner := &fakeToolStepRunner{outputs: map[string]any{"fetch": toolFileMap("collected\n")}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"region": "us-west-2"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	sink := &recordingSink{}
	emitAudit(context.Background(), sink, st, launchProvenance{Skill: "interp-plan", InvocationID: "inv-1"})

	var out *AuditRecord
	for i := range sink.records {
		if sink.records[i].Kind == RecordKindOutput {
			out = &sink.records[i]
		}
	}
	if out == nil {
		t.Fatal("no output.materialized record emitted")
	}
	cmd, ok := out.Fields["aileron.step.command"].([]string)
	if !ok || strings.Join(cmd, " ") != "aws s3 ls --region=us-west-2" {
		t.Errorf("step.command = %v, want the RESOLVED argv", out.Fields["aileron.step.command"])
	}
	derived, ok := out.Fields["aileron.step.command_derived"].([]int)
	if !ok || len(derived) != 1 || derived[0] != 3 {
		t.Errorf("step.command_derived = %v, want [3]", out.Fields["aileron.step.command_derived"])
	}
}

// TestEmitAudit_TokenFreeCommandHasNoDerivedMarker is the regression guard that
// a token-free tool command records the literal argv with NO derived marker, so
// existing behavior is byte-for-byte unchanged.
func TestEmitAudit_TokenFreeCommandHasNoDerivedMarker(t *testing.T) {
	p := toolMaterializePlan()
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": toolFileMap("collected-bytes\n")}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	sink := &recordingSink{}
	emitAudit(context.Background(), sink, st, launchProvenance{Skill: "tool-plan"})

	var out *AuditRecord
	for i := range sink.records {
		if sink.records[i].Kind == RecordKindOutput {
			out = &sink.records[i]
		}
	}
	if out == nil {
		t.Fatal("no output.materialized record emitted")
	}
	cmd, ok := out.Fields["aileron.step.command"].([]string)
	if !ok || strings.Join(cmd, " ") != "extract-tool" {
		t.Errorf("step.command = %v, want the literal argv", out.Fields["aileron.step.command"])
	}
	if _, present := out.Fields["aileron.step.command_derived"]; present {
		t.Error("a token-free tool command must carry NO aileron.step.command_derived marker")
	}
}
