package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
)

// fakeToolStepRunner records the specs it was handed and returns a canned
// collected output keyed by step id, so a test asserts the argv, the mounted
// input, the sealed hosts, and the collected output all flow correctly.
type fakeToolStepRunner struct {
	specs    []ToolStepSpec
	outputs  map[string]any // step id -> collected output
	forceErr error
}

func (f *fakeToolStepRunner) Run(_ context.Context, spec ToolStepSpec) (ToolStepResult, error) {
	f.specs = append(f.specs, spec)
	if f.forceErr != nil {
		return ToolStepResult{}, f.forceErr
	}
	return ToolStepResult{Output: f.outputs[spec.StepID]}, nil
}

// trustContract builds the untyped per-step trust-contract map of the shape
// the manifest carries, with the given hosts and effect and the minimal
// credential/idempotency/audit the shared validator requires.
func trustContract(effect string, hosts ...string) map[string]any {
	hs := make([]any, len(hosts))
	for i, h := range hosts {
		hs[i] = h
	}
	return map[string]any{
		"credential":  map[string]any{"kind": "none"},
		"hosts":       hs,
		"effect":      effect,
		"idempotency": map[string]any{"safeToRetry": true},
		"audit":       map[string]any{"fields": []any{"result"}},
	}
}

// --- decode: kind: tool steps (#1829) ---

// TestDecode_ToolStepHappyPath proves a tool step decodes with its argv,
// mount/collect paths, and validated trust contract carried onto the typed
// Step, and participates in graph ordering/bindings like any other kind.
func TestDecode_ToolStepHappyPath(t *testing.T) {
	m := rawManifest(
		[]any{map[string]any{
			"name": "payload", "type": "string",
			"resolution": map[string]any{"rule": "literal", "default": "hello"},
		}},
		nil,
		[]any{
			step(map[string]any{
				"id": "extract", "kind": "tool",
				"command":       []any{"extract-tool", "--mode", "csv"},
				"bindings":      map[string]any{"payload": "inputs.payload"},
				"mount":         map[string]any{"path": "/work/in"},
				"collect":       map[string]any{"path": "/work/out/result.csv"},
				"trustContract": trustContract("read", "api.example.com", "api.example.com:443"),
				"outputs":       []any{"result"},
			}),
			step(map[string]any{
				"id": "consume", "kind": "transform",
				"bindings": map[string]any{"upstream": "steps.extract.result"},
				"outputs":  []any{"final"},
			}),
		})
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var tool *Step
	for i := range p.Steps {
		if p.Steps[i].ID == "extract" {
			tool = &p.Steps[i]
		}
	}
	if tool == nil || tool.Kind != KindTool {
		t.Fatalf("extract must decode as a KindTool step, got %+v", p.Steps)
	}
	if strings.Join(tool.Command, " ") != "extract-tool --mode csv" {
		t.Errorf("command = %v, want the declared argv", tool.Command)
	}
	if tool.MountPath != "/work/in" || tool.CollectPath != "/work/out/result.csv" {
		t.Errorf("mount/collect = %q/%q", tool.MountPath, tool.CollectPath)
	}
	if tool.TrustContract == nil || tool.TrustContract.Effect != EffectRead {
		t.Fatalf("trust contract not carried: %+v", tool.TrustContract)
	}
	if strings.Join(tool.TrustContract.Hosts, ",") != "api.example.com,api.example.com:443" {
		t.Errorf("contract hosts = %v", tool.TrustContract.Hosts)
	}
	// The tool step participates in graph ordering: consume binds its output,
	// so extract must precede consume in the topological order.
	if p.Steps[p.Order[0]].ID != "extract" {
		t.Errorf("first step = %q, want extract", p.Steps[p.Order[0]].ID)
	}
}

// TestDecode_ToolStepMinimal proves a tool step with only an argv (no
// mount/collect/contract) decodes: absence of the optional blocks is
// handled, not assumed present.
func TestDecode_ToolStepMinimal(t *testing.T) {
	m := rawManifest(nil, nil, []any{step(map[string]any{
		"id": "s1", "kind": "tool", "command": []any{"tool"}, "outputs": []any{"out"},
	})})
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	s := p.Steps[0]
	if s.MountPath != "" || s.CollectPath != "" || s.TrustContract != nil {
		t.Errorf("minimal tool step must have empty mount/collect and nil contract, got %+v", s)
	}
}

