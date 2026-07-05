package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
)

const templatedHost = "athena.{{ inputs.aws_region }}.amazonaws.com"

// enumRegionInput builds a constrained aws_region input (enum us-east-1 /
// us-west-2). rule literal with no default means its value is taken from the
// launch args, so an out-of-bound arg fails the #1957 constraint at resolution.
func enumRegionInput() Input {
	return Input{
		Name: "aws_region", Type: "string",
		Resolution: Resolution{Rule: ResolutionLiteral},
		Constraint: &Constraint{Enum: []string{"us-east-1", "us-west-2"}},
	}
}

// TestExecute_ToolStepInstantiatesSealedHost proves the executor instantiates
// the sealed host TEMPLATE from the resolved plan inputs BEFORE minting the
// step scope (#1959): the runner spec and the reach record both carry the
// CONCRETE host, never the template, and the reach is enforced.
func TestExecute_ToolStepInstantiatesSealedHost(t *testing.T) {
	p := toolStepPlan()
	p.Inputs = append(p.Inputs, enumRegionInput())
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{templatedHost}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{
		plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner,
		stepTrust: map[string]freeze.StepReach{"extract": {Hosts: []string{templatedHost}}},
	}
	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello", "aws_region": "us-east-1"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(runner.specs) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.specs))
	}
	if got := strings.Join(runner.specs[0].Hosts, ","); got != "athena.us-east-1.amazonaws.com" {
		t.Errorf("spec.Hosts = %q, want the INSTANTIATED host minted into the step scope", got)
	}
	for _, h := range runner.specs[0].Hosts {
		if strings.Contains(h, "{{") {
			t.Errorf("the step-scope mint must not receive a template token, got %q", h)
		}
	}
	if len(st.reaches) != 1 {
		t.Fatalf("reaches = %d, want 1", len(st.reaches))
	}
	if got := strings.Join(st.reaches[0].Hosts, ","); got != "athena.us-east-1.amazonaws.com" {
		t.Errorf("reach hosts = %q, want the INSTANTIATED host", got)
	}
	if !st.reaches[0].Enforced {
		t.Error("a sealed, instantiated reach must record enforced:true")
	}
}

// TestEmitAudit_ReachRecordShowsInstantiatedHost proves the audit reach record
// (aileron.reach.hosts) shows the INSTANTIATED host, not the sealed template.
func TestEmitAudit_ReachRecordShowsInstantiatedHost(t *testing.T) {
	p := toolStepPlan()
	p.Inputs = append(p.Inputs, enumRegionInput())
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{templatedHost}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{
		plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner,
		stepTrust: map[string]freeze.StepReach{"extract": {Hosts: []string{templatedHost}}},
	}
	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello", "aws_region": "us-west-2"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	sink := &recordingSink{}
	emitAudit(context.Background(), sink, st, launchProvenance{Skill: "tool-plan", InvocationID: "inv-1"})

	var reach *AuditRecord
	for i := range sink.records {
		if sink.records[i].Kind == RecordKindReach {
			reach = &sink.records[i]
		}
	}
	if reach == nil {
		t.Fatal("no reach record emitted")
	}
	hosts, ok := reach.Fields["aileron.reach.hosts"].([]string)
	if !ok || strings.Join(hosts, ",") != "athena.us-west-2.amazonaws.com" {
		t.Errorf("aileron.reach.hosts = %v, want the INSTANTIATED host", reach.Fields["aileron.reach.hosts"])
	}
}

// TestExecute_TokenFreeSealedHostUnchanged is the regression guard that a
// token-free sealed host instantiates to itself: an existing non-templated plan
// mints exactly the sealed host, byte-for-byte unchanged.
func TestExecute_TokenFreeSealedHostUnchanged(t *testing.T) {
	p := toolStepPlan()
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{"api.example.com"}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{
		plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner,
		stepTrust: map[string]freeze.StepReach{"extract": {Hosts: []string{"api.example.com", "cdn.example.com"}}},
	}
	if _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := strings.Join(runner.specs[0].Hosts, ","); got != "api.example.com,cdn.example.com" {
		t.Errorf("spec.Hosts = %q, want the sealed hosts verbatim", got)
	}
}

// TestExecute_InstantiatedHostShapeViolationFailsClosed proves a value that
// substitutes to a non-host string (a scheme, a path byte) fails the step
// closed at the post-instantiation shape re-check, BEFORE the step-scope mint —
// instantiate first, then exact-match, never let a bad host reach the proxy.
// The constraint guard is freeze-only, so at runtime a value could carry a
// stray byte; this local re-check is the fail-closed backstop.
func TestExecute_InstantiatedHostShapeViolationFailsClosed(t *testing.T) {
	p := toolStepPlan()
	p.Inputs = append(p.Inputs, enumRegionInput())
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{templatedHost}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{
		plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner,
		stepTrust: map[string]freeze.StepReach{"extract": {Hosts: []string{templatedHost}}},
	}
	// A value carrying a path byte substitutes to athena.us-east-1/x.amazonaws.com.
	_, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello", "aws_region": "us-east-1/x"}})
	if err == nil {
		t.Fatal("an instantiated host that violates the host[:port] shape must fail the step closed")
	}
	if !strings.Contains(err.Error(), "host interpolation") {
		t.Errorf("error should name the host interpolation failure, got: %v", err)
	}
	if len(runner.specs) != 0 {
		t.Error("the shape re-check must fire BEFORE the step-scope mint / subprocess runs")
	}
}

// TestRunPlan_OutOfConstraintRegionFailsBeforeStep proves an out-of-constraint
// input value fails the launch closed at input resolution (#1957), before the
// tool step (and its host instantiation) ever runs — the constraint gate, not
// a proxy error, is what stops an unexpected region.
func TestRunPlan_OutOfConstraintRegionFailsBeforeStep(t *testing.T) {
	p := toolStepPlan()
	p.Inputs = append(p.Inputs, enumRegionInput())
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{templatedHost}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	stepTrust := map[string]freeze.StepReach{"extract": {Hosts: []string{templatedHost}}}
	_, err := runPlan(context.Background(), p, "sha256:test", "sha256:signer", stepTrust, Options{
		Clock:      FixedClock{},
		ToolRunner: runner,
		Inputs:     LaunchArgs{"payload": "hello", "aws_region": "eu-west-1"},
	})
	if err == nil {
		t.Fatal("an out-of-constraint region must fail the launch closed at resolution")
	}
	if len(runner.specs) != 0 {
		t.Error("the constraint gate must fire BEFORE the tool step runs; the runner must not be invoked")
	}
}
