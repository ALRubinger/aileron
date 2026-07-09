package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

func TestRun_WritesArtifactsToOutDir(t *testing.T) {
	dir := t.TempDir()
	res, _, _ := runFixture(t, Options{OutDir: dir})
	if len(res.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts, got %d", len(res.Artifacts))
	}
	csv, err := os.ReadFile(filepath.Join(dir, "digest.csv"))
	if err != nil {
		t.Fatalf("read digest.csv: %v", err)
	}
	if string(csv) != "name\ncpu\n" {
		t.Errorf("digest.csv = %q", csv)
	}
	issue, err := os.ReadFile(filepath.Join(dir, "filed_issue.json"))
	if err != nil {
		t.Fatalf("read filed_issue.json: %v", err)
	}
	if string(issue) != `{"url":"https://tracker.example.com/issues/1"}` {
		t.Errorf("filed_issue.json = %q", issue)
	}
}

// TestRun_DeterminismProperty pins the resolved-input fixture and runs the
// full pipeline twice with a deterministic dispatcher, asserting byte-identical
// outputs (the behavioral-determinism property: same resolved inputs → same
// output).
func TestRun_DeterminismProperty(t *testing.T) {
	a, _, _ := runFixture(t, Options{Inputs: LaunchArgs{"window_days": 14}})
	b, _, _ := runFixture(t, Options{Inputs: LaunchArgs{"window_days": 14}})

	if len(a.Artifacts) != len(b.Artifacts) {
		t.Fatalf("artifact counts differ: %d vs %d", len(a.Artifacts), len(b.Artifacts))
	}
	for i := range a.Artifacts {
		if a.Artifacts[i].Name != b.Artifacts[i].Name {
			t.Errorf("artifact %d name differs", i)
		}
		if string(a.Artifacts[i].Content) != string(b.Artifacts[i].Content) {
			t.Errorf("artifact %q content not byte-identical across runs", a.Artifacts[i].Name)
		}
	}
	// Resolved inputs identical.
	if a.ResolvedInputs["window_days"] != b.ResolvedInputs["window_days"] {
		t.Error("resolved inputs differ across runs with identical launch args")
	}
}

// TestRun_StructuralNoLLMGuarantee proves a default launch (no seam provider)
// reaches no LLM: the llm-seam step errors, so a plan with an llm-seam refuses
// to run unless a seam is explicitly supplied. The action-call and transform
// branches never reach the seam.
func TestRun_StructuralNoLLMGuarantee(t *testing.T) {
	p := fixturePlan()
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "x", "mimeType": "text/csv"}}, nil
	})
	_, err := runPlan(context.Background(), p, "h", "sha256:signer", nil, Options{
		Dispatcher: &dispatchRouter{results: map[string]map[string]any{
			"aileron:metrics.query_series": {"series": []any{}},
		}},
		Approver:   &fakeApprover{decision: Decision{Approved: true}},
		Clock:      FixedClock{},
		Transforms: reg,
		// Seam intentionally nil: the v1 default.
	})
	if err == nil {
		t.Fatal("a plan with an llm-seam and no configured provider must refuse (no LLM by default)")
	}
}