// TestDecode_ToolStepRefusals proves each malformed tool-step shape is a
// decode refusal: no command, an empty argv element, an empty mount/collect
// path, a stray actionRef/transform/args, and an invalid trust contract.
func TestDecode_ToolStepRefusals(t *testing.T) {
	cases := []struct {
		name    string
		fields  map[string]any
		wantErr string
	}{
		{"no command", map[string]any{
			"id": "s1", "kind": "tool", "outputs": []any{"out"},
		}, "declares no command"},
		{"empty argv element", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool", ""}, "outputs": []any{"out"},
		}, "command element 1 is empty"},
		{"zero outputs", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"},
		}, "exactly one output"},
		{"multiple outputs", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"}, "outputs": []any{"a", "b"},
		}, "exactly one output"},
		{"empty mount path", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"}, "mount": map[string]any{"path": ""}, "outputs": []any{"out"},
		}, "mount with an empty path"},
		{"empty collect path", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"}, "collect": map[string]any{"path": ""}, "outputs": []any{"out"},
		}, "collect with an empty path"},
		{"actionRef on tool", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"}, "actionRef": "aileron:metrics.query_series", "outputs": []any{"out"},
		}, "must not declare an actionRef"},
		{"transform on tool", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"}, "transform": "html-render", "outputs": []any{"out"},
		}, "must not declare a transform"},
		{"args on tool", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"}, "args": map[string]any{"x": "inputs.x"}, "outputs": []any{"out"},
		}, "must not declare args"},
		{"contract with no hosts", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"}, "trustContract": trustContract("read"), "outputs": []any{"out"},
		}, "no hosts"},
		{"contract with unknown effect", map[string]any{
			"id": "s1", "kind": "tool", "command": []any{"tool"}, "trustContract": trustContract("teleport", "api.example.com"), "outputs": []any{"out"},
		}, "unknown effect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := rawManifest(nil, nil, []any{step(tc.fields)})
			_, err := Decode(m)
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestDecode_ToolSurfaceRefusedOnOtherKinds proves the tool-step surface
// (command/mount/collect/trustContract) is closed to kind: tool — any other
// kind carrying one of these fields is refused rather than silently ignored.
func TestDecode_ToolSurfaceRefusedOnOtherKinds(t *testing.T) {
	cases := []struct {
		name   string
		extra  map[string]any
		wanted string
	}{
		{"command on transform", map[string]any{"command": []any{"tool"}}, "must not declare a command"},
		{"mount on transform", map[string]any{"mount": map[string]any{"path": "/w"}}, "must not declare mount/collect"},
		{"collect on transform", map[string]any{"collect": map[string]any{"path": "/w/out"}}, "must not declare mount/collect"},
		{"trustContract on transform", map[string]any{"trustContract": trustContract("read", "api.example.com")}, "must not declare a trustContract"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}}
			for k, v := range tc.extra {
				fields[k] = v
			}
			_, err := Decode(rawManifest(nil, nil, []any{step(fields)}))
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wanted) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wanted)
			}
		})
	}
}

// --- executor: tool-step execution through the ToolStepRunner seam ---

// digestA is a valid content-addressed digest for tests.
const digestA = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// toolStepPlan builds a two-step plan: a tool step (extract) that mounts an
// input and collects an output, and a downstream transform (consume) that
// binds the collected output. It is the executor fixture for the
// mount → run → collect and dataflow proofs.
func toolStepPlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name:    "tool-plan",
		Actions: map[string]Action{},
		Outputs: map[string]Output{},
		Inputs: []Input{
			{Name: "payload", Type: "string", Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: "hello"}},
		},
		Steps: []Step{
			{
				ID:   "extract",
				Kind: KindTool,
				Bindings: map[string]Binding{
					"payload": mb("inputs.payload"),
				},
				Outputs:     []string{"result"},
				Command:     []string{"extract-tool", "--mode", "csv"},
				MountPath:   "/work/in",
				CollectPath: "/work/out/result.csv",
			},
			{
				ID:       "consume",
				Kind:     KindTransform,
				Bindings: map[string]Binding{"upstream": mb("steps.extract.result")},
				Outputs:  []string{"final"},
			},
		},
	}
	order, err := topoSort(p, map[string]int{"extract": 0, "consume": 1})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

