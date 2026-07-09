package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	fpstore "github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/model"
)

// deterministicPlanMD is a minimal, no-environment, no-llm-seam Flight Plan: a
// single read action-call that materializes a JSON artifact. With no
// `environment` block, freeze pins no image, so the daemon's in-process path
// runs it to completion (the deterministic happy path #2097 targets). The
// action's output is a file-map, so it materializes a real artifact without a
// custom transform.
const deterministicPlanMD = `---
name: daemon-launch-fixture
description: A deterministic single-read plan that materializes a JSON artifact.
license: Apache-2.0
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:reporter.build_report
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
              - approval-decision
              - result
            sink: audit/reads
  inputs:
    - name: window_days
      type: number
      description: How many days back the report window covers.
      resolution:
        rule: literal
        default: 7
  outputs:
    - name: report.json
      mimeType: application/json
      encoding: utf-8
      publish:
        target: file
        path: report.json
  steps:
    - id: build
      kind: action-call
      actionRef: aileron:reporter.build_report
      args:
        window_days: inputs.window_days
      outputs:
        - report
      materializesOutput: report.json
---

# Daemon Launch Fixture

A deterministic single-read plan used by the daemon launch endpoint tests.
`

// writePlanMD is a plan whose single action-call has a WRITE effect, so the
// runtime routes it through the approver and the daemon dispatcher's
// approval-gate manifest check fires. Used to exercise the approval-refusal path.
const writePlanMD = `---
name: daemon-write-fixture
description: A single write action-call plan used to exercise approval refusal.
license: Apache-2.0
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:reporter.file_report
        trustContract:
          credential:
            kind: none
          hosts:
            - api.example.com
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
  inputs: []
  outputs:
    - name: report.json
      mimeType: application/json
      encoding: utf-8
      publish:
        target: file
        path: report.json
  steps:
    - id: file
      kind: action-call
      actionRef: aileron:reporter.file_report
      outputs:
        - report
      materializesOutput: report.json
---

# Daemon Write Fixture

A single write action-call plan used by the approval-refusal test.
`

// approvalGatedActionManifest declares the write action reached by writePlanMD
// (daemon action name file_report) with [approval] required = true.
const approvalGatedActionManifest = `+++
name = "file_report"
version = "1.0.0"
source = "hub://aileron/file-report@1.0.0"

[[requires.connectors]]
name = "github://aileron/tracker"
version = "1.0.0"
hash = "sha256:abc123"
capabilities = ["issues:write"]

[match]
intent = "file a report"

[[execute]]
id = "post"
connector = "github://aileron/tracker"
op = "create_issue"

[approval]
required = true
+++

# File Report
`

// approvalGatedActionStore builds an action.Store carrying the approval-gated
// file_report manifest.
func approvalGatedActionStore(t *testing.T) *action.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file_report.md"), []byte(approvalGatedActionManifest), 0o644); err != nil {
		t.Fatalf("write action manifest: %v", err)
	}
	s := action.NewStore(dir)
	if _, err := s.Load(); err != nil {
		t.Fatalf("action store Load: %v", err)
	}
	return s
}

// reportFileMap is the file-map JSON the fixture executor returns for
// aileron:reporter.build_report. The whole result is the step's single declared
// output; materialize decodes it as a file-map and writes report.json.
const reportFileMap = `{"path":"report.json","mimeType":"application/json","encoding":"utf-8","content":"{\"window\":7}"}`

// flightplanRecordingExecutor is an in-mem action.Executor stub: it records each call and
// returns a per-action canned Result. It stands in for the daemon's real
// SandboxExecutor so the launch handler's in-process dispatcher can be driven
// without a WASM runtime.
type flightplanRecordingExecutor struct {
	results  map[string]string // action name → Content JSON
	failures map[string]*failure.Failure
	calls    []flightplanRecordedCall
}

type flightplanRecordedCall struct {
	name string
	args map[string]any
}

