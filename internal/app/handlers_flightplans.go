package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"sort"

	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/model"
)

// flightPlanLaunchActor is the audit actor id stamped on every record a
// daemon-initiated Flight Plan launch emits. The launch runs on the daemon's
// own behalf (the HTTP door aileron-mcp calls), not an interactive operator, so
// it is a service actor rather than the human-operator id the CLI's
// daemonAuditSink resolves. Using a stable service id keeps the daemon-launched
// records attributable and self-describing without inventing a new
// AILERON_OPERATOR_ID dependency on the daemon side.
const flightPlanLaunchActor = "flightplan-launch"

// ListFlightPlans returns the installed frozen Flight Plans in the skill store,
// plus a per-plan load_errors slice for any plan whose latest frozen version
// fails to verify or parse (per ADR-0010). It mirrors ListActions: a nil store
// lists as empty, loading is non-fatal, and the list is sorted by name so the
// aileron-mcp diff is stable.
//
// Each surviving plan is projected to a FlightPlanSummary carrying its latest
// frozen version id, description, and declared input/output interface — enough
// for aileron-mcp to expose one MCP tool per plan (#2098). The daemon carries
// the raw manifest input type (including `timestamp`); the MCP side projects
// `timestamp` to a JSON Schema type when deriving the tool input schema.
func (s *apiServer) ListFlightPlans(w http.ResponseWriter, r *http.Request) {
	if s.flightPlanStore == nil {
		// Match the populated path's shape: an always-present (possibly empty)
		// items array, so a client sees a consistent contract whether or not a
		// store is configured.
		writeJSON(w, http.StatusOK, api.FlightPlanListResponse{Items: &[]api.FlightPlanSummary{}})
		return
	}
	names, err := s.flightPlanStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "flightplan_list_failed", err.Error())
		return
	}

	items := make([]api.FlightPlanSummary, 0, len(names))
	var loadErrors []api.FlightPlanLoadError
	for _, name := range names {
		id, count, err := s.flightPlanStore.LatestFrozen(name)
		if err != nil {
			loadErrors = append(loadErrors, flightPlanLoadError(
				"lookup_error", err.Error(), s.flightPlanStore.Dir(name)))
			continue
		}
		if count == 0 {
			// List() already filters to installed/frozen plans, but a plan whose
			// only frozen dir lost its SKILL.md between calls must not appear.
			continue
		}
		loaded, err := runtime.LoadVerified(s.flightPlanStore, name, id)
		if err != nil {
			// A tampered or unparseable frozen version surfaces under load_errors
			// (ADR-0010 non-fatal loading) rather than aborting the whole list.
			loadErrors = append(loadErrors, flightPlanLoadError(
				"verification_error", err.Error(), s.flightPlanStore.FrozenDir(name, id)))
			continue
		}
		items = append(items, s.planToSummary(name, id, loaded))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	resp := api.FlightPlanListResponse{Items: &items}
	if len(loadErrors) > 0 {
		resp.LoadErrors = &loadErrors
	}
	writeJSON(w, http.StatusOK, resp)
}

// planToSummary projects a verified LoadedPlan into the API summary shape: the
// plan name, its latest frozen version id, the manifest description, and the
// declared input/output interface.
func (s *apiServer) planToSummary(name, id string, loaded runtime.LoadedPlan) api.FlightPlanSummary {
	summary := api.FlightPlanSummary{Name: name, Version: id}
	if desc := s.planDescription(name, id); desc != "" {
		summary.Description = &desc
	}
	if loaded.Plan != nil {
		if inputs := planInputsToAPI(loaded.Plan.Inputs); len(inputs) > 0 {
			summary.Inputs = &inputs
		}
		if outputs := planOutputsToAPI(loaded.Plan.Outputs); len(outputs) > 0 {
			summary.Outputs = &outputs
		}
	}
	return summary
}

// planDescription reads the frozen manifest's frontmatter description. The
// bytes were already verified by LoadVerified above; this parse is a cheap
// read of the same file for the human-facing description the typed Plan does
// not carry. A read or parse failure yields an empty description rather than
// failing the list — the description is presentational, not load-bearing.
func (s *apiServer) planDescription(name, id string) string {
	fv, err := s.flightPlanStore.ReadFrozen(name, id)
	if err != nil {
		return ""
	}
	m, err := manifest.Parse(fv.SkillMD)
	if err != nil {
		return ""
	}
	return m.Description
}