// setExtractContract attaches a per-step trust contract to the extract tool
// step of a toolStepPlan.
func setExtractContract(p *Plan, tc *TrustContract) {
	p.Steps[0].TrustContract = tc
}

// TestExecute_ToolStepMountsRunsCollects proves a tool step runs through the
// subprocess seam with the argv, mount/collect paths, and resolved input
// threaded, and the collected output becomes the step's declared output.
func TestExecute_ToolStepMountsRunsCollects(t *testing.T) {
	p := toolStepPlan()
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}

	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}
	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(runner.specs) != 1 {
		t.Fatalf("tool runner must be called exactly once, got %d", len(runner.specs))
	}
	spec := runner.specs[0]
	if strings.Join(spec.Command, " ") != "extract-tool --mode csv" {
		t.Errorf("runner got command %v, want the step argv verbatim", spec.Command)
	}
	if spec.MountPath != "/work/in" || spec.CollectPath != "/work/out/result.csv" {
		t.Errorf("runner got mount/collect %q/%q", spec.MountPath, spec.CollectPath)
	}
	in, ok := spec.Input.(map[string]any)
	if !ok || in["payload"] != "hello" {
		t.Errorf("runner got mounted input %#v, want the resolved payload", spec.Input)
	}
	// No sealed reach threaded: the spec carries no hosts.
	if len(spec.Hosts) != 0 {
		t.Errorf("spec.Hosts = %v, want empty with no sealed reach", spec.Hosts)
	}
	if got := st.stepOutput["extract"]["result"]; got != "COLLECTED" {
		t.Errorf("collected output = %v, want COLLECTED", got)
	}
}

// TestExecute_ToolStepThreadsSealedHosts proves the executor threads the
// verified lock's SEALED reach (stepTrust) into the runner spec — never the
// frontmatter contract's hosts — so the runner scopes the subprocess to
// exactly what the signature sealed.
func TestExecute_ToolStepThreadsSealedHosts(t *testing.T) {
	p := toolStepPlan()
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{"frontmatter.example.com"}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{
		plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner,
		stepTrust: map[string]freeze.StepReach{"extract": {Hosts: []string{"sealed.example.com"}}},
	}
	if _, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Join(runner.specs[0].Hosts, ",") != "sealed.example.com" {
		t.Errorf("spec.Hosts = %v, want the SEALED reach only", runner.specs[0].Hosts)
	}
}

// TestExecute_ToolStepThreadsCredentialIdentity proves the executor threads
// the step's declared credential identity (kind + identity label from the
// trust contract) into the runner spec (#1980), so the runner's mint tells the
// daemon which credential identity the step's egress belongs to.
func TestExecute_ToolStepThreadsCredentialIdentity(t *testing.T) {
	p := toolStepPlan()
	setExtractContract(p, &TrustContract{
		Effect:         EffectRead,
		Hosts:          []string{"api.example.com"},
		CredentialKind: "aws_sigv4",
		IdentityLabel:  "prod-reader",
	})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{
		plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner,
		stepTrust: map[string]freeze.StepReach{"extract": {Hosts: []string{"api.example.com"}}},
	}
	if _, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	spec := runner.specs[0]
	if spec.CredentialKind != "aws_sigv4" || spec.IdentityLabel != "prod-reader" {
		t.Errorf("spec identity = (%q, %q), want (aws_sigv4, prod-reader)", spec.CredentialKind, spec.IdentityLabel)
	}
}

// TestExecute_ToolStepNoCredentialIdentityLeavesEmpty proves a step whose
// trust contract declares no credential identity threads empty strings, so the
// runner's mint sends no credential block and the scope stays unconstrained —
// today's behavior preserved by construction.
func TestExecute_ToolStepNoCredentialIdentityLeavesEmpty(t *testing.T) {
	p := toolStepPlan()
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{"api.example.com"}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{
		plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner,
		stepTrust: map[string]freeze.StepReach{"extract": {Hosts: []string{"api.example.com"}}},
	}
	if _, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	spec := runner.specs[0]
	if spec.CredentialKind != "" || spec.IdentityLabel != "" {
		t.Errorf("spec identity = (%q, %q), want empty with no declared credential identity", spec.CredentialKind, spec.IdentityLabel)
	}
}

