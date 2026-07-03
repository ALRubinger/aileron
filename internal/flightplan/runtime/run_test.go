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

// TestRun_BootsPinnedImageWhenLockPinsRung proves the boot branch end to end:
// the worked example is rung-2, so a store-backed Run delegates to the wired
// ImageRunner with the exact ref@digest from the verified lock and never walks
// the in-process pipeline.
func TestRun_BootsPinnedImageWhenLockPinsRung(t *testing.T) {
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	wantDigest := "sha256:" + strings.Repeat("a", 64)
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
		t.Fatal("Run did not delegate to the ImageRunner for a rung-pinned unit")
	}
	if !strings.HasSuffix(fake.spec.Image, "@"+wantDigest) {
		t.Errorf("booted image = %q, want it to end with @%s", fake.spec.Image, wantDigest)
	}
	if res.ContentHash != "sha256:booted" {
		t.Errorf("RunResult.ContentHash = %q, want the runner's result", res.ContentHash)
	}
}

// TestRun_PinnedButNoRunnerErrorsFromStore proves the guard fires end to end: a
// store-backed unit that pins a rung with no ImageRunner configured refuses,
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
		t.Fatal("a rung-pinned unit with no ImageRunner must refuse")
	}
	if !strings.Contains(err.Error(), "no image runner is configured") {
		t.Errorf("error = %q, want a no-runner-configured message", err.Error())
	}
}

// TestRun_NoRungStaysInProcess proves parity: a unit that pins no image never
// touches the ImageRunner and drives the in-process runPlan pipeline.
func TestRun_NoRungStaysInProcess(t *testing.T) {
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
// guard: a rung-pinned unit launched with InPinnedImage (the in-container
// re-entry, #1731) drives the in-process pipeline and never touches the
// ImageRunner. Without the guard the re-entry would boot the pin again and
// recurse — the failure mode is "no container runtime found on PATH" inside
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