func (e *flightplanRecordingExecutor) Execute(_ context.Context, name string, args map[string]any) (action.Result, error) {
	e.calls = append(e.calls, flightplanRecordedCall{name: name, args: args})
	if f, ok := e.failures[name]; ok {
		return action.Result{Failure: f}, nil
	}
	return action.Result{Content: e.results[name]}, nil
}

// writeSigningKey writes a fresh ed25519 PKCS#8 PEM to a temp file for freeze.
func writeSigningKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "signing-key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return keyPath
}

// freezeFixture freezes the given manifest bytes into a store.FrozenVersion the
// runtime can load. The manifest declares no environment, so freeze needs no
// resolver/composer and pins no image.
func freezeFixture(t *testing.T, md string) fpstore.FrozenVersion {
	t.Helper()
	res, err := freeze.Run(context.Background(), []byte(md), freeze.Options{
		Version:        "1.0.0",
		SigningKeyPath: writeSigningKey(t),
	})
	if err != nil {
		t.Fatalf("freeze.Run: %v", err)
	}
	return fpstore.FrozenVersion{
		ID:        "test",
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}
}

// newFlightPlanTestServer builds an apiServer wired with a temp Flight Plan
// store containing the given frozen plan under planName, an in-mem audit
// store+recorder, and the supplied executor. Returns the server and the audit
// store so tests can assert emitted records.
func newFlightPlanTestServer(t *testing.T, planName string, fv fpstore.FrozenVersion, exec *flightplanRecordingExecutor) (*apiServer, *audit.MemStore) {
	t.Helper()
	dir := t.TempDir()
	s := fpstore.New(dir)
	if err := s.WriteFrozen(planName, fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	auditStore := audit.NewMemStore()
	recorder := audit.NewRecorder(auditStore, nil, nil)
	return &apiServer{
		log:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		flightPlanStore: s,
		executor:        exec,
		auditStore:      auditStore,
		auditRecorder:   recorder,
		newID:           func() string { return "test-id" },
	}, auditStore
}

// launch POSTs a launch request body (or nil for an empty body) and returns the
// recorder.
func launch(t *testing.T, srv *apiServer, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/flightplans/"+name+"/launch", reader)
	rec := httptest.NewRecorder()
	srv.LaunchFlightPlan(rec, req, name)
	return rec
}

func TestLaunchFlightPlan_HappyPath200(t *testing.T) {
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": reportFileMap}}
	srv, auditStore := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)

	rec := launch(t, srv, "daemon-launch-fixture", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.FlightPlanLaunchResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ContentHash == "" || !strings.HasPrefix(got.ContentHash, "sha256:") {
		t.Errorf("content_hash = %q, want sha256:...", got.ContentHash)
	}
	// The read action was dispatched through the in-process executor.
	if len(exec.calls) != 1 || exec.calls[0].name != "build_report" {
		t.Fatalf("executor calls = %+v, want one build_report", exec.calls)
	}
	// The default input flows through to resolved_inputs.
	if got.ResolvedInputs == nil {
		t.Fatal("resolved_inputs missing")
	}
	if v := (*got.ResolvedInputs)["window_days"]; v != float64(7) {
		t.Errorf("resolved_inputs[window_days] = %v (%T), want 7", v, v)
	}
	// The step output is surfaced.
	if got.StepOutputs == nil {
		t.Fatal("step_outputs missing")
	}
	if _, ok := (*got.StepOutputs)["build"]; !ok {
		t.Errorf("step_outputs missing step %q; got %v", "build", got.StepOutputs)
	}
	// The artifact materialized with the declared name/path/mime and a digest.
	if got.Artifacts == nil || len(*got.Artifacts) != 1 {
		t.Fatalf("artifacts = %v, want 1", got.Artifacts)
	}
	art := (*got.Artifacts)[0]
	if art.Name != "report.json" {
		t.Errorf("artifact name = %q", art.Name)
	}
	if art.Path == nil || *art.Path != "report.json" {
		t.Errorf("artifact path = %v", art.Path)
	}
	if art.MimeType == nil || *art.MimeType != "application/json" {
		t.Errorf("artifact mime = %v", art.MimeType)
	}
	if !strings.HasPrefix(art.Digest, "sha256:") {
		t.Errorf("artifact digest = %q", art.Digest)
	}
	// Audit ids returned and the launch + action records landed in the store.
	if got.AuditIds == nil || len(*got.AuditIds) == 0 {
		t.Fatalf("audit_ids empty")
	}
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var haveLaunch, haveAction bool
	for _, e := range events {
		if e.Actor.Type != model.ActorTypeService || e.Actor.ID != flightPlanLaunchActor {
			t.Errorf("audit actor = %+v, want service/%s", e.Actor, flightPlanLaunchActor)
		}
		switch e.EventType {
		case model.EventTypeFlightPlanLaunch:
			haveLaunch = true
		case model.EventTypeFlightPlanLaunchAction:
			haveAction = true
			if e.Payload["actionRef"] != "aileron:reporter.build_report" {
				t.Errorf("action record actionRef = %v", e.Payload["actionRef"])
			}
		}
	}
	if !haveLaunch {
		t.Error("no flightplan.launch record emitted")
	}
	if !haveAction {
		t.Error("no flightplan.launch.action record emitted")
	}
}

func TestLaunchFlightPlan_InputOverrideForwarded(t *testing.T) {
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)

	rec := launch(t, srv, "daemon-launch-fixture", map[string]any{
		"inputs": map[string]any{"window_days": 30},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.FlightPlanLaunchResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ResolvedInputs == nil {
		t.Fatal("resolved_inputs missing")
	}
	if v := (*got.ResolvedInputs)["window_days"]; v != float64(30) {
		t.Errorf("override did not flow through: window_days = %v, want 30", v)
	}
	// The overridden value reached the executor's args too.
	if len(exec.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(exec.calls))
	}
	if v := exec.calls[0].args["window_days"]; v != float64(30) {
		t.Errorf("executor arg window_days = %v (%T), want 30", v, v)
	}
}

func TestLaunchFlightPlan_VaultLocked412(t *testing.T) {
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)
	srv.vaultLocked = true

	rec := launch(t, srv, "daemon-launch-fixture", nil)
	// writeVaultLocked maps binding_required → 412 (same as RunAction).
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor must not be called on a locked vault; calls=%v", exec.calls)
	}
}

