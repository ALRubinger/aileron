package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/model"
)

// The Launch SPIs are wired here from package-level seams so CLI tests
// exercise the orchestration with fakes and no live daemon, mirroring
// skill_freeze.go's newDigestResolver/newFeatureComposer discipline. Each seam
// returns the daemon-backed implementation in production.
var newLaunchDispatcher = func() runtime.ActionDispatcher { return daemonDispatcher{} }
var newLaunchApprover = func() runtime.Approver { return daemonApprover{} }
var newLaunchAuditSink = func(stderr io.Writer) runtime.AuditSink { return daemonAuditSink{stderr: stderr} }

// newLaunchImageRunner returns the production image runner that boots the
// verified pinned rung-1/rung-2 image and runs the plan inside it (#1731). It
// is a package-level seam so CLI tests swap in a fake that records the exact
// image string and never touches Docker, mirroring the other launch seams.
var newLaunchImageRunner = func() runtime.ImageRunner { return containerImageRunner{} }

// newLaunchToolImageRunner returns the production tool-image runner that
// dispatches a rung-3 step to its pinned sibling tool image with mount → run →
// collect I/O (#1733). It is a package-level seam so CLI tests swap in a fake
// that records the pinned image and mount/collect wiring and never touches
// Docker, mirroring newLaunchImageRunner.
// The production runner injects the daemon-backed proxy bootstrapper so a
// rung-3 tool step's HTTPS egress is routed through the ADR-0019 daemon forward
// proxy for host-binding credential injection (#1769). The bootstrapper stays
// passthrough when no daemon config is resolvable.
var newLaunchToolImageRunner = func() runtime.ToolImageRunner {
	return containerToolImageRunner{proxy: daemonToolProxyBootstrapper{}}
}

// launchSeamForTest is the LLM seam the launch wires into the runtime. It is
// nil by default, which is the v1 contract: a plan with an llm-seam step
// errors unless a provider is configured, so a default launch reaches no LLM.
// It is a package-level seam so tests can supply a deterministic seam; there
// is no production LLM seam provider wired in v1.
var launchSeamForTest runtime.LLMSeam

// inputFlag collects repeated --input name=value launch overrides.
type inputFlag struct {
	values map[string]any
}

func (f *inputFlag) String() string { return "" }

func (f *inputFlag) Set(s string) error {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return fmt.Errorf("--input must be name=value, got %q", s)
	}
	if f.values == nil {
		f.values = map[string]any{}
	}
	f.values[s[:i]] = s[i+1:]
	return nil
}