// planInputsToAPI projects the typed plan inputs into the API input shape. The
// raw manifest type is carried verbatim (the MCP side projects `timestamp`);
// `required` is computed here: a literal-rule input with no declared default is
// caller-required, while dynamic, source, and defaulted inputs auto-resolve at
// launch and are not.
func planInputsToAPI(inputs []runtime.Input) []api.FlightPlanInput {
	out := make([]api.FlightPlanInput, 0, len(inputs))
	for _, in := range inputs {
		required := in.Resolution.Rule == runtime.ResolutionLiteral && !in.Resolution.HasDefault
		item := api.FlightPlanInput{
			Name:     in.Name,
			Type:     api.FlightPlanInputType(string(in.Type)),
			Required: required,
		}
		if in.Description != "" {
			d := in.Description
			item.Description = &d
		}
		out = append(out, item)
	}
	return out
}

// planOutputsToAPI projects the declared outputs into the API output shape,
// sorted by name for a deterministic list.
func planOutputsToAPI(outputs map[string]runtime.Output) []api.FlightPlanOutput {
	out := make([]api.FlightPlanOutput, 0, len(outputs))
	for _, o := range outputs {
		item := api.FlightPlanOutput{Name: o.Name}
		if o.MimeType != "" {
			mt := o.MimeType
			item.MimeType = &mt
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// flightPlanLoadError builds an ADR-0010 load-error entry for a plan whose
// latest frozen version failed to load, with the always-"flightplan" boundary.
func flightPlanLoadError(class, message, file string) api.FlightPlanLoadError {
	boundary := "flightplan"
	return api.FlightPlanLoadError{
		Class:    class,
		Message:  message,
		File:     file,
		Boundary: &boundary,
	}
}

// LaunchFlightPlan launches an installed, frozen, deterministic Flight Plan by
// name through internal/flightplan/runtime.Run (issue #2097). It is the HTTP
// door aileron-mcp uses: the MCP process speaks only HTTP to the daemon, so it
// cannot reach the in-process runtime.Run entry the CLI calls directly.
//
// The handler mirrors RunAction's request/dispatch/auth/error-mapping shape:
// vault-locked → 412, no store/executor → 404/500, bad JSON → 400, ADR-0010
// FailureEnvelope mapping on error. The load-bearing difference from the CLI
// launch path is that this endpoint IS the daemon, so its SPI seams call the
// daemon's own executor and audit recorder in-process rather than looping back
// over HTTP (which is what the CLI's daemonDispatcher / daemonAuditSink do,
// because the CLI is out-of-process). That keeps the executor and the audit
// trail single-writer.
func (s *apiServer) LaunchFlightPlan(w http.ResponseWriter, r *http.Request, name string) {
	if s.vaultLocked {
		writeVaultLocked(w)
		return
	}
	if s.flightPlanStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "flight plan store not configured")
		return
	}
	if s.executor == nil {
		writeError(w, http.StatusInternalServerError, "executor_unavailable", "action executor not configured")
		return
	}

	// An empty body is valid (launch latest, all-default inputs), so decode
	// only when a body is present. An explicitly malformed body is a 400,
	// mirroring RunAction's invalid_body branch.
	var req api.FlightPlanLaunchRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			// An empty body (io.EOF) is valid: launch latest with all defaults.
			// Any other decode error is a malformed body.
			if !errors.Is(err, io.EOF) {
				writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
				return
			}
		} else if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			// Reject trailing tokens after the first JSON value so a body like
			// `{}{}` or `{} garbage` is a 400 rather than silently accepting the
			// first object.
			writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
			return
		}
	}

	// Resolve the version to launch, mirroring the CLI: an explicit version
	// pins it; otherwise the latest frozen version by freeze time. A plan with
	// no frozen version is a 404 (the plan is unknown to the launch surface).
	version := ""
	if req.Version != nil {
		version = *req.Version
	}
	if version == "" {
		id, count, err := s.flightPlanStore.LatestFrozen(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "flightplan_lookup_failed", err.Error())
			return
		}
		if count == 0 {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("flight plan %q has no frozen version to launch", name))
			return
		}
		version = id
	}

	inputs := runtime.LaunchArgs{}
	if req.Inputs != nil {
		inputs = runtime.LaunchArgs(*req.Inputs)
	}

	// A fresh launch owns no run id yet (the runtime mints one on the first
	// suspend) and carries no accumulated memo. runOrResume drives the run and
	// branches on completion vs. suspend, storing the run record on a suspend and
	// deleting it on a terminal outcome.
	s.runOrResume(w, r, flightPlanRunRecord{
		Name:    name,
		Version: version,
		Inputs:  inputs,
	}, "")
}

