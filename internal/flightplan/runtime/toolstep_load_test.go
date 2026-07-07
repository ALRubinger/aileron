package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// toolStepSkillMD renders an inline, schema-valid SKILL.md with one
// contracted tool step. withEnvironment toggles the environment block so a
// test can freeze a pinned unit (the tool step's legal home) or an unpinned
// one (the Run-gate refusal fixture).
func toolStepSkillMD(withEnvironment bool) []byte {
	env := ""
	if withEnvironment {
		env = "  environment:\n    tools: [aws-cli@2.x]\n"
	}
	md := `---
name: tool-step-plan
description: Tool step load fixture.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential: { kind: none }
          hosts: [api.example.com]
          effect: read
          idempotency: { safeToRetry: true }
          audit: { fields: [result] }
` + env + `  inputs:
    - name: payload
      type: string
      resolution: { rule: literal, default: hello }
  outputs:
    - name: out.txt
      mimeType: text/plain
      encoding: utf-8
      publish: { target: none }
  steps:
    - id: extract
      kind: tool
      command: [extract-tool, --mode, csv]
      bindings: { payload: inputs.payload }
      trustContract:
        credential: { kind: none }
        hosts: [api.sealed.example.com]
        effect: read
        idempotency: { safeToRetry: true }
        audit: { fields: [result] }
      outputs: [result]
    - id: render
      kind: transform
      bindings: { upstream: steps.extract.result }
      outputs: [file]
      materializesOutput: out.txt
---
# Tool step fixture
`
	return []byte(md)
}

// frozenToolStep freezes the inline tool-step fixture and returns the
// store.FrozenVersion the runtime loads. Freeze seals the contracted step's
// reach into lock.stepTrust, so the verified unit carries both the graph
// step and its sealed entry.
func frozenToolStep(t *testing.T, withEnvironment bool) store.FrozenVersion {
	t.Helper()
	keyPath := writeSigningKey(t)
	opts := freeze.Options{
		Version:        "1.0.0",
		SigningKeyPath: keyPath,
	}
	if withEnvironment {
		opts.Resolver = freeze.DigestResolverFunc(func(_ context.Context, _ string) (string, error) {
			return "sha256:" + strings.Repeat("b", 64), nil
		})
		opts.Composer = freeze.FeatureComposerFunc(func(_ context.Context, _ string, _ []string) ([]freeze.PlatformDigest, error) {
			return []freeze.PlatformDigest{
				{OS: "linux", Arch: "amd64", Digest: "sha256:" + strings.Repeat("a", 64)},
				{OS: "linux", Arch: "arm64", Digest: "sha256:" + strings.Repeat("a", 64)},
			}, nil
		})
	}
	res, err := freeze.Run(context.Background(), toolStepSkillMD(withEnvironment), opts)
	if err != nil {
		t.Fatalf("freeze.Run tool-step fixture: %v", err)
	}
	return store.FrozenVersion{
		ID:        "test",
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}
}

// TestVerifyAndDecode_ToolStepThreadsSealedStepTrust proves the verified
// load path threads the lock's sealed stepTrust onto the LoadedPlan: a
// contracted tool step's sealed reach is exactly what freeze sealed from its
// declared contract, read from the verified lock (never re-supplied).
func TestVerifyAndDecode_ToolStepThreadsSealedStepTrust(t *testing.T) {
	lp, err := verifyAndDecode(frozenToolStep(t, true))
	if err != nil {
		t.Fatalf("verifyAndDecode: %v", err)
	}
	reach, ok := lp.StepTrust["extract"]
	if !ok {
		t.Fatalf("LoadedPlan.StepTrust missing the sealed extract entry: %+v", lp.StepTrust)
	}
	if strings.Join(reach.Hosts, ",") != "api.sealed.example.com" {
		t.Errorf("sealed hosts = %v, want the frozen contract's hosts", reach.Hosts)
	}
	// The decoded step carries the frontmatter contract for audit context.
	var tool *Step
	for i := range lp.Plan.Steps {
		if lp.Plan.Steps[i].Kind == KindTool {
			tool = &lp.Plan.Steps[i]
		}
	}
	if tool == nil || tool.TrustContract == nil {
		t.Fatal("the decoded tool step must carry its declared contract")
	}
}

