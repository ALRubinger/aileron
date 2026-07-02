package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// rung3EnvManifest builds a raw manifest with a rung-3 executionEnvironment
// block plus the given inputs/steps, bypassing schema validation so the
// runtime's own decode guards are exercised directly. env is the untyped
// rung3PerStepImages value (a map[string]any mirroring the schema shape).
func rung3EnvManifest(env any, inputs, steps []any) *manifest.Manifest {
	return &manifest.Manifest{
		Name: "rung3-plan",
		Aileron: manifest.AileronBlock{
			SchemaVersion: "aileron.flightplan.v1",
			Requires: manifest.Requires{
				Actions:              []manifest.ActionRequirement{readActionReq()},
				ExecutionEnvironment: map[string]any{"rung3PerStepImages": env},
			},
			Inputs: inputs,
			Steps:  steps,
		},
	}
}

// digestA/digestB are valid content-addressed digests for tests.
const (
	digestA = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	digestB = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// fakeToolRunner records the spec it was handed and returns a canned collected
// output keyed by step id, so a test asserts the pinned image, the mounted
// input, and the collected output all flow correctly.
type fakeToolRunner struct {
	specs    []ToolRunSpec
	outputs  map[string]any // step id -> collected output
	forceErr error
}

func (f *fakeToolRunner) Run(_ context.Context, spec ToolRunSpec) (ToolRunResult, error) {
	f.specs = append(f.specs, spec)
	if f.forceErr != nil {
		return ToolRunResult{}, f.forceErr
	}
	return ToolRunResult{Output: f.outputs[spec.StepID]}, nil
}

// --- Unit 3: decode association ---

// TestDecodeToolDispatch_AttachesPinnedImage proves a rung-3 step decodes with
// its verified pinned image plus mount/collect paths attached, matched by step
// id to the verified pin, and that the attached image is the content-addressed
// ref@digest (never the mutable tag alone).
func TestDecodeToolDispatch_AttachesPinnedImage(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{
			"id":      "extract",
			"image":   "registry.example.com/tool-a:1",
			"mount":   map[string]any{"path": "/work"},
			"collect": map[string]any{"path": "/work/out"},
		},
	}}
	steps := []any{step(map[string]any{
		"id": "extract", "kind": "transform",
		"outputs": []any{"result"},
	})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "extract"}}

	p, err := DecodeWithImages(m, pins)
	if err != nil {
		t.Fatalf("DecodeWithImages: %v", err)
	}
	var found *Step
	for i := range p.Steps {
		if p.Steps[i].ID == "extract" {
			found = &p.Steps[i]
		}
	}
	if found == nil || found.ToolDispatch == nil {
		t.Fatalf("extract step must carry a tool dispatch, got %+v", p.Steps)
	}
	td := found.ToolDispatch
	if td.Image != "registry.example.com/tool-a:1@"+digestA {
		t.Errorf("dispatch image = %q, want the content-addressed ref@digest", td.Image)
	}
	if td.MountPath != "/work" || td.CollectPath != "/work/out" {
		t.Errorf("dispatch mount/collect = %q/%q", td.MountPath, td.CollectPath)
	}
}

// TestDecodeToolDispatch_MinimalNoMountCollect proves a rung-3 step declaring
// only an image (no mount/collect) decodes (minimal-first): absence of the
// optional blocks is handled, not assumed present.
func TestDecodeToolDispatch_MinimalNoMountCollect(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"id": "s1", "image": "registry.example.com/tool-a:1"},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "s1"}}

	p, err := DecodeWithImages(m, pins)
	if err != nil {
		t.Fatalf("DecodeWithImages: %v", err)
	}
	td := p.Steps[0].ToolDispatch
	if td == nil {
		t.Fatal("minimal rung-3 step must still carry a tool dispatch")
	}
	if td.MountPath != "" || td.CollectPath != "" {
		t.Errorf("minimal step must have empty mount/collect, got %q/%q", td.MountPath, td.CollectPath)
	}
}