// ResumeFlightPlan resumes a Flight Plan run that suspended mid-plan (#2101),
// keyed by the server-minted run id. It reads the in-memory run record, merges
// any newly-supplied seam outputs into the accumulated memo, and replays the run
// through the runtime with that memo (exactly-once for effects). The response
// mirrors launch: 200 completed / 200 seam_pending / 202 pending_approval, with
// a denied approval failing the run closed (403) and a terminal outcome deleting
// the record.
func (s *apiServer) ResumeFlightPlan(w http.ResponseWriter, r *http.Request, runID string) {
	if s.vaultLocked {
		writeVaultLocked(w)
		return
	}
	if s.flightPlanStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "flight plan store not configured")
		return
	}
	if s.executor == nil {
		writeError(w, http.StatusInternalServerError, "executor_unavailable", "action executor not configured")
		return
	}

	// Decode the optional resume body. An empty body is valid (an approval-driven
	// resume supplies no outputs); a malformed body is a 400.
	var req api.FlightPlanResumeRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) {
				writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
				return
			}
		} else if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
			return
		}
	}

	rec, ok := s.flightPlanRuns.Get(runID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found",
			"run id is unknown or expired (the run registry is in-memory; a daemon restart orphans in-flight runs)")
		return
	}

	// Merge seam outputs supplied on this resume into the accumulated memo before
	// replay. Validate they name the suspended seam's declared outputs so a
	// malformed resume fails before touching the runtime.
	var resumeOutputs map[string]map[string]any
	if req.Outputs != nil {
		resumeOutputs = normalizeResumeOutputs(*req.Outputs)
		if err := s.validateSeamResumeOutputs(rec, resumeOutputs); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_resume_outputs", err.Error())
			return
		}
		s.flightPlanRuns.MergeOutputs(runID, resumeOutputs)
	}

	// Re-read the record so the merged memo is visible, then replay. The registry
	// is shared and in-memory: a racing resume/completion could have deleted the
	// record between the first Get and here, so re-check ok rather than
	// dereferencing a possibly-nil pointer.
	rec, ok = s.flightPlanRuns.Get(runID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found",
			"run id is unknown or expired (the run registry is in-memory; a daemon restart orphans in-flight runs)")
		return
	}
	s.runOrResume(w, r, *rec, runID)
}

// normalizeResumeOutputs deep-copies the API resume-outputs shape
// (stepId → outputName → value) into the runtime's memo shape so the stored
// record never aliases the request body.
func normalizeResumeOutputs(in map[string]map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(in))
	for step, named := range in {
		cp := make(map[string]any, len(named))
		for k, v := range named {
			cp[k] = v
		}
		out[step] = cp
	}
	return out
}

// validateSeamResumeOutputs checks that resume outputs name the run's currently
// suspended seam step and carry exactly its declared outputs. It loads the plan
// to read the suspended step's declared output names. A resume that supplies
// outputs for a step that is not an llm-seam, or omits/adds a declared output,
// is a 400 (the malformed resume fails before replay).
func (s *apiServer) validateSeamResumeOutputs(rec *flightPlanRunRecord, outputs map[string]map[string]any) error {
	if len(outputs) == 0 {
		return nil
	}
	loaded, err := runtime.LoadVerified(s.flightPlanStore, rec.Name, rec.Version)
	if err != nil {
		return fmt.Errorf("cannot verify plan to validate resume outputs: %v", err)
	}
	declared := map[string][]string{}
	for _, st := range loaded.Plan.Steps {
		if st.Kind == runtime.KindLLMSeam {
			declared[st.ID] = st.Outputs
		}
	}
	for stepID, named := range outputs {
		want, ok := declared[stepID]
		if !ok {
			return fmt.Errorf("resume outputs name step %q, which is not an llm-seam step in this plan", stepID)
		}
		if len(named) != len(want) {
			return fmt.Errorf("seam step %q declares %d output(s) but resume supplied %d", stepID, len(want), len(named))
		}
		for _, name := range want {
			if _, present := named[name]; !present {
				return fmt.Errorf("seam step %q resume is missing declared output %q", stepID, name)
			}
		}
	}
	return nil
}