// TestExecute_CollectedOutputFlowsDownstream proves the collected output
// flows into a downstream step's dataflow through the existing binding path,
// with no new dataflow mechanism.
func TestExecute_CollectedOutputFlowsDownstream(t *testing.T) {
	p := toolStepPlan()
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: b["upstream"]}, nil
	})
	p.Steps[1].Transform = "identity"

	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: reg, toolRunner: runner}
	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := st.stepOutput["consume"]["final"]; got != "COLLECTED" {
		t.Errorf("downstream step must receive the collected output, got %v", got)
	}
}

// TestExecute_ToolStepNoRunnerErrors proves a tool step with no configured
// runner is an explicit error, never a silent skip.
func TestExecute_ToolStepNoRunnerErrors(t *testing.T) {
	p := toolStepPlan()
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: nil}
	_, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil {
		t.Fatal("a tool step with no runner must error")
	}
	if !strings.Contains(err.Error(), "no tool step runner is configured") {
		t.Errorf("error should name the missing runner, got: %v", err)
	}
}

// TestExecute_ToolStepMultipleOutputsErrors proves a tool step declaring
// multiple outputs is refused BEFORE the subprocess runs: a single collected
// blob has no unambiguous mapping across several declared outputs, and a
// misdeclared step must never trigger a (possibly non-idempotent) execution
// whose result would only be discarded. (Decode refuses this shape too; this
// is the direct-construct backstop.)
func TestExecute_ToolStepMultipleOutputsErrors(t *testing.T) {
	p := toolStepPlan()
	p.Steps[0].Outputs = []string{"a", "b"}
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p.Steps[1].Bindings = map[string]Binding{"upstream": mb("steps.extract.a")}
	order, _ := topoSort(p, map[string]int{"extract": 0, "consume": 1})
	p.Order = order

	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}
	_, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil || !strings.Contains(err.Error(), "exactly one collected output") {
		t.Fatalf("a multi-output tool step must error, got %v", err)
	}
	if len(runner.specs) != 0 {
		t.Fatal("the arity refusal must fire BEFORE the subprocess runs")
	}
}

// TestExecute_ToolStepZeroOutputsErrors proves a tool step declaring zero
// outputs (materialize-only shape) is refused before the subprocess runs:
// the collected blob has nowhere to land.
func TestExecute_ToolStepZeroOutputsErrors(t *testing.T) {
	p := toolStepPlan()
	p.Steps = p.Steps[:1]
	p.Steps[0].Outputs = nil
	p.Steps[0].MaterializesOutput = "" // direct-construct; decode would refuse
	p.Order = []int{0}

	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}
	_, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil || !strings.Contains(err.Error(), "exactly one collected output") {
		t.Fatalf("a zero-output tool step must error, got %v", err)
	}
	if len(runner.specs) != 0 {
		t.Fatal("the arity refusal must fire BEFORE the subprocess runs")
	}
}

// TestExecute_ToolStepRunnerErrorPropagates proves a runner failure aborts
// the walk rather than being swallowed.
func TestExecute_ToolStepRunnerErrorPropagates(t *testing.T) {
	p := toolStepPlan()
	runner := &fakeToolStepRunner{forceErr: context.DeadlineExceeded}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}
	_, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil {
		t.Fatal("a tool-runner error must propagate")
	}
}

// --- reach capture (#1784, enforced dimension #1829) ---

// TestExecute_ToolStepCapturesSealedReachEnforced proves a contracted tool
// step with a sealed stepTrust entry captures exactly one reach entry
// carrying the SEALED hosts marked enforced:true.
func TestExecute_ToolStepCapturesSealedReachEnforced(t *testing.T) {
	p := toolStepPlan()
	setExtractContract(p, &TrustContract{Effect: EffectExternalSend, Hosts: []string{"frontmatter.example.com"}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{
		plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner,
		stepTrust: map[string]freeze.StepReach{"extract": {Hosts: []string{"api.example.com", "cdn.example.com"}}},
	}
	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.reaches) != 1 {
		t.Fatalf("reaches = %d, want exactly 1", len(st.reaches))
	}
	r := st.reaches[0]
	if r.StepID != "extract" || r.Effect != EffectExternalSend {
		t.Errorf("reach = %+v, want extract/external-send", r)
	}
	if strings.Join(r.Hosts, ",") != "api.example.com,cdn.example.com" {
		t.Errorf("reach hosts = %v, want the sealed hosts", r.Hosts)
	}
	if !r.Enforced {
		t.Error("a sealed reach must record enforced:true")
	}
}

// TestExecute_ToolStepUnsealedContractEnforcedFalse proves a contracted step
// with NO sealed entry (a directly-constructed plan; the verified load path
// refuses this shape) records its frontmatter hosts with enforced:false —
// the record never overclaims.
func TestExecute_ToolStepUnsealedContractEnforcedFalse(t *testing.T) {
	p := toolStepPlan()
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{"api.example.com"}})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}
	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.reaches) != 1 {
		t.Fatalf("reaches = %d, want 1", len(st.reaches))
	}
	if st.reaches[0].Enforced {
		t.Error("an unsealed contract must record enforced:false")
	}
	if strings.Join(st.reaches[0].Hosts, ",") != "api.example.com" {
		t.Errorf("hosts = %v, want the frontmatter hosts (the only reach that exists)", st.reaches[0].Hosts)
	}
}

