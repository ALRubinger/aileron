package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	_, err := runPlan(context.Background(), p, "h", Options{
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
	_, err := runPlan(context.Background(), p, "h", Options{
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