// runOrResume drives a Flight Plan run (fresh launch or resume) through the
// suspendable runtime path and branches on the outcome, shared by
// LaunchFlightPlan and ResumeFlightPlan so the two speak an identical
// suspend/complete contract. runID is "" on a fresh launch (the runtime mints
// one on the first suspend) and the stable run id on a resume.
//
// The seam is intentionally left unwired (Options.Seam nil): on the suspendable
// path an unfulfilled seam with no memo entry suspends via SuspendKindSeam, and
// the agent fulfills it by feeding outputs into ResumeOutputs on the next
// resume. The daemon is the seam provider, so no LLM is reached daemon-side.
func (s *apiServer) runOrResume(w http.ResponseWriter, r *http.Request, rec flightPlanRunRecord, runID string) {
	// The approver writes a first-reach approval id into this shared map so the
	// handler reads it back after Run returns (to fill the 202 and store the run
	// record). A fresh launch seeds an empty map; a resume carries the record's.
	if rec.Approvals == nil {
		rec.Approvals = map[string]string{}
	}
	approver := &flightplanApprover{server: s, runID: runID, approvals: rec.Approvals}
	res, err := runtime.Run(r.Context(), runtime.Options{
		Store:         s.flightPlanStore,
		Name:          rec.Name,
		Version:       rec.Version,
		Inputs:        rec.Inputs,
		Suspendable:   true,
		ResumeOutputs: rec.Outputs,
		RunID:         runID,
		// In-process seams: the dispatcher calls the daemon's own executor and
		// the audit sink calls the daemon's own recorder, so this endpoint does
		// not loop back over HTTP the way the CLI's daemon seams do.
		Dispatcher: &flightplanDispatcher{server: s},
		Approver:   approver,
		Audit:      &flightplanAuditSink{server: s},
		// Image / registry / tool-step / publisher seams are intentionally nil:
		// this endpoint runs the in-process pipeline only. A plan that pins an
		// environment image or declares tool steps errors via the runtime's own
		// nil-guards. The LLM seam is nil so an unfulfilled seam SUSPENDS
		// (SuspendKindSeam) rather than erroring — the agent is the provider.
		//
		// The runtime's llm-seam surface gate (#2102) is INERT for this caller:
		// it fires only when the seam is unwired AND the run is not suspendable,
		// and this endpoint sets Suspendable above. So a seam plan here suspends
		// for the agent to fulfill via resume; it does not fail closed. That gate
		// governs the plain non-agent surface (a bare `aileron skill launch` with
		// no agent-backed provider), where a seam plan fails closed at the Run
		// precondition and surfaces through writeFlightPlanLaunchError as a
		// runtime-boundary FailureEnvelope.
	})
	if err != nil {
		// A denied approval (or any other runtime error) on this call is terminal:
		// drop the run record so a fail-closed run does not linger in the registry.
		if runID != "" {
			s.flightPlanRuns.Delete(runID)
		}
		s.writeFlightPlanLaunchError(w, err)
		return
	}

	if res.Pending != nil {
		s.writeFlightPlanPending(w, rec, res)
		return
	}

	// A completed run: drop the record (if any) and return the terminal result.
	if res.Pending == nil && runID != "" {
		s.flightPlanRuns.Delete(runID)
	}
	writeJSON(w, http.StatusOK, flightPlanLaunchResponse(res))
}

// writeFlightPlanPending stores/updates the run record with the suspend's
// accumulated memo and writes the matching pending envelope: 200 seam_pending
// for a seam suspend, 202 pending_approval for an approval suspend.
func (s *apiServer) writeFlightPlanPending(w http.ResponseWriter, rec flightPlanRunRecord, res runtime.RunResult) {
	pending := res.Pending
	runID := pending.RunID

	// Persist the accumulated memo (and immutable re-launch state) under the
	// run id so the resume replays the completed prefix without re-execution.
	// The approver may already have recorded an approval linkage on rec.Approvals
	// during this call (a first-reach gated action), so carry it forward.
	stored := &flightPlanRunRecord{
		Name:      rec.Name,
		Version:   rec.Version,
		Inputs:    rec.Inputs,
		Outputs:   pending.StepOutputs,
		Approvals: rec.Approvals,
	}
	s.flightPlanRuns.Put(runID, stored)

	switch pending.Kind {
	case runtime.SuspendKindSeam:
		writeJSON(w, http.StatusOK, flightPlanSeamPendingResponse(runID, pending.Seam))
	case runtime.SuspendKindApproval:
		// The approval id was recorded on the run record by the approver during
		// this call (keyed by the suspended action's ref).
		approvalID := ""
		actionName := ""
		connFQN := ""
		if pending.Approval != nil {
			actionName = flightplanActionName(pending.Approval.ActionRef)
			if stored.Approvals != nil {
				approvalID = stored.Approvals[pending.Approval.ActionRef]
			}
			if entry, ok := s.actionApprovals.Get(approvalID); ok {
				connFQN = entry.ConnectorFQN
			}
		}
		reviewURL := buildApprovalsReviewURL(s.webappURL, "", approvalID)
		writeJSON(w, http.StatusAccepted, buildPendingApprovalResponse(approvalID, actionName, connFQN, reviewURL))
	default:
		// An unknown suspend kind is a runtime-contract violation, not a client
		// error; surface it as a runtime-boundary failure rather than a bare 200.
		s.writeFlightPlanLaunchError(w, fmt.Errorf("flightplan: unknown suspend kind %d", pending.Kind))
	}
}