// TestExecute_ToolStepNoContractCapturesNoReach proves a tool step with a nil
// trust contract captures zero reach entries: a step that declares no reach
// emits no record, never a synthesized empty contract.
func TestExecute_ToolStepNoContractCapturesNoReach(t *testing.T) {
	p := toolStepPlan()
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}
	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.reaches) != 0 {
		t.Fatalf("reaches = %d, want 0 for a step with no trust contract", len(st.reaches))
	}
}

// TestExecute_ToolStepReachCapturedDespiteRunnerError proves the reach is
// recorded even when the execution itself fails: the record is captured
// before the runner call, so a runner error (or nil runner) does not
// suppress it.
func TestExecute_ToolStepReachCapturedDespiteRunnerError(t *testing.T) {
	p := toolStepPlan()
	setExtractContract(p, &TrustContract{Effect: EffectExternalSend, Hosts: []string{"api.example.com"}})
	runner := &fakeToolStepRunner{forceErr: context.DeadlineExceeded}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil {
		t.Fatal("a tool-runner error must propagate")
	}
	if len(st.reaches) != 1 || st.reaches[0].StepID != "extract" {
		t.Fatalf("reaches = %+v, want the extract reach despite the error", st.reaches)
	}
}

// TestExecute_ToolStepReachCapturedDespiteNilRunner proves the reach is
// recorded before the nil-runner guard.
func TestExecute_ToolStepReachCapturedDespiteNilRunner(t *testing.T) {
	p := toolStepPlan()
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{"api.example.com"}})
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: nil}

	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil {
		t.Fatal("a tool step with no runner must error")
	}
	if len(st.reaches) != 1 {
		t.Fatalf("reaches = %d, want 1 despite the nil runner", len(st.reaches))
	}
}

// --- materialized-output provenance (#1762 retargeted to argv identity) ---

// toolMaterializePlan builds a single-step plan whose tool step (extract)
// materializes a declared file-map output. The runner returns a file-map
// carrier as the collected output, so the existing materialize path produces
// one artifact.
func toolMaterializePlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name:    "tool-plan",
		Actions: map[string]Action{},
		Outputs: map[string]Output{
			"extract.txt": {Name: "extract.txt", MimeType: "text/plain", Encoding: EncodingUTF8, Target: PublishFile, Path: "extract.txt"},
		},
		Inputs: []Input{
			{Name: "payload", Type: "string", Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: "hello"}},
		},
		Steps: []Step{
			{
				ID:                 "extract",
				Kind:               KindTool,
				Bindings:           map[string]Binding{"payload": mb("inputs.payload")},
				Outputs:            []string{"result"},
				MaterializesOutput: "extract.txt",
				Command:            []string{"extract-tool"},
				MountPath:          "/work/in",
				CollectPath:        "/work/out",
			},
		},
	}
	order, err := topoSort(p, map[string]int{"extract": 0})
	if err != nil {
		panic(err)
	}
	p.Order = order
	return p
}

// toolFileMap is the file-map carrier a tool "collects": the shape the
// materialize path expects for a utf-8 file output.
func toolFileMap(content string) map[string]any {
	return map[string]any{
		"path": "extract.txt", "mimeType": "text/plain", "encoding": "utf-8", "content": content,
	}
}