// runSkillLaunch implements `aileron skill launch <name>`. It loads a frozen
// version from the store, verifies it, resolves declared inputs, walks the
// deterministic step graph through the daemon-backed action boundary, and
// materializes declared artifacts into --out-dir. This is the Flight-Plan
// Launch runtime (#1511); it is distinct from the top-level `aileron launch`
// agent-sandbox command (a different subsystem).
func runSkillLaunch(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill launch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "Frozen version id to launch (defaults to the only/most recent version)")
	outDir := flags.String("out-dir", ".", "Directory file-target artifacts are written to")
	// storeDir defaults to the process store seam. The in-container re-entry on
	// the image-boot path passes the bind-mounted store path here so the inner
	// binary loads the same verified frozen unit from the mount rather than the
	// (empty) default store inside the image.
	storeDir := flags.String("store-dir", skillStoreDir, "Skill store directory (defaults to ~/.aileron/skills)")
	var inputs inputFlag
	flags.Var(&inputs, "input", "Launch input override as name=value; repeatable")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	name := positionals[0]

	s := store.New(*storeDir)
	id, err := resolveLaunchVersion(s, name, *version)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	res, err := runtime.Run(context.Background(), runtime.Options{
		Store:      s,
		Name:       name,
		Version:    id,
		Inputs:     runtime.LaunchArgs(inputs.values),
		Dispatcher: newLaunchDispatcher(),
		Approver:   newLaunchApprover(),
		Audit:      newLaunchAuditSink(stderr),
		// Seam is nil in v1 production: the LLM seam is unwired, so a plan with
		// an llm-seam step errors unless a provider is supplied. Tests inject a
		// deterministic seam through launchSeamForTest.
		Seam: launchSeamForTest,
		// ImageRunner boots the verified pinned rung-1/rung-2 image and runs the
		// plan inside it. When the frozen unit pins no image, the runtime never
		// touches this seam and stays on the in-process path.
		ImageRunner: newLaunchImageRunner(),
		// ToolImageRunner dispatches each rung-3 step to its pinned sibling tool
		// image with mount → run → collect I/O. The plan orchestration stays
		// in-process; only the per-step tool dispatch shells out.
		ToolImageRunner: newLaunchToolImageRunner(),
		OutDir:          *outDir,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Launched %q\n", name)
	fmt.Fprintf(stdout, "  Version:     %s\n", id)
	fmt.Fprintf(stdout, "  ContentHash: %s\n", res.ContentHash)
	if len(res.ResolvedInputs) > 0 {
		fmt.Fprintln(stdout, "  Resolved inputs:")
		for k, v := range res.ResolvedInputs {
			fmt.Fprintf(stdout, "    %s = %v\n", k, v)
		}
	}
	if len(res.Artifacts) > 0 {
		fmt.Fprintln(stdout, "  Artifacts:")
		for _, a := range res.Artifacts {
			if a.Written {
				fmt.Fprintf(stdout, "    %s -> %s  %s\n", a.Name, a.Path, a.Digest)
			} else {
				fmt.Fprintf(stdout, "    %s (retained)  %s\n", a.Name, a.Digest)
			}
		}
	}
	if len(res.AuditIDs) > 0 {
		fmt.Fprintf(stdout, "  Audit records: %d\n", len(res.AuditIDs))
	}
	return 0
}

// resolveLaunchVersion resolves the version id to launch: the explicit
// --version when given, otherwise the single frozen version (or the most
// recent when several exist, since FrozenVersions returns sorted ids).
func resolveLaunchVersion(s *store.Store, name, version string) (string, error) {
	if version != "" {
		return version, nil
	}
	ids, err := s.FrozenVersions(name)
	if err != nil {
		return "", fmt.Errorf("list frozen versions for %q: %w", name, err)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("skill %q has no frozen versions; run `aileron skill freeze %s` first", name, name)
	}
	return ids[len(ids)-1], nil
}

// daemonDispatcher dispatches an action through the daemon's
// POST /v1/actions/{name}/run endpoint. The action ref (aileron:<c>.<a>) maps
// to the daemon action name; credentials are injected host-side at the
// boundary, so the dispatcher never sees them. A 202 (approval pending) is
// surfaced as an error directing the operator to approve, since v1 launch runs
// unattended and does not poll the approval lifecycle.
type daemonDispatcher struct{}

func (daemonDispatcher) Dispatch(ctx context.Context, ref string, args map[string]any) (runtime.DispatchResult, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	name := daemonActionName(ref)
	body, _ := json.Marshal(actionRunRequest{Args: args})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/actions/"+url.PathEscape(name)+"/run", bytes.NewReader(body))
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setDaemonAuthorization(req)
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var out actionRunResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return runtime.DispatchResult{}, fmt.Errorf("decode action result: %w", err)
		}
		// Thread the daemon's non-secret actor provenance into the runtime so
		// the per-output audit record can attribute the produced artifact to
		// the exact connector build and identity (issue #1753). These are
		// references only; credentials are injected host-side and never reach
		// the dispatcher.
		return runtime.DispatchResult{
			Output:            parseResultPayload(out.Result),
			ConnectorVersion:  out.ConnectorVersion,
			ConnectorHash:     out.ConnectorHash,
			IdentityLabel:     out.IdentityLabel,
			CredentialBinding: out.CredentialBinding,
			ConsentDecision:   out.ConsentDecision,
		}, nil
	case http.StatusAccepted:
		return runtime.DispatchResult{}, fmt.Errorf("action %q requires approval; approve it (aileron approval list) and re-launch", ref)
	default:
		return runtime.DispatchResult{}, fmt.Errorf("daemon returned %d for %q: %s", resp.StatusCode, ref, strings.TrimSpace(string(raw)))
	}
}

// daemonActionName maps a manifest action ref (aileron:<connector>.<action>)
// to the daemon action name. v1 uses the bare action segment as the daemon
// name; the connector binding is resolved daemon-side.
func daemonActionName(ref string) string {
	r := strings.TrimPrefix(ref, "aileron:")
	if i := strings.LastIndex(r, "."); i >= 0 {
		return r[i+1:]
	}
	return r
}

// parseResultPayload decodes the daemon's string result into a JSON map so the
// runtime can bind downstream steps against it. A non-JSON or empty result
// surfaces under a "result" key so a downstream binding still resolves.
func parseResultPayload(result *string) map[string]any {
	if result == nil || *result == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*result), &m); err == nil {
		return m
	}
	return map[string]any{"result": *result}
}

