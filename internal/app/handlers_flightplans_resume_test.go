package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// oneSeamPlanMD is a plan with a read action-call feeding one llm-seam step that
// materializes the seam's output. The seam declares a sealed prompt and a
// recorded model so the seam_pending response can surface them (#2105).
const oneSeamPlanMD = `---
name: one-seam-fixture
description: A plan with a single llm-seam step the agent fulfills on resume.
license: Apache-2.0
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
            idempotencyKey: false
          audit:
            fields:
              - operation-effect
              - result
            sink: audit/reads
  inputs:
    - name: window_days
      type: number
      description: Window.
      resolution:
        rule: literal
        default: 7
  outputs:
    - name: summary.json
      mimeType: application/json
      encoding: utf-8
      publish:
        target: file
        path: summary.json
  steps:
    - id: read
      kind: action-call
      actionRef: aileron:metrics.query_series
      args:
        window_days: inputs.window_days
      outputs:
        - series
    - id: summarize
      kind: llm-seam
      prompt: "Summarize {{ steps.read.series }}"
      model: anthropic:claude-haiku-4-5
      bindings:
        series: steps.read.series
      outputs:
        - summary
      materializesOutput: summary.json
---

# One Seam Fixture
`

// twoSeamPlanMD chains two llm-seam steps after a read, so the run suspends
// twice (topological order read → seam_a → seam_b).
const twoSeamPlanMD = `---
name: two-seam-fixture
description: A plan with two chained llm-seam steps.
license: Apache-2.0
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
            idempotencyKey: false
          audit:
            fields:
              - operation-effect
              - result
            sink: audit/reads
  inputs:
    - name: window_days
      type: number
      description: Window.
      resolution:
        rule: literal
        default: 7
  outputs:
    - name: body.json
      mimeType: application/json
      encoding: utf-8
      publish:
        target: file
        path: body.json
  steps:
    - id: read
      kind: action-call
      actionRef: aileron:metrics.query_series
      args:
        window_days: inputs.window_days
      outputs:
        - series
    - id: seam_a
      kind: llm-seam
      bindings:
        series: steps.read.series
      outputs:
        - summary
    - id: seam_b
      kind: llm-seam
      bindings:
        summary: steps.seam_a.summary
      outputs:
        - body
      materializesOutput: body.json
---

# Two Seam Fixture
`

// mixedPlanMD chains a gated write action → a read action → an llm-seam, so a
// launch suspends first at the gate (202), then at the seam (seam_pending), then
// completes. The write is topologically before the read + seam, so a naive
// replay that re-ran the prefix would double the write effect: the exactly-once
// assertion guards Assumption B.
const mixedPlanMD = `---
name: mixed-fixture
description: A plan with a gated write, then a read, then an llm-seam.
license: Apache-2.0
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:tracker.file_report
        trustContract:
          credential:
            kind: none
          hosts:
            - tracker.example.com
          effect: write
          idempotency:
            safeToRetry: false
            idempotencyKey: true
          audit:
            fields:
              - operation-effect
              - approval-decision
              - result
            sink: audit/writes
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
          effect: read
          idempotency:
            safeToRetry: true
            idempotencyKey: false
          audit:
            fields:
              - operation-effect
              - result
            sink: audit/reads
  inputs: []
  outputs:
    - name: out.json
      mimeType: application/json
      encoding: utf-8
      publish:
        target: file
        path: out.json
  steps:
    - id: gated
      kind: action-call
      actionRef: aileron:tracker.file_report
      outputs:
        - ticket
    - id: read
      kind: action-call
      actionRef: aileron:metrics.query_series
      args:
        ticket: steps.gated.ticket
      outputs:
        - series
    - id: summarize
      kind: llm-seam
      bindings:
        series: steps.read.series
      outputs:
        - summary
      materializesOutput: out.json
---

# Mixed Fixture
`