// flightPlanSeamPendingResponse builds the 200 seam_pending body from a seam
// SuspendResult. The prompt/model/outputs/bindings ride the runtime's
// SeamRequest (the sealed template + recorded model hint landed by #2105).
func flightPlanSeamPendingResponse(runID string, seam *runtime.SeamRequest) api.FlightPlanSeamPendingResponse {
	out := api.FlightPlanSeamPendingResponse{
		Status: api.SeamPending,
		RunId:  runID,
	}
	if seam != nil {
		sr := api.FlightPlanSeamRequest{
			StepId:  seam.StepID,
			Outputs: append([]string(nil), seam.Outputs...),
		}
		if seam.Prompt != "" {
			p := seam.Prompt
			sr.Prompt = &p
		}
		if seam.Model != "" {
			m := seam.Model
			sr.Model = &m
		}
		if len(seam.Bindings) > 0 {
			b := map[string]interface{}(seam.Bindings)
			sr.Bindings = &b
		}
		out.Seam = sr
	}
	return out
}

// writeFlightPlanLaunchError maps a runtime.Run error onto an ADR-0010
// FailureEnvelope response. A *failure.Failure surfaced up from the executor
// (through the dispatcher) is written with its canonical per-class status; a
// runtime.DenyError (an effect-gated action refused on the deterministic path)
// and every other runtime error surface as their appropriate class.
func (s *apiServer) writeFlightPlanLaunchError(w http.ResponseWriter, err error) {
	// An executor action-side failure threaded up through the dispatcher keeps
	// its class/status so a caller sees the same ADR-0010 envelope RunAction
	// would have produced for that action.
	var fe *failure.Failure
	if errors.As(err, &fe) {
		failure.WriteHTTP(w, fe)
		return
	}
	// A denied approval decision (the runtime's own DenyError) fails the run
	// closed: a mid-plan gated action the user denied is a forbidden outcome
	// (#2101). This is the terminal state a resume reaches after the agent denied
	// the approval through the existing approval channel.
	var de *runtime.DenyError
	if errors.As(err, &de) {
		failure.WriteHTTP(w, failure.CapabilityDeniedAt(failure.Action, de.Error()))
		return
	}
	// Every other runtime error (verification failure, missing input, a plan
	// that needs an unwired seam) is a runtime-boundary error.
	failure.WriteHTTP(w, failure.ConnectorRuntime(err.Error(), false,
		failure.WithBoundary(failure.Runtime)))
}

// flightPlanLaunchResponse builds the API response body from a runtime.Run
// result. The daemon writes no host out-dir, so each artifact is surfaced by
// its metadata (name/path/mime) and content digest; the raw bytes are not
// returned inline.
func flightPlanLaunchResponse(res runtime.RunResult) api.FlightPlanLaunchResponse {
	out := api.FlightPlanLaunchResponse{ContentHash: res.ContentHash}
	if res.ResolvedInputs != nil {
		ri := map[string]interface{}(res.ResolvedInputs)
		out.ResolvedInputs = &ri
	}
	if len(res.StepOutputs) > 0 {
		so := make(map[string]map[string]interface{}, len(res.StepOutputs))
		for id, named := range res.StepOutputs {
			so[id] = map[string]interface{}(named)
		}
		out.StepOutputs = &so
	}
	if len(res.Artifacts) > 0 {
		arts := make([]api.FlightPlanArtifact, 0, len(res.Artifacts))
		for _, a := range res.Artifacts {
			art := api.FlightPlanArtifact{Name: a.Name, Digest: a.Digest}
			if a.Path != "" {
				p := a.Path
				art.Path = &p
			}
			if a.MimeType != "" {
				mt := a.MimeType
				art.MimeType = &mt
			}
			arts = append(arts, art)
		}
		out.Artifacts = &arts
	}
	if len(res.AuditIDs) > 0 {
		ids := append([]string(nil), res.AuditIDs...)
		out.AuditIds = &ids
	}
	return out
}