func TestLaunchFlightPlan_UnknownPlan404(t *testing.T) {
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": reportFileMap}}
	// Install under one name, launch a different name.
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)

	rec := launch(t, srv, "no-such-plan", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "not_found" {
		t.Errorf("code = %v, want not_found", code)
	}
}

func TestLaunchFlightPlan_NilStore404(t *testing.T) {
	srv := &apiServer{
		log:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		executor: &flightplanRecordingExecutor{},
	}
	rec := launch(t, srv, "anything", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestLaunchFlightPlan_BadBody400(t *testing.T) {
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)

	req := httptest.NewRequest(http.MethodPost, "/v1/flightplans/daemon-launch-fixture/launch",
		strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	srv.LaunchFlightPlan(rec, req, "daemon-launch-fixture")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "invalid_body" {
		t.Errorf("code = %v, want invalid_body", code)
	}
}

func TestLaunchFlightPlan_TrailingTokens400(t *testing.T) {
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)

	req := httptest.NewRequest(http.MethodPost, "/v1/flightplans/daemon-launch-fixture/launch",
		strings.NewReader(`{}{}`))
	rec := httptest.NewRecorder()
	srv.LaunchFlightPlan(rec, req, "daemon-launch-fixture")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for trailing tokens; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "invalid_body" {
		t.Errorf("code = %v, want invalid_body", code)
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor must not run on a malformed body; calls=%v", exec.calls)
	}
}

// errorCode extracts error.code from a writeError JSON body.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error.Code
}