// TestDecodeToolDispatch_UnpinnedImageErrors is the acceptance proof that an
// unpinned rung-3 image is refused: no tag-shaped or floating ref is dispatched.
// Here the step declares an image but no verified pin carries its id.
func TestDecodeToolDispatch_UnpinnedImageErrors(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"id": "s1", "image": "registry.example.com/tool-a:1"},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)

	// No pins at all: the step's image is unpinned.
	if _, err := DecodeWithImages(m, nil); err == nil {
		t.Fatal("a rung-3 step with no verified pin must be refused")
	} else if !strings.Contains(err.Error(), "unpinned") {
		t.Errorf("error should explain the pin is missing, got: %v", err)
	}
}

// TestDecodeToolDispatch_TagShapedDigestRefused proves defense in depth: a pin
// whose digest is a tag rather than a sha256: digest never survives into a
// dispatch, even if it somehow reached decode past the freeze guard.
func TestDecodeToolDispatch_TagShapedDigestRefused(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"id": "s1", "image": "registry.example.com/tool-a:1"},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: "latest", StepID: "s1"}}

	if _, err := DecodeWithImages(m, pins); err == nil {
		t.Fatal("a tag-shaped digest must be refused")
	} else if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error should explain the digest rule, got: %v", err)
	}
}

// TestDecodeToolDispatch_NoIDRefused proves a rung-3 step declaration with no
// id is refused: without an id there is no graph step to attach the dispatch to.
func TestDecodeToolDispatch_NoIDRefused(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"image": "registry.example.com/tool-a:1"},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "#0"}}

	if _, err := DecodeWithImages(m, pins); err == nil {
		t.Fatal("a rung-3 step with no id must be refused")
	}
}

// TestDecodeToolDispatch_UnknownStepRefused proves a rung-3 declaration whose id
// names no graph step is refused (dangling tool dispatch).
func TestDecodeToolDispatch_UnknownStepRefused(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"id": "ghost", "image": "registry.example.com/tool-a:1"},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "ghost"}}

	if _, err := DecodeWithImages(m, pins); err == nil {
		t.Fatal("a rung-3 step naming no graph step must be refused")
	}
}

// TestDecodeToolDispatch_ActionCallRefused proves a rung-3 dispatch attached to
// an action-call step is refused: a tool dispatch replaces in-process execution,
// so attaching it to an action-call would bypass the sealed action boundary
// (approval/idempotency/redaction/audit). Action effects must route through
// requires.actions, never a tool image.
func TestDecodeToolDispatch_ActionCallRefused(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"id": "call", "image": "registry.example.com/tool-a:1"},
	}}
	steps := []any{step(map[string]any{
		"id": "call", "kind": "action-call", "actionRef": "aileron:metrics.query_series",
		"outputs": []any{"out"},
	})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "call"}}
	if _, err := DecodeWithImages(m, pins); err == nil {
		t.Fatal("a tool dispatch on an action-call step must be refused")
	} else if !strings.Contains(err.Error(), "sealed action boundary") {
		t.Errorf("error should explain the boundary bypass, got: %v", err)
	}
}

// TestDecode_IgnoresPinsForNonRung3 proves a non-rung-3 plan ignores the pins
// entirely: no step gets a tool dispatch attached.
func TestDecode_IgnoresPinsForNonRung3(t *testing.T) {
	m := rawManifest(nil, nil, []any{
		step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}}),
	})
	pins := []freeze.ImagePin{{Ref: "registry.example.com/whole:1", Digest: digestA}}
	p, err := DecodeWithImages(m, pins)
	if err != nil {
		t.Fatalf("DecodeWithImages: %v", err)
	}
	if p.Steps[0].ToolDispatch != nil {
		t.Error("a non-rung-3 plan must not attach any tool dispatch")
	}
}

// --- #1775: per-step trust contract carried onto the dispatch ---

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