// flightplanDispatcher is the in-process ActionDispatcher seam: it dispatches a
// plan's declared action through the daemon's own executor (the same executor
// POST /v1/actions/{name}/run uses), never over HTTP. The action ref
// (aileron:<c>.<a>) maps to the bare daemon action name, and the executor's
// string Content is parsed into the map the runtime binds downstream.
//
// It no longer pre-emptively refuses an approval-gated action (#2101): gating
// now flows through the Approver seam, which suspends the run and registers a
// pending approval. The dispatcher only reaches a gated action's Execute on a
// resume AFTER the approver observed an approved outcome, so a dispatch here is
// always an approved (or read) action.
type flightplanDispatcher struct {
	server *apiServer
}

func (d *flightplanDispatcher) Dispatch(ctx context.Context, ref string, args map[string]any) (runtime.DispatchResult, error) {
	name := flightplanActionName(ref)

	result, err := d.server.executor.Execute(ctx, name, args)
	if err != nil {
		return runtime.DispatchResult{}, fmt.Errorf("flightplan: dispatch %q: %w", ref, err)
	}
	if result.Failure != nil {
		// Surface the executor's ADR-0010 failure up through the runtime error
		// channel so the handler can write it with its canonical status
		// (failure.WriteHTTP), exactly as RunAction does for a direct action run.
		return runtime.DispatchResult{}, result.Failure
	}

	return runtime.DispatchResult{
		Output:            parseFlightplanResultPayload(result.Content),
		ConnectorVersion:  result.Provenance.ConnectorVersion,
		ConnectorHash:     result.Provenance.ConnectorHash,
		IdentityLabel:     result.Provenance.IdentityLabel,
		CredentialBinding: result.Provenance.CredentialBinding,
		ConsentDecision:   "unattended",
	}, nil
}

// flightplanActionName maps a manifest action ref (aileron:<connector>.<action>)
// to the bare daemon action name, mirroring the CLI's daemonActionName.
func flightplanActionName(ref string) string {
	r := strings.TrimPrefix(ref, "aileron:")
	if i := strings.LastIndex(r, "."); i >= 0 {
		return r[i+1:]
	}
	return r
}

// parseFlightplanResultPayload decodes the executor's string result into a JSON
// map the runtime binds downstream, unwrapping the daemon's dispatch envelope
// exactly as the CLI's parseResultPayload / dispatchEnvelopeOutput do. A
// non-JSON or empty result surfaces under a "result" key so a downstream
// binding still resolves.
func parseFlightplanResultPayload(result string) map[string]any {
	if result == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err == nil {
		if out, ok := flightplanDispatchEnvelopeOutput(m); ok {
			return out
		}
		return m
	}
	return map[string]any{"result": result}
}

// flightplanDispatchEnvelopeOutput returns the inner output map when m is the
// daemon's dispatch envelope (a JSON object carrying an "action" string AND an
// "output" object), mirroring the CLI's dispatchEnvelopeOutput. Any other shape
// (including StubExecutor results, which carry "action" but no "output") passes
// through unchanged.
func flightplanDispatchEnvelopeOutput(m map[string]any) (map[string]any, bool) {
	if _, ok := m["action"].(string); !ok {
		return nil, false
	}
	out, ok := m["output"].(map[string]any)
	if !ok {
		return nil, false
	}
	return out, true
}

