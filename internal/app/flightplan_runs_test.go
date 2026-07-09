package app

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// TestFlightPlanRunRegistry_PutGetRoundTrip: a stored record is read back with
// its immutable re-launch state and accumulated outputs intact.
func TestFlightPlanRunRegistry_PutGetRoundTrip(t *testing.T) {
	reg := newFlightPlanRunRegistry()
	reg.Put("run-1", &flightPlanRunRecord{
		Name:    "plan",
		Version: "v1",
		Inputs:  runtime.LaunchArgs{"window": "7"},
		Outputs: map[string]map[string]any{"read": {"series": []any{1, 2}}},
	})

	rec, ok := reg.Get("run-1")
	if !ok {
		t.Fatal("Get(run-1) not found after Put")
	}
	if rec.Name != "plan" || rec.Version != "v1" {
		t.Errorf("name/version = %q/%q, want plan/v1", rec.Name, rec.Version)
	}
	if rec.Inputs["window"] != "7" {
		t.Errorf("inputs[window] = %v, want 7", rec.Inputs["window"])
	}
	if rec.Outputs["read"]["series"] == nil {
		t.Error("read.series output not round-tripped")
	}
	// Put normalizes nil maps so downstream writers never hit a nil map panic.
	if rec.Approvals == nil {
		t.Error("Approvals map should be non-nil after Put")
	}
}

// TestFlightPlanRunRegistry_MergeOutputsAccumulates: two suspends' memos merge
// last-write-wins per step, so a resume replays the full accumulated prefix.
func TestFlightPlanRunRegistry_MergeOutputsAccumulates(t *testing.T) {
	reg := newFlightPlanRunRegistry()
	reg.Put("run-2", &flightPlanRunRecord{
		Name:    "plan",
		Version: "v1",
		Outputs: map[string]map[string]any{"read": {"series": "s0"}},
	})

	// First suspend memo: adds seam_a, keeps read.
	if !reg.MergeOutputs("run-2", map[string]map[string]any{
		"read":   {"series": "s0"},
		"seam_a": {"summary": "S"},
	}) {
		t.Fatal("MergeOutputs on known run returned false")
	}
	// Second suspend memo: adds seam_b.
	reg.MergeOutputs("run-2", map[string]map[string]any{
		"read":   {"series": "s0"},
		"seam_a": {"summary": "S"},
		"seam_b": {"body": "B"},
	})

	rec, _ := reg.Get("run-2")
	for _, step := range []string{"read", "seam_a", "seam_b"} {
		if _, ok := rec.Outputs[step]; !ok {
			t.Errorf("accumulated memo missing step %q", step)
		}
	}
	if rec.Outputs["seam_b"]["body"] != "B" {
		t.Errorf("seam_b.body = %v, want B", rec.Outputs["seam_b"]["body"])
	}
}

// TestFlightPlanRunRegistry_MergeUnknownRun: merging into an unknown id reports
// not-found and creates nothing.
func TestFlightPlanRunRegistry_MergeUnknownRun(t *testing.T) {
	reg := newFlightPlanRunRegistry()
	if reg.MergeOutputs("ghost", map[string]map[string]any{"x": {"y": 1}}) {
		t.Error("MergeOutputs on unknown run should return false")
	}
	if _, ok := reg.Get("ghost"); ok {
		t.Error("MergeOutputs must not create a record for an unknown run")
	}
}

// TestFlightPlanRunRegistry_GetUnknown: an unknown run id is a clean miss.
func TestFlightPlanRunRegistry_GetUnknown(t *testing.T) {
	reg := newFlightPlanRunRegistry()
	if _, ok := reg.Get("nope"); ok {
		t.Error("Get on empty registry should miss")
	}
}

// TestFlightPlanRunRegistry_RecordApprovalAndDelete: an approval linkage is
// stored on the record, and Delete removes the whole run (a terminal outcome).
func TestFlightPlanRunRegistry_RecordApprovalAndDelete(t *testing.T) {
	reg := newFlightPlanRunRegistry()
	reg.Put("run-3", &flightPlanRunRecord{Name: "plan", Version: "v1"})
	reg.RecordApproval("run-3", "aileron:tracker.create_issue", "appr-9")

	rec, _ := reg.Get("run-3")
	if rec.Approvals["aileron:tracker.create_issue"] != "appr-9" {
		t.Errorf("approval linkage = %v, want appr-9", rec.Approvals)
	}

	reg.Delete("run-3")
	if _, ok := reg.Get("run-3"); ok {
		t.Error("Delete did not remove the run record")
	}
	// Delete is idempotent.
	reg.Delete("run-3")
}

// TestFlightPlanRunRegistry_NilSafety: the zero/nil registry tolerates every
// method (the daemon guards on a nil registry the same way it guards nil stores).
func TestFlightPlanRunRegistry_NilSafety(t *testing.T) {
	var reg *flightPlanRunRegistry
	reg.Put("x", &flightPlanRunRecord{})
	if _, ok := reg.Get("x"); ok {
		t.Error("nil registry Get should miss")
	}
	if reg.MergeOutputs("x", nil) {
		t.Error("nil registry MergeOutputs should be false")
	}
	reg.RecordApproval("x", "r", "id")
	reg.Delete("x")
}