// TestDecodeToolDispatch_CarriesTrustContract proves a rung-3 step declaring a
// per-step trust contract decodes with ToolDispatch.TrustContract carrying the
// declared hosts and effect. The reach is CARRIED, not consumed: nothing here
// stamps it onto a host allow-list (#1769).
func TestDecodeToolDispatch_CarriesTrustContract(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{
			"id":            "extract",
			"image":         "registry.example.com/tool-a:1",
			"trustContract": trustContract("read", "api.example.com", "api.example.com:443"),
		},
	}}
	steps := []any{step(map[string]any{"id": "extract", "kind": "transform", "outputs": []any{"result"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "extract"}}

	p, err := DecodeWithImages(m, pins)
	if err != nil {
		t.Fatalf("DecodeWithImages: %v", err)
	}
	td := p.Steps[0].ToolDispatch
	if td == nil || td.TrustContract == nil {
		t.Fatalf("rung-3 step must carry a trust contract, got %+v", p.Steps[0])
	}
	if td.TrustContract.Effect != EffectRead {
		t.Errorf("carried effect = %q, want read", td.TrustContract.Effect)
	}
	if strings.Join(td.TrustContract.Hosts, ",") != "api.example.com,api.example.com:443" {
		t.Errorf("carried hosts = %v, want the declared reach", td.TrustContract.Hosts)
	}
}

// TestDecodeToolDispatch_NoTrustContractNil is the regression guard that the
// per-step contract is optional: a rung-3 step with no trustContract decodes
// with a nil TrustContract (declares no reach), never a zero-valued contract.
func TestDecodeToolDispatch_NoTrustContractNil(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"id": "s1", "image": "registry.example.com/tool-a:1"},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "s1"}}

	p, err := DecodeWithImages(m, pins)
	if err != nil {
		t.Fatalf("DecodeWithImages: %v", err)
	}
	td := p.Steps[0].ToolDispatch
	if td == nil {
		t.Fatal("step must still carry a tool dispatch")
	}
	if td.TrustContract != nil {
		t.Errorf("a step with no trust contract must leave TrustContract nil, got %+v", td.TrustContract)
	}
}

// TestDecodeToolDispatch_EmptyHostsRefused proves a declared per-step contract
// with no hosts is refused with the same access-scope message the action path
// emits (shared toContract validation).
func TestDecodeToolDispatch_EmptyHostsRefused(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{
			"id":            "s1",
			"image":         "registry.example.com/tool-a:1",
			"trustContract": trustContract("read"), // no hosts
		},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "s1"}}

	_, err := DecodeWithImages(m, pins)
	if err == nil {
		t.Fatal("a per-step trust contract with no hosts must be refused")
	}
	if !strings.Contains(err.Error(), "no hosts") {
		t.Errorf("error should cite the access-scope boundary, got: %v", err)
	}
}

// TestDecodeToolDispatch_UnknownEffectRefused proves a declared per-step
// contract with an unknown effect is refused, exactly like the action path.
func TestDecodeToolDispatch_UnknownEffectRefused(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{
			"id":            "s1",
			"image":         "registry.example.com/tool-a:1",
			"trustContract": trustContract("teleport", "api.example.com"),
		},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "s1"}}

	_, err := DecodeWithImages(m, pins)
	if err == nil {
		t.Fatal("a per-step trust contract with an unknown effect must be refused")
	}
	if !strings.Contains(err.Error(), "unknown effect") {
		t.Errorf("error should cite the unknown effect, got: %v", err)
	}
}

// --- Unit 4: executor dispatch ---