// flightplanApprover is the plan-scoped Approver seam that drives the mid-plan
// action-approval handshake (#2101). The runtime calls Approve ONLY for an
// effect-gated action (a read never reaches it). Its behavior depends on whether
// the run has already registered an approval for the action ref:
//
//   - First reach (no recorded approval id): register a pending entry in the
//     SAME action-approval queue POST /v1/actions/{name}/run uses (reusing the
//     preview + input-field projection), record actionRef → entry.ID on the run
//     record, and return Decision{Pending:true}. The run SUSPENDS; the handler
//     surfaces the 202. NOTE: no executeApprovedAction goroutine is spawned — the
//     plan resumes via replay and the dispatcher runs the now-approved action
//     itself (residual #2104).
//   - Resume reach (a recorded approval id): read the queue's outcome. Approved
//     (or approved_not_started, the state Decide lands at with no auto-executor)
//     → Decision{Approved:true}, so the dispatcher runs the action. Denied →
//     Decision{} (an explicit deny), so the run fails closed. Still pending →
//     Decision{Pending:true} again (idempotent re-suspend, no re-register).
//
// runID is "" on the very first launch call (the runtime mints one on the first
// suspend). Because a suspend needs the recorded approval id to fill the 202, the
// approver writes into the shared approvals map (also referenced by the run
// record) so the handler reads the id back after Run returns.
type flightplanApprover struct {
	server *apiServer
	runID  string
	// approvals is the run record's actionRef → approvalID map, shared with the
	// handler so a first-reach registration is visible when the handler builds the
	// 202. Never nil for a resume (the record carries it); the handler seeds a
	// fresh map on the first launch.
	approvals map[string]string
}

func (a *flightplanApprover) Approve(ctx context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	if a.server.actionApprovals == nil {
		// No approval queue configured: fail closed rather than silently running a
		// gated action unattended.
		return runtime.Decision{}, nil
	}
	ref := req.ActionRef

	// Resume reach: a recorded approval id means this ref already suspended once.
	if id, ok := a.approvals[ref]; ok && id != "" {
		outcome, found := a.server.actionApprovals.Outcome(id)
		if !found {
			// The recorded entry is gone (queue is in-memory; a restart lost it):
			// re-suspend by re-registering below is unsafe (would double-register),
			// so fail closed — the agent re-launches.
			return runtime.Decision{}, nil
		}
		switch outcome.Status {
		case approval.OutcomeApprovedNotStarted, approval.OutcomeRunning,
			approval.OutcomeCompleted, approval.OutcomeAwaitingVault:
			// The user approved. No background executor ran the action for a
			// plan-scoped approval (#2104), so the dispatcher runs it now on replay.
			// (Running/Completed/AwaitingVault are unreachable on the plan path — no
			// executeApprovedAction goroutine is spawned — but treating them as
			// approved keeps the mapping correct if that ever changes.)
			return runtime.Decision{Approved: true}, nil
		case approval.OutcomeDenied:
			// The user denied: fail the run closed.
			return runtime.Decision{Reason: outcome.DenyReason}, nil
		case approval.OutcomePendingApproval:
			// Still awaiting the user's decision: re-suspend idempotently (no
			// re-register — the recorded id already exists).
			return runtime.Decision{Pending: true}, nil
		default: // OutcomeFailed or any unexpected terminal state
			// Unreachable on the plan-scoped path, but fail closed rather than
			// re-suspend forever if a terminal-failed entry is ever observed.
			return runtime.Decision{Reason: "approval entry reached an unexpected terminal state: " + string(outcome.Status)}, nil
		}
	}

	// First reach: register a pending approval mirroring RunAction's registration
	// (preview + input-field projection) and suspend.
	name := flightplanActionName(ref)
	connFQN := ""
	var preview *approval.ActionApprovalPreview
	var inputFields []approval.ActionApprovalPreviewField
	if a.server.actions != nil {
		if loaded, err := a.server.actions.Get(name); err == nil && loaded.Manifest != nil {
			if len(loaded.Manifest.Execute) > 0 {
				connFQN = loaded.Manifest.Execute[0].Connector
			}
			if invoker, ok := a.server.executor.(action.PreviewInvoker); ok && loaded.Manifest.ApprovalPreview() != nil {
				pr := invoker.InvokePreview(ctx, loaded.Manifest, req.Args)
				preview = previewFromActionResult(pr)
			}
			inputFields = inputFieldsForAPI(action.BuildInputFields(loaded.Manifest, req.Args))
		}
	}
	entry := a.server.actionApprovals.RegisterKindWithPreviewAndInputs(
		approval.ApprovalKindAction, name, connFQN, "", req.Args, preview, inputFields)

	// Record the linkage so the handler can fill the 202 and a later resume can
	// look up the outcome. The runtime has minted the run id by the time the
	// SuspendResult surfaces; the handler stores the record under it. Writing into
	// the shared map makes the id visible to the handler after Run returns.
	if a.approvals != nil {
		a.approvals[ref] = entry.ID
	}
	if a.runID != "" {
		a.server.flightPlanRuns.RecordApproval(a.runID, ref, entry.ID)
	}
	return runtime.Decision{Pending: true}, nil
}