// TestExecute_ToolMaterializedOutputCarriesCommand proves a tool step that
// materializes an output captures a materializedOutput whose Command is the
// executed argv and whose Artifact.Digest is the collected-output digest.
func TestExecute_ToolMaterializedOutputCarriesCommand(t *testing.T) {
	p := toolMaterializePlan()
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": toolFileMap("collected-bytes\n")}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.outputs) != 1 {
		t.Fatalf("outputs = %d, want exactly 1 materialized output", len(st.outputs))
	}
	o := st.outputs[0]
	if o.StepKind != KindTool {
		t.Errorf("StepKind = %v, want tool", o.StepKind)
	}
	if strings.Join(o.Command, " ") != "extract-tool" {
		t.Errorf("Command = %v, want the executed argv", o.Command)
	}
	wantDigest := contentDigest([]byte("collected-bytes\n"))
	if o.Artifact.Digest != wantDigest {
		t.Errorf("Artifact.Digest = %q, want the collected-output digest %q", o.Artifact.Digest, wantDigest)
	}
}

// TestExecute_NonToolMaterializedOutputHasNilCommand is the regression guard
// that the command marker is tool-only: a plain transform materializing step
// captures a materializedOutput with a nil Command.
func TestExecute_NonToolMaterializedOutputHasNilCommand(t *testing.T) {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name:    "transform-plan",
		Actions: map[string]Action{},
		Outputs: map[string]Output{
			"out.txt": {Name: "out.txt", MimeType: "text/plain", Encoding: EncodingUTF8, Target: PublishFile, Path: "out.txt"},
		},
		Inputs: []Input{
			{Name: "payload", Type: "string", Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: "hi"}},
		},
		Steps: []Step{
			{
				ID:                 "render",
				Kind:               KindTransform,
				Transform:          "mk",
				Bindings:           map[string]Binding{"payload": mb("inputs.payload")},
				Outputs:            []string{"file"},
				MaterializesOutput: "out.txt",
			},
		},
	}
	order, err := topoSort(p, map[string]int{"render": 0})
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	p.Order = order

	reg := NewTransformRegistry()
	reg.Register("mk", func(_ map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{
			"path": "out.txt", "mimeType": "text/plain", "encoding": "utf-8", "content": "plain\n",
		}}, nil
	})
	x := &executor{plan: p, enforcer: &enforcer{}, transform: reg, toolRunner: nil}

	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hi"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.outputs) != 1 {
		t.Fatalf("outputs = %d, want 1", len(st.outputs))
	}
	if st.outputs[0].Command != nil {
		t.Errorf("Command = %v, want nil for a non-tool step", st.outputs[0].Command)
	}
}

// TestEmitAudit_ToolStepEmitsToolProvenance proves the full path emits
// exactly one output.materialized record for a tool step, carrying
// kind="tool", the executed argv, the collected-output content_hash, and the
// input walk-back — and NO aileron.step.image or aileron.step.calls.
func TestEmitAudit_ToolStepEmitsToolProvenance(t *testing.T) {
	p := toolMaterializePlan()
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": toolFileMap("collected-bytes\n")}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, _, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	sink := &recordingSink{}
	prov := launchProvenance{Skill: "tool-plan", ContentHash: "sha256:plan", InvocationID: "inv-1"}
	emitAudit(context.Background(), sink, st, prov)

	var out *AuditRecord
	for i := range sink.records {
		if sink.records[i].Kind == RecordKindOutput {
			if out != nil {
				t.Fatal("more than one output.materialized record emitted")
			}
			out = &sink.records[i]
		}
	}
	if out == nil {
		t.Fatal("no output.materialized record emitted for the tool step")
	}
	f := out.Fields
	if f["aileron.step.kind"] != "tool" {
		t.Errorf("step.kind = %v, want tool", f["aileron.step.kind"])
	}
	cmd, ok := f["aileron.step.command"].([]string)
	if !ok || strings.Join(cmd, " ") != "extract-tool" {
		t.Errorf("step.command = %v, want the executed argv", f["aileron.step.command"])
	}
	if f["aileron.output.content_hash"] != contentDigest([]byte("collected-bytes\n")) {
		t.Errorf("content_hash = %v, want the collected-output digest", f["aileron.output.content_hash"])
	}
	if _, present := f["aileron.step.inputs"]; !present {
		t.Error("tool step record must carry aileron.step.inputs (the input walk-back)")
	}
	if _, present := f["aileron.step.calls"]; present {
		t.Error("aileron.step.calls[] is out of scope (Half B) and must be absent")
	}
	if _, present := f["aileron.step.image"]; present {
		t.Error("aileron.step.image must be absent; the environment identity is the plan's single pin")
	}
}