// toolDispatchPlan builds a two-step plan: a rung-3 tool step (extract) that
// mounts an input and collects an output, and a downstream transform (consume)
// that binds the collected output. It is the executor fixture for the
// mount → run → collect and dataflow proofs.
func toolDispatchPlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name:    "rung3-plan",
		Actions: map[string]Action{},
		Outputs: map[string]Output{},
		Inputs: []Input{
			{Name: "payload", Type: "string", Resolution: Resolution{Rule: ResolutionLiteral, HasDefault: true, Default: "hello"}},
		},
		Steps: []Step{
			{
				ID:   "extract",
				Kind: KindTransform,
				Bindings: map[string]Binding{
					"payload": mb("inputs.payload"),
				},
				Outputs: []string{"result"},
				ToolDispatch: &ToolDispatch{
					Image:       "registry.example.com/tool-a:1@" + digestA,
					MountPath:   "/work",
					CollectPath: "/work/out",
				},
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

// TestExecute_ToolDispatchMountsRunsCollects is acceptance proof (1): a pinned
// tool image runs with the resolved input mounted and the collected output read
// back. The fake tool runner asserts the digest, the mounted input, and the
// mount/collect paths; the executor threads the collected output into the step.
func TestExecute_ToolDispatchMountsRunsCollects(t *testing.T) {
	p := toolDispatchPlan()
	// The consume transform forwards the upstream binding so the collected
	// value is observable in the final step output.
	reg := NewTransformRegistry()
	runner := &fakeToolRunner{outputs: map[string]any{"extract": "COLLECTED"}}

	x := &executor{plan: p, enforcer: &enforcer{}, transform: reg, toolRunner: runner}
	inputs := ResolvedInputs{Values: map[string]any{"payload": "hello"}}
	st, err := x.execute(context.Background(), inputs)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(runner.specs) != 1 {
		t.Fatalf("tool runner must be called exactly once, got %d", len(runner.specs))
	}
	spec := runner.specs[0]
	// (a) the pinned image (digest) is handed to the runner verbatim.
	if spec.Image != "registry.example.com/tool-a:1@"+digestA {
		t.Errorf("runner got image %q, want the content-addressed pin", spec.Image)
	}
	// (b) the mount/collect paths flow through.
	if spec.MountPath != "/work" || spec.CollectPath != "/work/out" {
		t.Errorf("runner got mount/collect %q/%q", spec.MountPath, spec.CollectPath)
	}
	// (c) the resolved input is mounted.
	in, ok := spec.Input.(map[string]any)
	if !ok || in["payload"] != "hello" {
		t.Errorf("runner got mounted input %#v, want the resolved payload", spec.Input)
	}
	// The collected output becomes the step's declared output.
	if got := st.stepOutput["extract"]["result"]; got != "COLLECTED" {
		t.Errorf("collected output = %v, want COLLECTED", got)
	}
}

// TestExecute_CollectedOutputFlowsDownstream is acceptance proof (3): the
// collected output flows into a downstream step's dataflow through the existing
// binding path, with no new dataflow mechanism.
func TestExecute_CollectedOutputFlowsDownstream(t *testing.T) {
	p := toolDispatchPlan()
	reg := NewTransformRegistry()
	// The consume transform forwards its single upstream binding into "final".
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: b["upstream"]}, nil
	})
	// Default transform is identity-passthrough; make consume forward upstream.
	p.Steps[1].Transform = "identity"

	runner := &fakeToolRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: reg, toolRunner: runner}
	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := st.stepOutput["consume"]["final"]; got != "COLLECTED" {
		t.Errorf("downstream step must receive the collected output, got %v", got)
	}
}

// TestExecute_ToolDispatchNoRunnerErrors is acceptance proof (2b): a tool
// dispatch step with no configured runner is an explicit error, never a silent
// skip.
func TestExecute_ToolDispatchNoRunnerErrors(t *testing.T) {
	p := toolDispatchPlan()
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: nil}
	_, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil {
		t.Fatal("a tool dispatch with no runner must error")
	}
	if !strings.Contains(err.Error(), "no tool image runner is configured") {
		t.Errorf("error should name the missing runner, got: %v", err)
	}
}

