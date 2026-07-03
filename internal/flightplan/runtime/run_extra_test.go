package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

func TestSafeJoin_RefusesEscape(t *testing.T) {
	base := t.TempDir()
	cases := []string{"../escape.txt", "/etc/passwd", "a/../../escape.txt"}
	for _, c := range cases {
		if _, err := safeJoin(base, c); err == nil {
			t.Errorf("safeJoin(%q) must refuse an escaping path", c)
		}
	}
	// A nested relative path within base is allowed.
	if _, err := safeJoin(base, "sub/dir/report.csv"); err != nil {
		t.Errorf("safeJoin must allow a nested relative path: %v", err)
	}
}

func TestWriteArtifacts_RefusesEscapingArtifact(t *testing.T) {
	err := writeArtifacts(t.TempDir(), []Artifact{
		{Name: "evil", Path: "../escape.txt", Content: []byte("x"), Written: true},
	})
	if err == nil {
		t.Fatal("an artifact whose declared path escapes the output dir must be refused")
	}
}

func TestErrorMessages(t *testing.T) {
	if got := (&DenyError{ActionRef: "aileron:t.del", Reason: "no"}).Error(); !strings.Contains(got, "denied") {
		t.Errorf("DenyError = %q", got)
	}
	if got := (&DenyError{ActionRef: "aileron:t.del"}).Error(); !strings.Contains(got, "aileron:t.del") {
		t.Errorf("DenyError without reason = %q", got)
	}
	if got := (&LoadError{Reason: "bad"}).Error(); !strings.Contains(got, "bad") {
		t.Errorf("LoadError = %q", got)
	}
	if got := (&DecodeError{Reason: "bad"}).Error(); !strings.Contains(got, "bad") {
		t.Errorf("DecodeError = %q", got)
	}
}

// TestRun_InProcessPipelineFromLoadedPlan exercises the in-process pipeline
// from a frozen version on disk: load+verify then resolve+execute+materialize
// end-to-end. The worked example declares an environment, so the public Run now boots the
// pinned image (covered by TestRun_BootsPinnedImageWhenLockPinsEnvironment); this test
// drives the parity pipeline directly from the loaded plan so the resolve →
// execute → materialize path stays covered independent of the boot branch.
func TestRun_InProcessPipelineFromLoadedPlan(t *testing.T) {
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	lp, err := LoadVerified(s, "weekly-metrics-digest", "test")
	if err != nil {
		t.Fatalf("LoadVerified: %v", err)
	}

	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}

	res, err := runPlan(context.Background(), lp.Plan, lp.ContentHash, lp.SignerFingerprint, lp.StepTrust, Options{
		Dispatcher: disp,
		Approver:   &fakeApprover{decision: Decision{Approved: true}},
		Seam:       fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:      FixedClock{},
		Transforms: reg,
		OutDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if res.ContentHash == "" {
		t.Error("runPlan must surface the verified content hash")
	}
	if len(res.Artifacts) != 2 {
		t.Errorf("want 2 artifacts, got %d", len(res.Artifacts))
	}
}

func TestRun_LoadFailureSurfaces(t *testing.T) {
	s := store.New(t.TempDir())
	if _, err := Run(context.Background(), Options{Store: s, Name: "ghost", Version: "x"}); err == nil {
		t.Fatal("Run must surface a load/verify failure")
	}
}