// flightplanAuditSink is the in-process AuditSink seam: it persists each launch
// audit record through the daemon's own recorder (the same path CreateAudit
// uses), never over HTTP. It translates the runtime's AuditRecordKind into a
// model.EventType and shapes the payload exactly as the CLI's daemonAuditSink
// does (flat aileron.* for output/reach/launch records, nested actionRef/sink
// for the per-action record), then records with a service actor.
//
// Record returns only a string per the SPI, so a persistence failure is
// best-effort: it returns "" and the launch continues, matching the recorder's
// own best-effort append discipline (ADR-0010).
type flightplanAuditSink struct {
	server *apiServer
}

func (s *flightplanAuditSink) Record(ctx context.Context, rec runtime.AuditRecord) string {
	if s.server.auditRecorder == nil {
		return ""
	}
	eventType, payload := flightplanAuditEvent(rec)
	actor := model.ActorRef{Type: model.ActorTypeService, ID: flightPlanLaunchActor}
	// Normalize actor provenance onto the Actor object exactly as CreateAudit
	// does at ingest (residual #1770): the runtime emits flat `aileron.actor.*`
	// payload keys, and the daemon lifts them onto event.Actor so provenance
	// lives on the actor. The CLI's daemonAuditSink gets this for free by POSTing
	// through CreateAudit; this in-process sink calls RecordEvent directly, so it
	// must apply the same lift to keep the daemon-launched audit byte-equivalent
	// to the CLI-launched one.
	if v, ok := payloadString(payload, "aileron.actor.identity_label"); ok {
		actor.IdentityLabel = v
	}
	if v, ok := payloadString(payload, "aileron.actor.credential_binding"); ok {
		actor.CredentialBinding = v
	}
	id, err := s.server.auditRecorder.RecordEvent(ctx, eventType, actor, payload)
	if err != nil {
		return ""
	}
	return id
}

// flightplanAuditEvent maps a runtime AuditRecord onto the daemon event type and
// payload shape, mirroring the CLI daemonAuditSink.Record translation:
//
//   - RecordKindAction  → flightplan.launch.action, nested {actionRef, fields, sink}
//   - RecordKindLaunch  → flightplan.launch, flat aileron.* fields
//   - RecordKindOutput  → output.materialized, flat aileron.* fields
//   - RecordKindReach   → flightplan.launch.reach, flat aileron.* fields
//
// The flat-vs-nested split matches #1928: output/reach/launch records surface
// their aileron.* map as the top-level payload so the invocation filter and the
// webapp Timeline read aileron.invocation.id where they look for it; only the
// per-action record keeps the nested fields/actionRef/sink shape.
func flightplanAuditEvent(rec runtime.AuditRecord) (model.EventType, map[string]any) {
	switch rec.Kind {
	case runtime.RecordKindOutput:
		return model.EventTypeOutputMaterialized, flatOrEmpty(rec.Fields)
	case runtime.RecordKindReach:
		return model.EventTypeFlightPlanLaunchReach, flatOrEmpty(rec.Fields)
	case runtime.RecordKindLaunch:
		return model.EventTypeFlightPlanLaunch, flatOrEmpty(rec.Fields)
	default: // runtime.RecordKindAction
		return model.EventTypeFlightPlanLaunchAction, flightplanActionPayload(rec)
	}
}

// flatOrEmpty returns the record's flat aileron.* fields, guarding a nil map so
// an empty record records an object payload rather than a JSON null.
func flatOrEmpty(fields map[string]any) map[string]any {
	if fields == nil {
		return map[string]any{}
	}
	return fields
}

// flightplanActionPayload builds the nested payload for the per-action record:
// actionRef and sink when present, and the declared audit fields under a
// "fields" key. It mirrors the CLI's actionOrLaunchPayload.
func flightplanActionPayload(rec runtime.AuditRecord) map[string]any {
	payload := map[string]any{}
	if rec.ActionRef != "" {
		payload["actionRef"] = rec.ActionRef
	}
	if len(rec.Fields) > 0 {
		payload["fields"] = rec.Fields
	}
	if rec.Sink != "" {
		payload["sink"] = rec.Sink
	}
	return payload
}