// TestExecute_ToolDispatchMultipleOutputsErrors proves a tool-dispatch step
// declaring multiple outputs is refused: a single collected blob has no
// unambiguous mapping across several declared outputs.
func TestExecute_ToolDispatchMultipleOutputsErrors(t *testing.T) {
	p := toolDispatchPlan()
	// Give the extract step two declared outputs.
	p.Steps[0].Outputs = []string{"a", "b"}
	// Rewire consume to bind an output that still exists so decode-like shape
	// holds; the executor error must come from the dispatch, not the binding.
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p.Steps[1].Bindings = map[string]Binding{"upstream": mb("steps.extract.a")}
	order, _ := topoSort(p, map[string]int{"extract": 0, "consume": 1})
	p.Order = order

	runner := &fakeToolRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}
	_, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil || !strings.Contains(err.Error(), "exactly one collected output") {
		t.Fatalf("a multi-output tool dispatch must error, got %v", err)
	}
}

// TestDecodeToolDispatch_EmptyMountPathRefused proves a mount block with an
// empty path (a manifest tampered past the schema's minLength gate) is refused
// rather than mounting nothing.
func TestDecodeToolDispatch_EmptyMountPathRefused(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"id": "s1", "image": "registry.example.com/tool-a:1", "mount": map[string]any{"path": ""}},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "s1"}}
	if _, err := DecodeWithImages(m, pins); err == nil {
		t.Fatal("a mount block with an empty path must be refused")
	}
}

// TestDecodeToolDispatch_DuplicateStepIDRefused proves two rung-3 declarations
// with the same id are refused.
func TestDecodeToolDispatch_DuplicateStepIDRefused(t *testing.T) {
	env := map[string]any{"steps": []any{
		map[string]any{"id": "s1", "image": "registry.example.com/tool-a:1"},
		map[string]any{"id": "s1", "image": "registry.example.com/tool-b:2"},
	}}
	steps := []any{step(map[string]any{"id": "s1", "kind": "transform", "outputs": []any{"out"}})}
	m := rung3EnvManifest(env, nil, steps)
	pins := []freeze.ImagePin{{Ref: "registry.example.com/tool-a:1", Digest: digestA, StepID: "s1"}}
	if _, err := DecodeWithImages(m, pins); err == nil {
		t.Fatal("duplicate rung-3 step ids must be refused")
	}
}

// TestExecute_ToolRunnerErrorPropagates proves a tool-runner failure aborts the
// walk rather than being swallowed.
func TestExecute_ToolRunnerErrorPropagates(t *testing.T) {
	p := toolDispatchPlan()
	runner := &fakeToolRunner{forceErr: context.DeadlineExceeded}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}
	_, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil {
		t.Fatal("a tool-runner error must propagate")
	}
}

// setExtractContract attaches a per-step trust contract to the extract (rung-3)
// step of a toolDispatchPlan, so the reach-capture path (#1784) can be exercised.
func setExtractContract(p *Plan, tc *TrustContract) {
	p.Steps[0].ToolDispatch.TrustContract = tc
}

// --- Unit 1 (#1762): tool provenance threaded onto materializedOutput ---