// TestRun_SeamPlanOnNonAgentSurfaceFailsClosed proves the surface gate (#2102):
// a store-backed plan carrying an llm-seam step, launched through Run on a
// non-agent surface (no wired Seam, not suspendable), fails CLOSED before any
// step runs with the exact agent-context message. A dispatcher is wired so that
// any accidental step execution would be observable; the gate must fire first,
// so the dispatcher is never called.
func TestRun_SeamPlanOnNonAgentSurfaceFailsClosed(t *testing.T) {
	fv := frozenNoImage(t) // no-environment variant that keeps the summarize seam step
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	fake := &fakeImageRunner{}
	res, err := Run(context.Background(), Options{
		Store:       s,
		Name:        "no-exec-env",
		Version:     "test",
		ImageRunner: fake,
		Dispatcher:  disp,
		Approver:    &fakeApprover{decision: Decision{Approved: true}},
		// Seam intentionally nil and Suspendable unset: the non-agent surface.
		Clock:  FixedClock{},
		OutDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a seam plan on a non-agent surface must fail closed")
	}
	const want = "contains an llm-seam step; it can only be launched from an interactive agent context (aileron launch), not this surface"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
	// The gate is a static precondition: no step ran, so nothing dispatched and
	// no result was produced.
	if len(disp.calls) != 0 {
		t.Errorf("the gate must fire before any step runs; the dispatcher was invoked %d time(s)", len(disp.calls))
	}
	if fake.called {
		t.Error("a no-environment plan must not touch the ImageRunner")
	}
	if res.ContentHash != "" || len(res.Artifacts) != 0 {
		t.Errorf("a fail-closed run must produce no result: %+v", res)
	}
}

// TestRun_NoSeamPlanRunsOnNonAgentSurface proves the gate keys on seam presence,
// not on the absence of a wired Seam alone: a deterministic (no-seam) plan runs
// unchanged on the same non-agent surface (no Seam, not suspendable). This is
// the load-bearing "deterministic plans keep running everywhere" guarantee.
func TestRun_NoSeamPlanRunsOnNonAgentSurface(t *testing.T) {
	fv := frozenNoSeam(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-seam", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	res, err := Run(context.Background(), Options{
		Store:      s,
		Name:       "no-seam",
		Version:    "test",
		Dispatcher: disp,
		Approver:   &fakeApprover{decision: Decision{Approved: true}},
		// Seam nil and Suspendable unset: the same non-agent surface. A
		// deterministic plan must run here unchanged.
		Clock:      FixedClock{},
		Transforms: reg,
		OutDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("a deterministic plan must run on a non-agent surface: %v", err)
	}
	if !strings.HasPrefix(res.ContentHash, "sha256:") {
		t.Errorf("RunResult.ContentHash = %q, want the verified hash", res.ContentHash)
	}
}

// TestRun_SeamPlanWithWiredSeamRunsOnAgentSurface proves the gate is inert on an
// agent surface: the same seam plan, launched with a non-nil Seam (the CLI
// `aileron launch` wiring), runs to completion. The gate keys on provider
// absence, not merely on seam presence.
func TestRun_SeamPlanWithWiredSeamRunsOnAgentSurface(t *testing.T) {
	fv := frozenNoImage(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	res, err := Run(context.Background(), Options{
		Store:      s,
		Name:       "no-exec-env",
		Version:    "test",
		Dispatcher: disp,
		Approver:   &fakeApprover{decision: Decision{Approved: true}},
		Seam:       fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:      FixedClock{},
		Transforms: reg,
		OutDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("a seam plan with a wired Seam must run on an agent surface: %v", err)
	}
	if !strings.HasPrefix(res.ContentHash, "sha256:") {
		t.Errorf("RunResult.ContentHash = %q, want the verified hash", res.ContentHash)
	}
}

// TestRun_SeamPlanSuspendableStaysInert proves the gate does not fire on the
// daemon's suspendable surface: a seam plan with no wired Seam but Suspendable
// set SUSPENDS (SuspendKindSeam) for the agent to fulfill via resume, rather
// than failing closed. This is the third gate condition (`!opts.Suspendable`).
func TestRun_SeamPlanSuspendableStaysInert(t *testing.T) {
	fv := frozenNoImage(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	res, err := Run(context.Background(), Options{
		Store:       s,
		Name:        "no-exec-env",
		Version:     "test",
		Dispatcher:  disp,
		Approver:    &fakeApprover{decision: Decision{Approved: true}},
		Suspendable: true, // the daemon surface: unfulfilled seam suspends, not fails
		Clock:       FixedClock{},
		Transforms:  reg,
		OutDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("a suspendable seam launch must not fail closed: %v", err)
	}
	if res.Pending == nil {
		t.Fatal("a suspendable seam launch with no wired Seam must SUSPEND, not complete")
	}
	if res.Pending.Kind != SuspendKindSeam {
		t.Errorf("suspend kind = %v, want SuspendKindSeam", res.Pending.Kind)
	}
}

// TestRun_DeniedApprovalAbortsAndAudits proves a denied write mid-run aborts
// the run and the audit reflects the denial.
func TestRun_DeniedApprovalAbortsAndAudits(t *testing.T) {
	p := fixturePlan()
	sink := &recordingSink{}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "x", "mimeType": "text/csv"}}, nil
	})
	_, err := runPlan(context.Background(), p, "h", "sha256:signer", nil, Options{
		Dispatcher: &dispatchRouter{results: map[string]map[string]any{
			"aileron:metrics.query_series": {"series": []any{}},
			"aileron:tracker.create_issue": {"path": "filed_issue.json", "encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
		}},
		Approver:   &fakeApprover{decision: Decision{Approved: false, Reason: "operator declined"}},
		Audit:      sink,
		Seam:       fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:      FixedClock{},
		Transforms: reg,
	})
	if err == nil {
		t.Fatal("a denied write must abort the run")
	}
	var de *DenyError
	if !errors.As(err, &de) {
		t.Fatalf("error = %v, want *DenyError", err)
	}
	// The audit must include the denied write's record.
	found := false
	for _, rec := range sink.records {
		if rec.ActionRef == "aileron:tracker.create_issue" && rec.Fields["approval-decision"] == "denied" {
			found = true
		}
	}
	if !found {
		t.Error("the audit must record the denied approval decision")
	}
}

// TestRun_BootsPinnedImageWhenLockPinsEnvironment proves the boot branch end to end:
// the worked example declares tools, so freeze records a composed pin carrying a
// bootable local-daemon tag. A store-backed Run delegates to the wired
// ImageRunner with that local tag from the verified lock (#1856) and never walks
// the in-process pipeline.
func TestRun_BootsPinnedImageWhenLockPinsEnvironment(t *testing.T) {
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	// The verified lock's composed pin carries the bootable local tag; the boot
	// must hand that verbatim to the runner, not the unbootable ref@digest join.
	lp, err := verifyAndDecode(fv)
	if err != nil {
		t.Fatalf("verifyAndDecode: %v", err)
	}
	wantTag := lp.ResolvedImages[0].LocalTag
	if wantTag == "" || !strings.HasPrefix(wantTag, "aileron/sandbox-tools:") {
		t.Fatalf("expected a composed pin with an aileron/sandbox-tools: local tag, got %q", wantTag)
	}
	fake := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:booted"}}
	res, err := Run(context.Background(), Options{
		Store:       s,
		Name:        "weekly-metrics-digest",
		Version:     "test",
		ImageRunner: fake,
		// No Dispatcher/Approver/Seam: the boot path must not touch the
		// in-process pipeline, so leaving them nil proves runPlan is skipped.
	})
	if err != nil {
		t.Fatalf("Run boot path: %v", err)
	}
	if !fake.called {
		t.Fatal("Run did not delegate to the ImageRunner for an environment-pinned unit")
	}
	if fake.spec.Image != wantTag {
		t.Errorf("booted image = %q, want the composed pin's local tag %q", fake.spec.Image, wantTag)
	}
	if res.ContentHash != "sha256:booted" {
		t.Errorf("RunResult.ContentHash = %q, want the runner's result", res.ContentHash)
	}
}

// TestRun_PinnedButNoRunnerErrorsFromStore proves the guard fires end to end: a
// store-backed unit that pins an environment with no ImageRunner configured refuses,
// never silently falling back to the in-process path.
func TestRun_PinnedButNoRunnerErrorsFromStore(t *testing.T) {
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	_, err := Run(context.Background(), Options{
		Store:   s,
		Name:    "weekly-metrics-digest",
		Version: "test",
		// ImageRunner intentionally nil.
	})
	if err == nil {
		t.Fatal("an environment-pinned unit with no ImageRunner must refuse")
	}
	if !strings.Contains(err.Error(), "no image runner is configured") {
		t.Errorf("error = %q, want a no-runner-configured message", err.Error())
	}
}

// TestRun_NoEnvironmentStaysInProcess proves parity: a unit that pins no image never
// touches the ImageRunner and drives the in-process runPlan pipeline.
func TestRun_NoEnvironmentStaysInProcess(t *testing.T) {
	fv := frozenNoImage(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	fake := &fakeImageRunner{}
	res, err := Run(context.Background(), Options{
		Store:       s,
		Name:        "no-exec-env",
		Version:     "test",
		ImageRunner: fake,
		Dispatcher:  disp,
		Approver:    &fakeApprover{decision: Decision{Approved: true}},
		Seam:        fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:       FixedClock{},
		Transforms:  reg,
		OutDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run in-process path: %v", err)
	}
	if fake.called {
		t.Fatal("a unit that pins no image must not touch the ImageRunner")
	}
	if !strings.HasPrefix(res.ContentHash, "sha256:") {
		t.Errorf("in-process RunResult.ContentHash = %q, want the verified hash", res.ContentHash)
	}
}

// TestRun_InPinnedImageReentryStaysInProcess proves the image-boot re-entry
// guard: an environment-pinned unit launched with InPinnedImage (the in-container
// re-entry, #1731) drives the in-process pipeline and never touches the
// ImageRunner. Without the guard the re-entry would boot the pin again and
// recurse; the failure mode is "no container runtime found on PATH" inside
// the booted image.
func TestRun_InPinnedImageReentryStaysInProcess(t *testing.T) {
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	fake := &fakeImageRunner{}
	res, err := Run(context.Background(), Options{
		Store:         s,
		Name:          "weekly-metrics-digest",
		Version:       "test",
		ImageRunner:   fake,
		InPinnedImage: true,
		Dispatcher:    disp,
		Approver:      &fakeApprover{decision: Decision{Approved: true}},
		Seam:          fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:         FixedClock{},
		Transforms:    reg,
		OutDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run re-entry path: %v", err)
	}
	if fake.called {
		t.Fatal("the image-boot re-entry must not boot the pin again")
	}
	if !strings.HasPrefix(res.ContentHash, "sha256:") {
		t.Errorf("re-entry RunResult.ContentHash = %q, want the verified hash", res.ContentHash)
	}
}