// daemonApprover defers to the daemon's own approval gate at the action
// boundary: the daemon's POST /run returns 202 when an action needs approval,
// which the dispatcher surfaces. The runtime-level approver therefore approves
// here so the routing is decided once, at the daemon boundary, rather than
// gated twice. A future interactive launch can replace this seam with one that
// drives the approval lifecycle in-process.
type daemonApprover struct{}

func (daemonApprover) Approve(_ context.Context, _ runtime.ApprovalRequest) (runtime.Decision, error) {
	return runtime.Decision{Approved: true}, nil
}

// daemonAuditSink persists each launch audit record to the daemon's audit
// trail via POST /v1/audit, so launch provenance surfaces in
// `aileron audit list` / `aileron audit show`. The daemon is the single
// writer and single id-minter; this sink translates the runtime's
// AuditRecord into the ingest request and threads the minted audit_id back
// as the Record return value.
//
// The AuditSink SPI's Record returns only a string, so a POST failure is
// best-effort: it logs to stderr and returns "" (the launch continues and
// simply surfaces a lower Audit-records count). This keeps launch
// provenance a companion to the run rather than a hard dependency, matching
// the recorder's own best-effort append discipline (ADR-0010).
type daemonAuditSink struct {
	stderr io.Writer
}

func (s daemonAuditSink) Record(ctx context.Context, rec runtime.AuditRecord) string {
	// The record kind is the explicit discriminator (#1752): an output record
	// carries a flat `aileron.*` payload (top-level attributes, matching the
	// vault.user.credential.* convention), while action/launch records keep
	// their existing nested pay["fields"]/actionRef/sink shape so those event
	// shapes don't churn.
	var eventType string
	var payload map[string]any
	switch rec.Kind {
	case runtime.RecordKindOutput:
		eventType = string(model.EventTypeOutputMaterialized)
		payload = rec.Fields
		if payload == nil {
			// A well-formed output record always carries fields, but guard so an
			// empty record posts an object payload rather than a JSON null.
			payload = map[string]any{}
		}
	case runtime.RecordKindReach:
		// A declared-reach record (#1784) carries the same flat `aileron.*`
		// payload treatment as an output record: the reach attributes surface as
		// top-level keys (including `aileron.reach.enforced: false`), not nested
		// under `fields`. It is audit-only and enforces nothing.
		eventType = string(model.EventTypeFlightPlanLaunchReach)
		payload = rec.Fields
		if payload == nil {
			// A well-formed reach record always carries fields, but guard so an
			// empty record posts an object payload rather than a JSON null.
			payload = map[string]any{}
		}
	case runtime.RecordKindAction:
		eventType = string(model.EventTypeFlightPlanLaunchAction)
		payload = actionOrLaunchPayload(rec)
	default: // runtime.RecordKindLaunch
		eventType = string(model.EventTypeFlightPlanLaunch)
		payload = actionOrLaunchPayload(rec)
	}

	body, err := json.Marshal(auditIngestRequest{
		EventType: eventType,
		Actor:     auditIngestActor{Type: string(model.ActorTypeService), ID: "flightplan-launch"},
		Payload:   payload,
	})
	if err != nil {
		s.logErr("encode audit request: %v", err)
		return ""
	}

	base, err := bindingAPIBaseURL()
	if err != nil {
		s.logErr("resolve daemon URL: %v", err)
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audit", bytes.NewReader(body))
	if err != nil {
		s.logErr("build audit request: %v", err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	setDaemonAuthorization(req)
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		s.logErr("post audit event: %v", err)
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		s.logErr("daemon returned %d for audit append: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		return ""
	}
	var out auditIngestResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		s.logErr("decode audit response: %v", err)
		return ""
	}
	return out.AuditID
}

// actionOrLaunchPayload builds the nested payload shape shared by the
// per-action and per-launch summary records: actionRef and sink when present,
// and the declared audit fields under a "fields" key. The output.materialized
// record does NOT use this shape; it surfaces its flat aileron.* map directly.
func actionOrLaunchPayload(rec runtime.AuditRecord) map[string]any {
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

func (s daemonAuditSink) logErr(format string, args ...any) {
	if s.stderr == nil {
		return
	}
	fmt.Fprintf(s.stderr, "warning: launch audit not recorded: "+format+"\n", args...)
}

// auditIngestRequest mirrors the daemon's AuditIngestRequest schema
// (POST /v1/audit). Kept local to the CLI so the launch path does not
// import the generated server types.
type auditIngestRequest struct {
	EventType string           `json:"event_type"`
	Actor     auditIngestActor `json:"actor"`
	Payload   map[string]any   `json:"payload"`
}

type auditIngestActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type auditIngestResponse struct {
	AuditID string `json:"audit_id"`
}