// toolMaterializePlan builds a single-step rung-3 plan whose tool step (extract)
// materializes a declared file-map output. The tool runner returns a file-map
// carrier as the collected output, so the existing materialize path produces one
// artifact. It is the fixture for proving a tool-materialized output carries the
// pinned image (ToolImage) and the collected-output digest.
func toolMaterializePlan() *Plan {
	mb := func(s string) Binding { b, _ := ParseBinding(s); return b }
	p := &Plan{
		Name:    "rung3-plan",
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
				Kind:               KindTransform,
				Bindings:           map[string]Binding{"payload": mb("inputs.payload")},
				Outputs:            []string{"result"},
				MaterializesOutput: "extract.txt",
				ToolDispatch: &ToolDispatch{
					Image:       "registry.example.com/tool-a:1@" + digestA,
					MountPath:   "/work",
					CollectPath: "/work/out",
				},
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

// toolFileMap is the file-map carrier a tool image "collects": the shape the
// materialize path expects for a utf-8 file output.
func toolFileMap(content string) map[string]any {
	return map[string]any{
		"path": "extract.txt", "mimeType": "text/plain", "encoding": "utf-8", "content": content,
	}
}

// TestExecute_ToolMaterializedOutputCarriesImage proves a rung-3 tool step that
// materializes an output captures a materializedOutput whose ToolImage is the
// pinned ref@digest and whose Artifact.Digest is the collected-output digest.
// This is the discriminator the audit layer reads to emit kind="tool".
func TestExecute_ToolMaterializedOutputCarriesImage(t *testing.T) {
	p := toolMaterializePlan()
	runner := &fakeToolRunner{outputs: map[string]any{"extract": toolFileMap("collected-bytes\n")}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.outputs) != 1 {
		t.Fatalf("outputs = %d, want exactly 1 materialized output", len(st.outputs))
	}
	o := st.outputs[0]
	if o.ToolImage != "registry.example.com/tool-a:1@"+digestA {
		t.Errorf("ToolImage = %q, want the pinned ref@digest", o.ToolImage)
	}
	// The captured artifact digest is the digest of the collected output content.
	wantDigest := contentDigest([]byte("collected-bytes\n"))
	if o.Artifact.Digest != wantDigest {
		t.Errorf("Artifact.Digest = %q, want the collected-output digest %q", o.Artifact.Digest, wantDigest)
	}
}

// TestExecute_NonToolMaterializedOutputHasEmptyToolImage is the regression guard
// that the tool marker is tool-only: a plain transform materializing step
// captures a materializedOutput with ToolImage == "".
func TestExecute_NonToolMaterializedOutputHasEmptyToolImage(t *testing.T) {
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

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hi"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.outputs) != 1 {
		t.Fatalf("outputs = %d, want 1", len(st.outputs))
	}
	if st.outputs[0].ToolImage != "" {
		t.Errorf("ToolImage = %q, want empty for a non-tool step", st.outputs[0].ToolImage)
	}
}

// --- Unit 3 (#1762): end-to-end emit through the recording sink ---

// TestEmitAudit_ToolStepEmitsToolProvenance proves the full path emits exactly
// one output.materialized record for a rung-3 tool step, carrying kind="tool",
// the pinned image, the collected-output content_hash, and the input walk-back,
// and NO aileron.step.calls anywhere. The assertion is on the emitted record
// (the contract), not implementation internals.
func TestEmitAudit_ToolStepEmitsToolProvenance(t *testing.T) {
	p := toolMaterializePlan()
	runner := &fakeToolRunner{outputs: map[string]any{"extract": toolFileMap("collected-bytes\n")}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	sink := &recordingSink{}
	prov := launchProvenance{Skill: "rung3-plan", ContentHash: "sha256:plan", InvocationID: "inv-1"}
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
	if f["aileron.step.image"] != "registry.example.com/tool-a:1@"+digestA {
		t.Errorf("step.image = %v, want the pinned ref@digest", f["aileron.step.image"])
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
	if _, present := f["aileron.step.command"]; present {
		t.Error("aileron.step.command is not synthesized; the pin is the command identity")
	}
}

// --- Unit 2 (#1784): declared reach captured at rung-3 dispatch ---

// TestExecute_ToolDispatchCapturesDeclaredReach proves a rung-3 step whose
// ToolDispatch carries a trust contract captures exactly one st.reaches entry
// with the step id, declared effect, and declared hosts. The capture is
// audit-only; nothing is enforced.
func TestExecute_ToolDispatchCapturesDeclaredReach(t *testing.T) {
	p := toolDispatchPlan()
	setExtractContract(p, &TrustContract{
		Effect: EffectExternalSend,
		Hosts:  []string{"api.example.com", "cdn.example.com"},
	})
	runner := &fakeToolRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.reaches) != 1 {
		t.Fatalf("reaches = %d, want exactly 1", len(st.reaches))
	}
	r := st.reaches[0]
	if r.StepID != "extract" {
		t.Errorf("reach step id = %q, want extract", r.StepID)
	}
	if r.Effect != EffectExternalSend {
		t.Errorf("reach effect = %q, want external-send", r.Effect)
	}
	if len(r.Hosts) != 2 || r.Hosts[0] != "api.example.com" || r.Hosts[1] != "cdn.example.com" {
		t.Errorf("reach hosts = %v, want the two declared hosts", r.Hosts)
	}
}

// TestExecute_ToolDispatchNoContractCapturesNoReach proves a rung-3 step with a
// nil trust contract captures zero reach entries: a step that declares no reach
// emits no record, never a synthesized empty contract.
func TestExecute_ToolDispatchNoContractCapturesNoReach(t *testing.T) {
	p := toolDispatchPlan() // toolDispatchPlan's extract step has no TrustContract.
	runner := &fakeToolRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.reaches) != 0 {
		t.Fatalf("reaches = %d, want 0 for a step with no trust contract", len(st.reaches))
	}
}

// TestExecute_ToolDispatchEmptyHostsStillCaptures proves the reach guard keys on
// TrustContract != nil, not on a non-empty Hosts: a contract with a set effect
// but empty hosts still captures one entry.
func TestExecute_ToolDispatchEmptyHostsStillCaptures(t *testing.T) {
	p := toolDispatchPlan()
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: nil})
	runner := &fakeToolRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(st.reaches) != 1 {
		t.Fatalf("reaches = %d, want 1 even for empty hosts", len(st.reaches))
	}
	if st.reaches[0].Effect != EffectRead {
		t.Errorf("reach effect = %q, want read", st.reaches[0].Effect)
	}
	if len(st.reaches[0].Hosts) != 0 {
		t.Errorf("reach hosts = %v, want empty", st.reaches[0].Hosts)
	}
}