// TestLaunchFlightPlan_MixedGateThenSeam: the mixed plan suspends at the gate
// first (202), then at the seam (seam_pending), then completes. The gated write
// dispatches exactly once across the whole sequence.
func TestLaunchFlightPlan_MixedGateThenSeam(t *testing.T) {
	fv := freezeFixture(t, mixedPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{
		"file_report":  `{"ticket":"T-1"}`,
		"query_series": `{"series":[1,2]}`,
	}}
	srv, _ := newFlightPlanTestServer(t, "mixed-fixture", fv, exec)
	srv.actions = approvalGatedActionStore(t)

	// First suspend: the gate (202).
	rec := launch(t, srv, "mixed-fixture", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var pending api.ActionRunPendingResponse
	if err := json.NewDecoder(rec.Body).Decode(&pending); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	if got := countCalls(exec, "file_report"); got != 0 {
		t.Fatalf("gated write must not run before approval; calls=%d", got)
	}
	runID := onlyRunID(t, srv)

	// Approve, then resume: the write dispatches, the read runs, and the run
	// suspends at the seam (seam_pending).
	if err := srv.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide(approve): %v", err)
	}
	rec = resume(t, srv, runID, nil)
	seam := decodeSeam(t, rec)
	if seam.Seam.StepId != "summarize" {
		t.Fatalf("second suspend step = %q, want summarize", seam.Seam.StepId)
	}
	if got := countCalls(exec, "file_report"); got != 1 {
		t.Fatalf("gated write dispatched %d times after approval, want 1", got)
	}

	// Fulfill the seam and resume: completes.
	outFileMap := `{"path":"out.json","mimeType":"application/json","encoding":"utf-8","content":"{\"summary\":\"ok\"}"}`
	rec = resume(t, srv, runID, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{"summarize": {"summary": outFileMap}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("final resume status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Exactly-once across the whole sequence: the gated write and the read each
	// dispatched exactly once despite two resumes re-walking the DAG.
	if got := countCalls(exec, "file_report"); got != 1 {
		t.Errorf("gated write dispatched %d times total, want exactly 1", got)
	}
	if got := countCalls(exec, "query_series"); got != 1 {
		t.Errorf("read dispatched %d times total, want exactly 1", got)
	}
	if _, ok := srv.flightPlanRuns.Get(runID); ok {
		t.Error("run record should be deleted after completion")
	}
}

// TestLaunchFlightPlan_OneSeamSuspendsThenResumes: launch suspends at the seam
// with a seam_pending 200 carrying the resolved prompt/model/outputs and the run
// id; resuming with the seam's outputs completes the run.
func TestLaunchFlightPlan_OneSeamSuspendsThenResumes(t *testing.T) {
	fv := freezeFixture(t, oneSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{
		"query_series": `{"series":[1,2,3]}`,
	}}
	srv, _ := newFlightPlanTestServer(t, "one-seam-fixture", fv, exec)

	rec := launch(t, srv, "one-seam-fixture", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("launch status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var seam api.FlightPlanSeamPendingResponse
	if err := json.NewDecoder(rec.Body).Decode(&seam); err != nil {
		t.Fatalf("decode seam_pending: %v", err)
	}
	if seam.Status != api.SeamPending {
		t.Errorf("status = %q, want seam_pending", seam.Status)
	}
	if seam.RunId == "" {
		t.Error("seam_pending must carry a run id")
	}
	if seam.Seam.StepId != "summarize" {
		t.Errorf("seam step id = %q, want summarize", seam.Seam.StepId)
	}
	if seam.Seam.Prompt == nil || *seam.Seam.Prompt != "Summarize {{ steps.read.series }}" {
		t.Errorf("seam prompt = %v, want the sealed template", seam.Seam.Prompt)
	}
	if seam.Seam.Model == nil || *seam.Seam.Model != "anthropic:claude-haiku-4-5" {
		t.Errorf("seam model = %v, want the recorded model", seam.Seam.Model)
	}
	if len(seam.Seam.Outputs) != 1 || seam.Seam.Outputs[0] != "summary" {
		t.Errorf("seam outputs = %v, want [summary]", seam.Seam.Outputs)
	}
	// The read ran exactly once so far.
	if got := countCalls(exec, "query_series"); got != 1 {
		t.Fatalf("read dispatched %d times before resume, want 1", got)
	}

	// Resume supplying the seam's declared output. The seam materializes a file
	// artifact, so its output value is a file-map JSON document (the same carrier
	// contract the action-call materialize path speaks).
	summaryFileMap := `{"path":"summary.json","mimeType":"application/json","encoding":"utf-8","content":"{\"summary\":\"ok\"}"}`
	rec = resume(t, srv, seam.RunId, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{
			"summarize": {"summary": summaryFileMap},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var done api.FlightPlanLaunchResponse
	if err := json.NewDecoder(rec.Body).Decode(&done); err != nil {
		t.Fatalf("decode completed: %v", err)
	}
	if done.StepOutputs == nil {
		t.Fatal("completed run must carry step outputs")
	}
	so := *done.StepOutputs
	if so["summarize"]["summary"] != summaryFileMap {
		t.Errorf("summarize.summary = %v, want the resumed value", so["summarize"]["summary"])
	}
	// The read still ran exactly once total (memoized replay, no re-dispatch).
	if got := countCalls(exec, "query_series"); got != 1 {
		t.Errorf("read dispatched %d times total, want exactly 1", got)
	}
	if _, ok := srv.flightPlanRuns.Get(seam.RunId); ok {
		t.Error("run record should be deleted after completion")
	}
}

// TestLaunchFlightPlan_TwoSeamsResumeInOrder: a two-seam plan suspends at seam_a,
// resumes, suspends at seam_b, resumes, then completes. The read runs once total.
func TestLaunchFlightPlan_TwoSeamsResumeInOrder(t *testing.T) {
	fv := freezeFixture(t, twoSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{
		"query_series": `{"series":[1,2,3]}`,
	}}
	srv, _ := newFlightPlanTestServer(t, "two-seam-fixture", fv, exec)

	rec := launch(t, srv, "two-seam-fixture", nil)
	seam := decodeSeam(t, rec)
	if seam.Seam.StepId != "seam_a" {
		t.Fatalf("first suspend step = %q, want seam_a", seam.Seam.StepId)
	}
	runID := seam.RunId

	rec = resume(t, srv, runID, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{"seam_a": {"summary": "S"}},
	})
	seam = decodeSeam(t, rec)
	if seam.Seam.StepId != "seam_b" {
		t.Fatalf("second suspend step = %q, want seam_b", seam.Seam.StepId)
	}
	if seam.RunId != runID {
		t.Errorf("run id changed across resume: %q vs %q", seam.RunId, runID)
	}

	bodyFileMap := `{"path":"body.json","mimeType":"application/json","encoding":"utf-8","content":"{\"body\":\"done\"}"}`
	rec = resume(t, srv, runID, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{"seam_b": {"body": bodyFileMap}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("final resume status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var done api.FlightPlanLaunchResponse
	if err := json.NewDecoder(rec.Body).Decode(&done); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if (*done.StepOutputs)["seam_b"]["body"] != bodyFileMap {
		t.Errorf("seam_b.body = %v, want the materialized file-map", (*done.StepOutputs)["seam_b"]["body"])
	}
	if got := countCalls(exec, "query_series"); got != 1 {
		t.Errorf("read dispatched %d times total across two resumes, want 1", got)
	}
}

// TestResumeFlightPlan_MissingSeamOutputs400: resuming a seam run with outputs
// that omit the declared output (or name a non-seam step) is a 400 before replay.
func TestResumeFlightPlan_MissingSeamOutputs400(t *testing.T) {
	fv := freezeFixture(t, oneSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"query_series": `{"series":[1]}`}}
	srv, _ := newFlightPlanTestServer(t, "one-seam-fixture", fv, exec)

	seam := decodeSeam(t, launch(t, srv, "one-seam-fixture", nil))

	// Missing the declared "summary" output.
	rec := resume(t, srv, seam.RunId, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{"summarize": {"wrong": "x"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// Naming a non-seam step is also a 400.
	rec = resume(t, srv, seam.RunId, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{"read": {"series": "x"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-seam step status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// The run record survives a rejected resume so the agent can retry correctly.
	if _, ok := srv.flightPlanRuns.Get(seam.RunId); !ok {
		t.Error("run record should survive a 400 resume")
	}
}

// TestResumeFlightPlan_VaultLockedAndBadBody: the resume endpoint enforces the
// same guards as launch (412 vault-locked) and rejects a malformed body (400).
func TestResumeFlightPlan_GuardPaths(t *testing.T) {
	fv := freezeFixture(t, oneSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"query_series": `{"series":[1]}`}}
	srv, _ := newFlightPlanTestServer(t, "one-seam-fixture", fv, exec)

	// Vault locked → 412, before any registry lookup.
	srv.vaultLocked = true
	rec := resume(t, srv, "any", nil)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("vault-locked status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
	srv.vaultLocked = false

	// Malformed body → 400 (before the registry lookup, so any run id works).
	req := httptest.NewRequest(http.MethodPost, "/v1/flightplans/runs/x/resume", strings.NewReader("{ bad json"))
	badRec := httptest.NewRecorder()
	srv.ResumeFlightPlan(badRec, req, "x")
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400; body=%s", badRec.Code, badRec.Body.String())
	}
}

// TestResumeFlightPlan_StillPendingReSuspends: resuming a gated run BEFORE the
// user has decided re-suspends idempotently (another 202) without re-registering
// or dispatching the action.
func TestResumeFlightPlan_StillPendingReSuspends(t *testing.T) {
	fv := freezeFixture(t, writePlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"file_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-write-fixture", fv, exec)
	srv.actions = approvalGatedActionStore(t)

	rec := launch(t, srv, "daemon-write-fixture", nil)
	var pending api.ActionRunPendingResponse
	_ = json.NewDecoder(rec.Body).Decode(&pending)
	runID := onlyRunID(t, srv)

	// Resume without deciding: still pending → another 202, same approval id.
	rec = resume(t, srv, runID, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("re-suspend status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var pending2 api.ActionRunPendingResponse
	if err := json.NewDecoder(rec.Body).Decode(&pending2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pending2.ApprovalId != pending.ApprovalId {
		t.Errorf("re-suspend minted a new approval id %q, want the same %q (no re-register)", pending2.ApprovalId, pending.ApprovalId)
	}
	if got := countCalls(exec, "file_report"); got != 0 {
		t.Errorf("action dispatched %d times while still pending, want 0", got)
	}
	// Exactly one queue entry exists for this run.
	if _, ok := srv.actionApprovals.Outcome(pending.ApprovalId); !ok {
		t.Error("the original approval entry should still exist")
	}
}

// TestResumeFlightPlan_UnknownRun404: resuming an unknown/expired run id is 404.
func TestResumeFlightPlan_UnknownRun404(t *testing.T) {
	fv := freezeFixture(t, oneSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"query_series": `{"series":[1]}`}}
	srv, _ := newFlightPlanTestServer(t, "one-seam-fixture", fv, exec)

	rec := resume(t, srv, "no-such-run", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestResumeFlightPlan_DeniedApprovalFailsClosed: launch → 202 → deny → resume
// → 403 fail-closed; the run record is deleted and the write never dispatched.
func TestResumeFlightPlan_DeniedApprovalFailsClosed(t *testing.T) {
	fv := freezeFixture(t, writePlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"file_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-write-fixture", fv, exec)
	srv.actions = approvalGatedActionStore(t)

	rec := launch(t, srv, "daemon-write-fixture", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var pending api.ActionRunPendingResponse
	if err := json.NewDecoder(rec.Body).Decode(&pending); err != nil {
		t.Fatalf("decode: %v", err)
	}
	runID := onlyRunID(t, srv)

	if err := srv.actionApprovals.Decide(pending.ApprovalId, false, "not this time", nil); err != nil {
		t.Fatalf("Decide(deny): %v", err)
	}

	rec = resume(t, srv, runID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("resume status = %d, want 403 fail-closed; body=%s", rec.Code, rec.Body.String())
	}
	if got := countCalls(exec, "file_report"); got != 0 {
		t.Errorf("denied action dispatched %d times, want 0", got)
	}
	if _, ok := srv.flightPlanRuns.Get(runID); ok {
		t.Error("run record must be deleted after a denied fail-closed run")
	}
}

// TestLaunchFlightPlan_SeamAuditRecordsModelAndBindings (R8): a completed seam
// run's audit trail carries the seam step's outputs, and each real step is
// recorded exactly once across the suspend/resume sequence (no double-audit).
func TestLaunchFlightPlan_SeamAuditNoDoubleRecord(t *testing.T) {
	fv := freezeFixture(t, oneSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"query_series": `{"series":[1,2,3]}`}}
	srv, auditStore := newFlightPlanTestServer(t, "one-seam-fixture", fv, exec)

	seam := decodeSeam(t, launch(t, srv, "one-seam-fixture", nil))
	summaryFileMap := `{"path":"summary.json","mimeType":"application/json","encoding":"utf-8","content":"{\"summary\":\"ok\"}"}`
	rec := resume(t, srv, seam.RunId, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{"summarize": {"summary": summaryFileMap}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The read action-call is recorded exactly once despite the resume re-walking
	// the DAG (memoized replay re-audits nothing).
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	actionRecords := 0
	launchSummaries := 0
	for _, ev := range events {
		switch ev.EventType {
		case model.EventTypeFlightPlanLaunchAction:
			actionRecords++
		case model.EventTypeFlightPlanLaunch:
			launchSummaries++
		}
	}
	if actionRecords != 1 {
		t.Errorf("per-action audit records = %d, want exactly 1 (no double-audit on replay)", actionRecords)
	}
	// Exactly one per-launch summary, emitted by the terminal (completing) resume.
	if launchSummaries != 1 {
		t.Errorf("per-launch summary records = %d, want exactly 1", launchSummaries)
	}
}

// decodeSeam asserts a 200 seam_pending response and decodes it.
func decodeSeam(t *testing.T, rec interface{ Result() *http.Response }) api.FlightPlanSeamPendingResponse {
	t.Helper()
	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 seam_pending", resp.StatusCode)
	}
	var seam api.FlightPlanSeamPendingResponse
	if err := json.NewDecoder(resp.Body).Decode(&seam); err != nil {
		t.Fatalf("decode seam_pending: %v", err)
	}
	if seam.Status != api.SeamPending {
		t.Fatalf("status = %q, want seam_pending", seam.Status)
	}
	return seam
}