// TestCrossCheckStepTrust_ContractedStepWithoutSealRefuses is the fail-closed
// load gate (#1829): a tool step that declares a trustContract but has no
// sealed stepTrust entry is an inconsistent frozen unit and must refuse.
// (freeze always seals a contracted step, so this shape cannot be produced
// through the public freeze API — the gate defends against a frozen unit
// produced by other means.)
func TestCrossCheckStepTrust_ContractedStepWithoutSealRefuses(t *testing.T) {
	plan := &Plan{Steps: []Step{{
		ID:            "extract",
		Kind:          KindTool,
		Command:       []string{"tool"},
		TrustContract: &TrustContract{Effect: EffectRead, Hosts: []string{"api.example.com"}},
	}}}
	err := crossCheckStepTrust(plan, nil)
	if err == nil {
		t.Fatal("a contracted tool step with no sealed reach must refuse")
	}
	if !strings.Contains(err.Error(), "seals no stepTrust reach") {
		t.Errorf("error = %v, want the inconsistent-frozen-unit refusal", err)
	}
	if _, ok := err.(*LoadError); !ok {
		t.Errorf("error type = %T, want *LoadError", err)
	}
}

// TestCrossCheckStepTrust_TolerantShapes proves the gate's tolerance rules:
// an uncontracted tool step needs no sealed entry, and a sealed entry with
// no matching contracted step is ignored (it grants nothing).
func TestCrossCheckStepTrust_TolerantShapes(t *testing.T) {
	plan := &Plan{Steps: []Step{
		{ID: "plain", Kind: KindTool, Command: []string{"tool"}},
		{ID: "render", Kind: KindTransform},
	}}
	sealed := map[string]freeze.StepReach{"ghost": {Hosts: []string{"x.example.com"}}}
	if err := crossCheckStepTrust(plan, sealed); err != nil {
		t.Fatalf("tolerant shapes must pass, got %v", err)
	}
}

// TestRun_ToolStepsWithoutPinnedEnvironmentRefuse is the Run gate (#1829): a
// frozen unit that declares tool steps but pins no environment image must
// refuse — the signed argv may only exec inside the declared environment.
func TestRun_ToolStepsWithoutPinnedEnvironmentRefuse(t *testing.T) {
	fv := frozenToolStep(t, false)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("tool-step-plan", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	_, err := Run(context.Background(), Options{
		Store:      s,
		Name:       "tool-step-plan",
		Version:    "test",
		ToolRunner: &fakeToolStepRunner{},
	})
	if err == nil {
		t.Fatal("a tool-step plan with no pinned environment must refuse")
	}
	if !strings.Contains(err.Error(), "pins no environment image") {
		t.Errorf("error = %v, want the no-environment refusal", err)
	}
}

// TestRun_ToolStepsBootPinnedEnvironment proves a pinned tool-step plan
// routes into the whole-plan image boot (one container; the tool step runs
// inside it on re-entry) rather than the in-process path.
func TestRun_ToolStepsBootPinnedEnvironment(t *testing.T) {
	fv := frozenToolStep(t, true)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("tool-step-plan", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	fake := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:booted"}}
	res, err := Run(context.Background(), Options{
		Store:       s,
		Name:        "tool-step-plan",
		Version:     "test",
		ImageRunner: fake,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !fake.called {
		t.Fatal("a pinned tool-step plan must boot the whole-plan image")
	}
	if res.ContentHash != "sha256:booted" {
		t.Errorf("ContentHash = %q, want the runner's result", res.ContentHash)
	}
}

// TestRun_InPinnedImageReentryRunsToolStepInProcess proves the re-entered
// in-container run (InPinnedImage) executes the tool step through the
// ToolStepRunner seam with the SEALED reach threaded — the end-to-end proof
// that the sealed lock, not the frontmatter, drives the scope the runner
// receives.
func TestRun_InPinnedImageReentryRunsToolStepInProcess(t *testing.T) {
	fv := frozenToolStep(t, true)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("tool-step-plan", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "x\n", "mimeType": "text/plain", "path": "out.txt"}}, nil
	})
	runner := &fakeToolStepRunner{outputs: map[string]any{"extract": "COLLECTED"}}
	fakeBoot := &fakeImageRunner{}
	res, err := Run(context.Background(), Options{
		Store:         s,
		Name:          "tool-step-plan",
		Version:       "test",
		ImageRunner:   fakeBoot,
		ToolRunner:    runner,
		InPinnedImage: true,
		Transforms:    reg,
		Clock:         FixedClock{},
		OutDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run re-entry: %v", err)
	}
	if fakeBoot.called {
		t.Fatal("the re-entry must not boot the pin again")
	}
	if len(runner.specs) != 1 {
		t.Fatalf("tool runner calls = %d, want 1", len(runner.specs))
	}
	if strings.Join(runner.specs[0].Hosts, ",") != "api.sealed.example.com" {
		t.Errorf("runner hosts = %v, want the SEALED reach from the verified lock", runner.specs[0].Hosts)
	}
	if got := res.StepOutputs["extract"]["result"]; got != "COLLECTED" {
		t.Errorf("step output = %v, want the collected value", got)
	}
}