// TestExecute_ToolDispatchReachCapturedDespiteRunnerError proves the declared
// reach is recorded even when the dispatch itself fails: the declaration is
// captured before the runner call, so a runner error (or nil runner) does not
// suppress the reach record. The execState is returned alongside the error, so
// the captured entry is observable.
func TestExecute_ToolDispatchReachCapturedDespiteRunnerError(t *testing.T) {
	p := toolDispatchPlan()
	setExtractContract(p, &TrustContract{Effect: EffectExternalSend, Hosts: []string{"api.example.com"}})
	runner := &fakeToolRunner{forceErr: context.DeadlineExceeded}
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: runner}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil {
		t.Fatal("a tool-runner error must propagate")
	}
	if len(st.reaches) != 1 {
		t.Fatalf("reaches = %d, want 1 despite the dispatch error", len(st.reaches))
	}
	if st.reaches[0].StepID != "extract" {
		t.Errorf("reach step id = %q, want extract", st.reaches[0].StepID)
	}
}

// TestExecute_ToolDispatchReachCapturedDespiteNilRunner proves the reach is
// recorded before the nil-runner guard, so even a step that cannot dispatch
// still surfaces its declared reach for audit.
func TestExecute_ToolDispatchReachCapturedDespiteNilRunner(t *testing.T) {
	p := toolDispatchPlan()
	setExtractContract(p, &TrustContract{Effect: EffectRead, Hosts: []string{"api.example.com"}})
	x := &executor{plan: p, enforcer: &enforcer{}, transform: NewTransformRegistry(), toolRunner: nil}

	st, err := x.execute(context.Background(), ResolvedInputs{Values: map[string]any{"payload": "hello"}})
	if err == nil {
		t.Fatal("a tool dispatch with no runner must error")
	}
	if len(st.reaches) != 1 {
		t.Fatalf("reaches = %d, want 1 despite the nil runner", len(st.reaches))
	}
}
