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

	api "github.com/ALRubinger/aileron/internal/api/gen"
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

	res, err := runtime.Run(r.Context(), runtime.Options{
		Store:   s.flightPlanStore,
		Name:    name,
		Version: version,
		Inputs:  inputs,
		// In-process seams: the dispatcher calls the daemon's own executor and
		// the audit sink calls the daemon's own recorder, so this endpoint does
		// not loop back over HTTP the way the CLI's daemon seams do.
		Dispatcher: &flightplanDispatcher{server: s},
		Approver:   flightplanApprover{},
		Audit:      &flightplanAuditSink{server: s},
		// Image / registry / tool-step / publisher / LLM seams are intentionally
		// nil: this endpoint runs the deterministic in-process pipeline only. A
		// plan that pins an environment image, declares tool steps, or has an
		// llm-seam step errors via the runtime's own nil-guards. Image-booted
		// launch is a follow-up.
	})
	if err != nil {
		s.writeFlightPlanLaunchError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, flightPlanLaunchResponse(res))
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
	// An approval-required constituent action mirrors the CLI's daemonDispatcher
	// contract: refuse, directing the operator to approve it and re-launch. The
	// resume / seam-pending surface is a separate follow-up (#2101).
	var ae *approvalRequiredError
	if errors.As(err, &ae) {
		failure.WriteHTTP(w, failure.CapabilityDeniedAt(failure.Action,
			fmt.Sprintf("action %q requires approval; approve it (POST /v1/actions/%s/run) and re-launch",
				ae.actionRef, ae.actionName)))
		return
	}
	// A denied approval decision (the runtime's own DenyError) is likewise a
	// forbidden outcome.
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

// approvalRequiredError is the sentinel the in-process dispatcher raises when a
// constituent action's manifest declares [approval] required = true. It mirrors
// the CLI daemonDispatcher's "202 → approve-and-re-launch error" behavior: the
// daemon does not run the mid-plan action-boundary 202 registration here (that
// lives in RunAction, not the executor), so the launch refuses deterministically
// and the handler surfaces an ADR-0010 FailureEnvelope. The resume surface is
// #2101.
type approvalRequiredError struct {
	actionRef  string
	actionName string
}

func (e *approvalRequiredError) Error() string {
	return fmt.Sprintf("flightplan: action %q requires approval; approve it and re-launch", e.actionRef)
}

// flightplanDispatcher is the in-process ActionDispatcher seam: it dispatches a
// plan's declared action through the daemon's own executor (the same executor
// POST /v1/actions/{name}/run uses), never over HTTP. It replicates the CLI
// daemonDispatcher's behavior in-process: the action ref (aileron:<c>.<a>) maps
// to the bare daemon action name, an approval-gated action is refused (the CLI
// surfaces the daemon's 202 as an error; here the dispatcher checks the
// manifest and raises the same-shaped refusal), and the executor's string
// Content is parsed into the map the runtime binds downstream.
type flightplanDispatcher struct {
	server *apiServer
}

func (d *flightplanDispatcher) Dispatch(ctx context.Context, ref string, args map[string]any) (runtime.DispatchResult, error) {
	name := flightplanActionName(ref)

	// Refuse an approval-gated action before executing, mirroring the CLI's
	// daemonDispatcher (which surfaces the daemon's 202 as an error). The
	// executor itself does not run RunAction's 202 registration, so the launch
	// refuses deterministically; the handler maps this to a FailureEnvelope.
	//
	// The approval gate reads the SAME action store the executor is backed by
	// (s.actions), so the two can never diverge: an action that is not resolvable
	// here is not resolvable by the executor either, and fails not-found there
	// with a precise ADR-0010 failure. The security property — an approval-gated
	// action never runs unattended through launch — therefore holds by refusing
	// on any positively-loaded manifest that declares [approval] required = true
	// (or has failed to carry a manifest). A missing store or a not-installed
	// action is deliberately allowed to fall through to the executor, which is
	// the single authority on action existence and produces the accurate error.
	if d.server.actions != nil {
		if loaded, err := d.server.actions.Get(name); err == nil {
			if loaded.Manifest == nil || loaded.Manifest.ApprovalRequired() {
				return runtime.DispatchResult{}, &approvalRequiredError{actionRef: ref, actionName: name}
			}
		}
	}

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

// flightplanApprover defers to the action boundary exactly as the CLI's
// daemonApprover does: it always approves so the effect-gate is decided once,
// at the dispatcher's manifest check (which refuses an approval-required action
// before executing). A read action never reaches the approver.
type flightplanApprover struct{}

func (flightplanApprover) Approve(_ context.Context, _ runtime.ApprovalRequest) (runtime.Decision, error) {
	return runtime.Decision{Approved: true}, nil
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