func TestLaunchFlightPlan_ExecutorFailureEnvelope(t *testing.T) {
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{
		failures: map[string]*failure.Failure{
			"build_report": failure.Network("metrics API unreachable"),
		},
	}
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)

	rec := launch(t, srv, "daemon-launch-fixture", nil)
	// failure.Network → NetworkError class → 502 Bad Gateway per ADR-0010.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := env["error"]; !ok {
		t.Errorf("body is not an ADR-0010 FailureEnvelope: %v", env)
	}
}

func TestLaunchFlightPlan_UnwrapsDispatchEnvelope(t *testing.T) {
	// The daemon action executor wraps the last step's output in a dispatch
	// envelope {"action": <name>, "output": <result>, "steps": {...}}. The
	// dispatcher must unwrap it (mirroring the CLI's parseResultPayload) so the
	// inner output — here the file-map — is what materializes, not the envelope.
	envelope := `{"action":"build_report","output":` + reportFileMap + `,"steps":{}}`
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": envelope}}
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)

	rec := launch(t, srv, "daemon-launch-fixture", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.FlightPlanLaunchResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The unwrapped file-map materialized a single report.json artifact; had the
	// envelope not been unwrapped, materialize would have written the whole
	// envelope object instead.
	if got.Artifacts == nil || len(*got.Artifacts) != 1 {
		t.Fatalf("artifacts = %v, want 1", got.Artifacts)
	}
	if (*got.Artifacts)[0].Name != "report.json" {
		t.Errorf("artifact name = %q", (*got.Artifacts)[0].Name)
	}
	// The materializing step's output is the unwrapped inner map (carries the
	// file-map keys), not the envelope's action/steps keys.
	if got.StepOutputs == nil {
		t.Fatal("step_outputs missing")
	}
	so := (*got.StepOutputs)["build"]
	if _, ok := so["action"]; ok {
		t.Errorf("step output still carries the envelope's action key: %v", so)
	}
}

func TestLaunchFlightPlan_NilExecutor500(t *testing.T) {
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)
	srv.executor = nil

	rec := launch(t, srv, "daemon-launch-fixture", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "executor_unavailable" {
		t.Errorf("code = %v, want executor_unavailable", code)
	}
}

func TestLaunchFlightPlan_ExplicitVersion(t *testing.T) {
	// An explicit version pins that frozen version (the fixture id is "test").
	fv := freezeFixture(t, deterministicPlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"build_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-launch-fixture", fv, exec)

	rec := launch(t, srv, "daemon-launch-fixture", map[string]any{"version": "test"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestLaunchFlightPlan_ApprovalGatedActionRefused(t *testing.T) {
	// A plan whose constituent action is a WRITE and whose installed action
	// manifest declares [approval] required = true is refused before executing,
	// mirroring the CLI daemonDispatcher's approve-and-re-launch contract. The
	// runtime approves the effect gate (flightplanApprover), so the refusal is
	// the dispatcher's manifest check, surfaced as an ADR-0010 FailureEnvelope.
	fv := freezeFixture(t, writePlanMD)
	exec := &flightplanRecordingExecutor{results: map[string]string{"file_report": reportFileMap}}
	srv, _ := newFlightPlanTestServer(t, "daemon-write-fixture", fv, exec)
	// Install an action store carrying the approval-gated action manifest.
	srv.actions = approvalGatedActionStore(t)

	rec := launch(t, srv, "daemon-write-fixture", nil)
	// The refusal maps to a capability_denied FailureEnvelope (403).
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(exec.calls) != 0 {
		t.Errorf("approval-gated action must not execute; calls=%v", exec.calls)
	}
	var env map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := env["error"]; !ok {
		t.Errorf("body is not an ADR-0010 FailureEnvelope: %v", env)
	}
}
