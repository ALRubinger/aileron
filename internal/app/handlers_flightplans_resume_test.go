package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	fpstore "github.com/ALRubinger/aileron/internal/flightplan/store"
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

// seamSourceBindingPlanMD is a one-seam plan whose seam binds BOTH a non-source
// step output (steps.read.series) AND a source-rule input (inputs.dataset,
// resolved from an action-call read). The seam sends the whole bindings map to
// the agent, but the flightplan.launch.seam audit record must exclude the
// source binding's inline dataset per the ADR-0027 audit boundary (#2119).
const seamSourceBindingPlanMD = `---
name: seam-source-fixture
description: A one-seam plan whose seam binds a source input plus a step output.
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
      - ref: aileron:metrics.load_dataset
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
    - name: dataset
      type: object
      description: A source-resolved dataset the seam binds.
      resolution:
        rule: source
        source:
          actionRef: aileron:metrics.load_dataset
          select: rows
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
      outputs:
        - series
    - id: summarize
      kind: llm-seam
      model: anthropic:claude-haiku-4-5
      bindings:
        series: steps.read.series
        dataset: inputs.dataset
      outputs:
        - summary
      materializesOutput: summary.json
---

# Seam Source Fixture
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

// TestResumeFlightPlan_UnconfiguredGuards: the resume endpoint fails closed when
// the store or executor is not configured (404 / 500), mirroring launch.
func TestResumeFlightPlan_UnconfiguredGuards(t *testing.T) {
	// No flight plan store → 404.
	noStore := &apiServer{
		log:            slog.New(slog.NewJSONHandler(io.Discard, nil)),
		flightPlanRuns: newFlightPlanRunRegistry(),
	}
	rec := resume(t, noStore, "run", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no-store status = %d, want 404", rec.Code)
	}

	// Store present but no executor → 500.
	fv := freezeFixture(t, oneSeamPlanMD)
	dir := t.TempDir()
	fps := fpstore.New(dir)
	if err := fps.WriteFrozen("one-seam-fixture", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	noExec := &apiServer{
		log:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		flightPlanStore: fps,
		flightPlanRuns:  newFlightPlanRunRegistry(),
	}
	rec = resume(t, noExec, "run", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("no-executor status = %d, want 500", rec.Code)
	}
}

// TestResumeFlightPlan_TrailingTokensBadBody: a body with trailing tokens after
// the first JSON value is a 400 (mirrors launch's strict-decode contract).
func TestResumeFlightPlan_TrailingTokensBadBody(t *testing.T) {
	fv := freezeFixture(t, oneSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"query_series": `{"series":[1]}`}}
	srv, _ := newFlightPlanTestServer(t, "one-seam-fixture", fv, exec)

	req := httptest.NewRequest(http.MethodPost, "/v1/flightplans/runs/x/resume",
		strings.NewReader(`{"outputs":{}} trailing`))
	rec := httptest.NewRecorder()
	srv.ResumeFlightPlan(rec, req, "x")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing-token status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestFlightPlanApprover_NoQueueFailsClosed: a gated action with no approval
// queue configured fails the run closed (403) rather than running unattended.
func TestFlightPlanApprover_NoQueueFailsClosed(t *testing.T) {
	fv := freezeFixture(t, writePlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"file_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-write-fixture", fv, exec)
	srv.actions = approvalGatedActionStore(t)
	// Remove the approval queue to exercise the fail-closed guard.
	srv.actionApprovals = nil

	rec := launch(t, srv, "daemon-write-fixture", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 fail-closed when no queue is configured; body=%s", rec.Code, rec.Body.String())
	}
	if got := countCalls(exec, "file_report"); got != 0 {
		t.Errorf("gated action ran %d times with no queue, want 0", got)
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
// run's audit trail carries exactly one flightplan.launch.seam record for the
// seam step, stamped with the seam's model hint and its non-source bindings, and
// each real step is recorded exactly once across the suspend/resume sequence (no
// double-audit).
func TestLaunchFlightPlan_SeamAuditRecordsModelAndBindings(t *testing.T) {
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
	var seamRecords []audit.Event
	for _, ev := range events {
		switch ev.EventType {
		case model.EventTypeFlightPlanLaunchAction:
			actionRecords++
		case model.EventTypeFlightPlanLaunch:
			launchSummaries++
		case model.EventTypeFlightPlanLaunchSeam:
			seamRecords = append(seamRecords, ev)
		}
	}
	if actionRecords != 1 {
		t.Errorf("per-action audit records = %d, want exactly 1 (no double-audit on replay)", actionRecords)
	}
	// Exactly one per-launch summary, emitted by the terminal (completing) resume.
	if launchSummaries != 1 {
		t.Errorf("per-launch summary records = %d, want exactly 1", launchSummaries)
	}
	// Exactly one flightplan.launch.seam record, carrying the model + bindings.
	if len(seamRecords) != 1 {
		t.Fatalf("seam audit records = %d, want exactly 1", len(seamRecords))
	}
	ev := seamRecords[0]
	if ev.Actor.Type != model.ActorTypeService || ev.Actor.ID != flightPlanLaunchActor {
		t.Errorf("seam record actor = %+v, want service/%s", ev.Actor, flightPlanLaunchActor)
	}
	if ev.Payload["aileron.seam.step_id"] != "summarize" {
		t.Errorf("seam step_id = %v, want summarize", ev.Payload["aileron.seam.step_id"])
	}
	if ev.Payload["aileron.seam.model"] != "anthropic:claude-haiku-4-5" {
		t.Errorf("seam model = %v, want anthropic:claude-haiku-4-5", ev.Payload["aileron.seam.model"])
	}
	binds, ok := ev.Payload["aileron.seam.bindings"].(map[string]any)
	if !ok {
		t.Fatalf("seam bindings = %v (%T), want map", ev.Payload["aileron.seam.bindings"], ev.Payload["aileron.seam.bindings"])
	}
	if _, present := binds["series"]; !present {
		t.Errorf("seam bindings missing the non-source BindStep binding %q; got %v", "series", binds)
	}
}

// countSeamAuditEvents returns the flightplan.launch.seam records grouped by
// their seam step id, so a test can assert one record per distinct seam step.
func countSeamAuditEvents(t *testing.T, store *audit.MemStore) map[string]int {
	t.Helper()
	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	byStep := map[string]int{}
	for _, ev := range events {
		if ev.EventType != model.EventTypeFlightPlanLaunchSeam {
			continue
		}
		stepID, _ := ev.Payload["aileron.seam.step_id"].(string)
		byStep[stepID]++
	}
	return byStep
}

// TestLaunchFlightPlan_SeamAuditNoDoubleOnEmptyResume: an empty-body resume while
// the same seam is still unfulfilled re-walks and re-suspends the same step, but
// the daemon's AuditedSeams stamp keeps exactly one flightplan.launch.seam record
// for that step (#2119).
func TestLaunchFlightPlan_SeamAuditNoDoubleOnEmptyResume(t *testing.T) {
	fv := freezeFixture(t, oneSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"query_series": `{"series":[1,2,3]}`}}
	srv, auditStore := newFlightPlanTestServer(t, "one-seam-fixture", fv, exec)

	seam := decodeSeam(t, launch(t, srv, "one-seam-fixture", nil))
	if got := countSeamAuditEvents(t, auditStore)["summarize"]; got != 1 {
		t.Fatalf("seam records after launch = %d, want 1", got)
	}

	// Resume with an empty body: the seam is still unfulfilled, so the run
	// re-suspends at the same step. No new seam record must be emitted.
	rec := resume(t, srv, seam.RunId, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty-body resume status = %d, want 200 seam_pending; body=%s", rec.Code, rec.Body.String())
	}
	reSeam := decodeSeam(t, rec)
	if reSeam.Seam.StepId != "summarize" {
		t.Fatalf("re-suspend step = %q, want summarize", reSeam.Seam.StepId)
	}
	if got := countSeamAuditEvents(t, auditStore)["summarize"]; got != 1 {
		t.Errorf("seam records after empty re-suspend = %d, want exactly 1 (no double-audit)", got)
	}
}

// TestLaunchFlightPlan_TwoSeamsAuditOnePerStep: a two-seam plan emits exactly one
// flightplan.launch.seam record per distinct seam step across the
// suspend→resume→suspend→resume sequence — both present, neither duplicated.
func TestLaunchFlightPlan_TwoSeamsAuditOnePerStep(t *testing.T) {
	fv := freezeFixture(t, twoSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"query_series": `{"series":[1,2,3]}`}}
	srv, auditStore := newFlightPlanTestServer(t, "two-seam-fixture", fv, exec)

	seam := decodeSeam(t, launch(t, srv, "two-seam-fixture", nil))
	runID := seam.RunId
	if seam.Seam.StepId != "seam_a" {
		t.Fatalf("first suspend step = %q, want seam_a", seam.Seam.StepId)
	}

	rec := resume(t, srv, runID, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{"seam_a": {"summary": "S"}},
	})
	seam = decodeSeam(t, rec)
	if seam.Seam.StepId != "seam_b" {
		t.Fatalf("second suspend step = %q, want seam_b", seam.Seam.StepId)
	}

	bodyFileMap := `{"path":"body.json","mimeType":"application/json","encoding":"utf-8","content":"{\"body\":\"done\"}"}`
	rec = resume(t, srv, runID, api.FlightPlanResumeRequest{
		Outputs: &map[string]map[string]interface{}{"seam_b": {"body": bodyFileMap}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("final resume status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	byStep := countSeamAuditEvents(t, auditStore)
	if byStep["seam_a"] != 1 {
		t.Errorf("seam_a records = %d, want exactly 1", byStep["seam_a"])
	}
	if byStep["seam_b"] != 1 {
		t.Errorf("seam_b records = %d, want exactly 1", byStep["seam_b"])
	}
	if len(byStep) != 2 {
		t.Errorf("distinct seam steps recorded = %d, want 2 (seam_a, seam_b); got %v", len(byStep), byStep)
	}
}

// TestLaunchFlightPlan_SeamAuditExcludesSourceBinding: a seam that binds a
// source-rule input inline-carries that dataset to the agent, but the
// flightplan.launch.seam record excludes the source binding (ADR-0027 audit
// boundary) while keeping the non-source step-output binding (#2119).
func TestLaunchFlightPlan_SeamAuditExcludesSourceBinding(t *testing.T) {
	fv := freezeFixture(t, seamSourceBindingPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{
		"query_series": `{"series":[1,2,3]}`,
		"load_dataset": `{"rows":[{"secret":"do-not-audit"}]}`,
	}}
	srv, auditStore := newFlightPlanTestServer(t, "seam-source-fixture", fv, exec)

	seam := decodeSeam(t, launch(t, srv, "seam-source-fixture", nil))
	if seam.Seam.StepId != "summarize" {
		t.Fatalf("suspend step = %q, want summarize", seam.Seam.StepId)
	}
	// The agent still receives the full bindings, source dataset included.
	if seam.Seam.Bindings == nil {
		t.Fatal("seam_pending must carry the full bindings to the agent")
	}
	if _, present := (*seam.Seam.Bindings)["dataset"]; !present {
		t.Errorf("agent bindings must include the source binding %q; got %v", "dataset", *seam.Seam.Bindings)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var seamRecords []audit.Event
	for _, ev := range events {
		if ev.EventType == model.EventTypeFlightPlanLaunchSeam {
			seamRecords = append(seamRecords, ev)
		}
	}
	if len(seamRecords) != 1 {
		t.Fatalf("seam audit records = %d, want exactly 1", len(seamRecords))
	}
	binds, ok := seamRecords[0].Payload["aileron.seam.bindings"].(map[string]any)
	if !ok {
		t.Fatalf("seam bindings = %v (%T), want map", seamRecords[0].Payload["aileron.seam.bindings"], seamRecords[0].Payload["aileron.seam.bindings"])
	}
	if _, present := binds["dataset"]; present {
		t.Errorf("seam audit bindings must EXCLUDE the source binding %q; got %v", "dataset", binds)
	}
	if _, present := binds["series"]; !present {
		t.Errorf("seam audit bindings must include the non-source binding %q; got %v", "series", binds)
	}
}

// TestLaunchFlightPlan_SeamAuditNilRecorder: a daemon with no configured audit
// recorder still suspends the seam cleanly and simply emits no seam record (the
// best-effort discipline: audit is a companion, never a hard dependency).
func TestLaunchFlightPlan_SeamAuditNilRecorder(t *testing.T) {
	fv := freezeFixture(t, oneSeamPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"query_series": `{"series":[1,2,3]}`}}
	srv, auditStore := newFlightPlanTestServer(t, "one-seam-fixture", fv, exec)
	srv.auditRecorder = nil

	seam := decodeSeam(t, launch(t, srv, "one-seam-fixture", nil))
	if seam.Seam.StepId != "summarize" {
		t.Fatalf("suspend step = %q, want summarize", seam.Seam.StepId)
	}
	if got := countSeamAuditEvents(t, auditStore); len(got) != 0 {
		t.Errorf("seam records with nil recorder = %v, want none", got)
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
