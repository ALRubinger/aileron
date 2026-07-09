package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeDaemon is a routing httptest server that serves both /v1/actions and
// /v1/flightplans (and captures launch requests), mirroring the daemon door
// aileron-mcp speaks to. It lets the Flight Plan discovery/dispatch tests drive
// a real HTTP surface, matching the action tests' pattern.
type fakeDaemon struct {
	mu           sync.Mutex
	actions      []actionMeta
	plans        []flightPlanMeta
	planStatus   int    // status for /launch; 0 → 200
	planBody     string // raw body for /launch (used for non-2xx envelopes / suspend shapes)
	launchedName string // last launched plan name
	launchedReq  flightPlanLaunchRequest

	// Resume-path capture (#2101).
	resumeStatus int    // status for /resume; 0 → 200
	resumeBody   string // raw body for /resume (suspend shapes or completed result)
	resumedRunID string // last resumed run id
	resumedReq   flightPlanResumeRequest
}

func (d *fakeDaemon) setPlans(plans ...flightPlanMeta) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.plans = plans
}

func (d *fakeDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/actions":
			d.mu.Lock()
			items := d.actions
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(actionListResponse{Items: items})
		case r.URL.Path == "/v1/flightplans":
			d.mu.Lock()
			items := d.plans
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(flightPlanListResponse{Items: items})
		case strings.HasPrefix(r.URL.Path, "/v1/flightplans/runs/") && strings.HasSuffix(r.URL.Path, "/resume"):
			runID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/flightplans/runs/"), "/resume")
			var req flightPlanResumeRequest
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			d.mu.Lock()
			d.resumedRunID = runID
			d.resumedReq = req
			status := d.resumeStatus
			raw := d.resumeBody
			d.mu.Unlock()
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			if raw != "" {
				_, _ = w.Write([]byte(raw))
				return
			}
			_ = json.NewEncoder(w).Encode(flightPlanLaunchResponse{ContentHash: "sha256:resumed"})
		case strings.HasPrefix(r.URL.Path, "/v1/flightplans/") && strings.HasSuffix(r.URL.Path, "/launch"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/flightplans/"), "/launch")
			var req flightPlanLaunchRequest
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			d.mu.Lock()
			d.launchedName = name
			d.launchedReq = req
			status := d.planStatus
			raw := d.planBody
			d.mu.Unlock()
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			if raw != "" {
				_, _ = w.Write([]byte(raw))
				return
			}
			_ = json.NewEncoder(w).Encode(flightPlanLaunchResponse{
				ContentHash:    "sha256:deadbeef",
				ResolvedInputs: map[string]any{"echo": "ok"},
			})
		default:
			http.NotFound(w, r)
		}
	})
}

func requiredPtr(v bool) *bool { return &v }

func toolByName(tools []toolDef, name string) (toolDef, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return toolDef{}, false
}

func TestDiscoverFlightPlans_Success(t *testing.T) {
	d := &fakeDaemon{}
	d.setPlans(flightPlanMeta{
		Name:        "weekly-digest",
		Version:     "v1",
		Description: "Build the weekly metrics digest.",
		Inputs: []flightPlanInput{
			{Name: "query", Type: "string", Description: "search", Required: requiredPtr(true)},
		},
		Outputs: []flightPlanOutput{{Name: "report.json"}},
	})
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	tools, nameMap, err := s.discoverFlightPlans(context.Background(), nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(tools))
	}
	if nameMap["weekly_digest"] != "weekly-digest" {
		t.Errorf("nameMap[weekly_digest] = %q, want weekly-digest", nameMap["weekly_digest"])
	}
	td := tools[0]
	if td.Name != "weekly_digest" {
		t.Errorf("tool name = %q, want weekly_digest (snake_case)", td.Name)
	}
	if !strings.Contains(td.Description, "Build the weekly metrics digest.") {
		t.Errorf("description missing manifest text: %q", td.Description)
	}
	if !strings.Contains(td.Description, "report.json") {
		t.Errorf("description missing declared output: %q", td.Description)
	}
}

func TestDeriveFlightPlanInputSchema_TimestampProjectsToString(t *testing.T) {
	p := flightPlanMeta{
		Name: "p",
		Inputs: []flightPlanInput{
			{Name: "as_of", Type: "timestamp", Required: requiredPtr(false)},
		},
	}
	sc := deriveFlightPlanInputSchema(p)
	prop, ok := sc.Properties["as_of"]
	if !ok {
		t.Fatal("as_of property missing")
	}
	if prop.Type != "string" {
		t.Errorf("as_of type = %q, want string (timestamp projected)", prop.Type)
	}
}

func TestDeriveFlightPlanInputSchema_ArrayAndObject(t *testing.T) {
	p := flightPlanMeta{
		Name: "p",
		Inputs: []flightPlanInput{
			{Name: "tags", Type: "array", Required: requiredPtr(false)},
			{Name: "filters", Type: "object", Required: requiredPtr(false)},
		},
	}
	sc := deriveFlightPlanInputSchema(p)
	tags := sc.Properties["tags"]
	if tags.Type != "array" {
		t.Errorf("tags type = %q, want array", tags.Type)
	}
	// Array always carries an `items` clause (Codex-permissive), even without
	// items_type.
	if tags.Items == nil {
		t.Error("array input missing items clause")
	}
	filters := sc.Properties["filters"]
	if filters.Type != "object" {
		t.Errorf("filters type = %q, want object", filters.Type)
	}
}

func TestDeriveFlightPlanInputSchema_RequiredReflected(t *testing.T) {
	p := flightPlanMeta{
		Name: "p",
		Inputs: []flightPlanInput{
			{Name: "req", Type: "string", Required: requiredPtr(true)},
			{Name: "opt", Type: "string", Required: requiredPtr(false)},
		},
	}
	sc := deriveFlightPlanInputSchema(p)
	if len(sc.Required) != 1 || sc.Required[0] != "req" {
		t.Errorf("required = %v, want [req]", sc.Required)
	}
}

func TestLaunchFlightPlan_HappyPath(t *testing.T) {
	d := &fakeDaemon{}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	res := s.launchFlightPlan(context.Background(), "weekly-digest", map[string]any{"query": "metrics"})
	if res.IsError {
		t.Fatalf("launch returned error result: %v", res)
	}
	// The daemon received the inputs under {inputs:{...}}.
	d.mu.Lock()
	gotName := d.launchedName
	gotReq := d.launchedReq
	d.mu.Unlock()
	if gotName != "weekly-digest" {
		t.Errorf("launched name = %q, want weekly-digest", gotName)
	}
	if gotReq.Inputs["query"] != "metrics" {
		t.Errorf("launched inputs = %v, want query=metrics", gotReq.Inputs)
	}
	// The response text carries the content hash.
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "sha256:deadbeef") {
		t.Errorf("result text missing content hash: %v", res.Content)
	}
}

func TestLaunchFlightPlan_GatedActionReturns202Pending(t *testing.T) {
	// Superseding #2098's 403 fail-closed (#2101): a gated constituent action now
	// suspends with a 202 pending_approval. The plan tool surfaces the pending
	// message (not an IsError) so the agent knows to approve and resume.
	pendingBody := `{"status":"pending_approval","approval_id":"appr-1","review_url":"https://app/approvals?focus=appr-1","message":"Approval needed for file_report."}`
	d := &fakeDaemon{planStatus: http.StatusAccepted, planBody: pendingBody}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	res := s.launchFlightPlan(context.Background(), "write-plan", nil)
	if res.IsError {
		t.Fatalf("a 202 pending_approval must not be an IsError result: %v", res)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "Approval needed for file_report.") {
		t.Errorf("result text missing the pending message: %v", res.Content)
	}
}

func TestLaunchFlightPlan_SeamPendingSurfacesEnvelope(t *testing.T) {
	// A launch that suspends at a seam returns 200 seam_pending. The plan tool
	// surfaces the seam envelope verbatim (run_id + declared outputs) so the agent
	// knows to call resume_flight_plan.
	seamBody := `{"status":"seam_pending","run_id":"run-42","seam":{"step_id":"summarize","prompt":"Summarize {{ steps.read.series }}","model":"anthropic:claude-haiku-4-5","outputs":["summary"]}}`
	d := &fakeDaemon{planStatus: http.StatusOK, planBody: seamBody}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	res := s.launchFlightPlan(context.Background(), "seam-plan", nil)
	if res.IsError {
		t.Fatalf("a seam_pending result must not be an IsError: %v", res)
	}
	text := res.Content[0].Text
	for _, want := range []string{"seam_pending", "run-42", "summarize", "summary"} {
		if !strings.Contains(text, want) {
			t.Errorf("seam_pending result text missing %q: %s", want, text)
		}
	}
}

func TestResumeFlightPlan_SeamOutputsCompletesRun(t *testing.T) {
	// resume_flight_plan with the seam's outputs POSTs to the resume endpoint and
	// surfaces the completed launch result.
	d := &fakeDaemon{resumeStatus: http.StatusOK} // default completed body
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	res := s.resumeFlightPlan(context.Background(), map[string]any{
		"run_id": "run-42",
		"outputs": map[string]any{
			"summarize": map[string]any{"summary": "a short summary"},
		},
	})
	if res.IsError {
		t.Fatalf("resume returned error: %v", res)
	}
	d.mu.Lock()
	gotRun := d.resumedRunID
	gotReq := d.resumedReq
	d.mu.Unlock()
	if gotRun != "run-42" {
		t.Errorf("resumed run id = %q, want run-42", gotRun)
	}
	if gotReq.Outputs["summarize"]["summary"] != "a short summary" {
		t.Errorf("resumed outputs = %v, want the seam output", gotReq.Outputs)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "sha256:resumed") {
		t.Errorf("resume result missing completed content hash: %v", res.Content)
	}
}

func TestResumeFlightPlan_ChainsToNextSeam(t *testing.T) {
	// A resume that lands on the next seam surfaces another seam_pending envelope.
	nextSeam := `{"status":"seam_pending","run_id":"run-7","seam":{"step_id":"seam_b","outputs":["body"]}}`
	d := &fakeDaemon{resumeStatus: http.StatusOK, resumeBody: nextSeam}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	res := s.resumeFlightPlan(context.Background(), map[string]any{
		"run_id":  "run-7",
		"outputs": map[string]any{"seam_a": map[string]any{"summary": "S"}},
	})
	if res.IsError {
		t.Fatalf("resume to next seam must not be IsError: %v", res)
	}
	if !strings.Contains(res.Content[0].Text, "seam_b") {
		t.Errorf("resume result missing the next seam step: %v", res.Content)
	}
}

func TestResumeFlightPlan_DeniedApproval403IsError(t *testing.T) {
	// A denied approval fails the run closed: the resume returns 403 and the tool
	// surfaces the FailureEnvelope as an IsError result.
	envelope := `{"error":{"class":"capability_denied","boundary":"action","message":"approval denied"}}`
	d := &fakeDaemon{resumeStatus: http.StatusForbidden, resumeBody: envelope}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	res := s.resumeFlightPlan(context.Background(), map[string]any{"run_id": "run-9"})
	if !res.IsError {
		t.Fatal("a denied-approval 403 resume must be an IsError result")
	}
	if res.Content[0].Text != envelope {
		t.Errorf("resume result = %q, want the envelope verbatim", res.Content[0].Text)
	}
}

func TestResumeFlightPlan_MissingRunID(t *testing.T) {
	s := &server{aileronURL: "http://unused"}
	res := s.resumeFlightPlan(context.Background(), map[string]any{})
	if !res.IsError {
		t.Fatal("resume with no run_id must be an IsError result")
	}
}

func TestResumeFlightPlan_InBuiltinNamesAndToolsList(t *testing.T) {
	// resume_flight_plan is a static built-in (a plan/action can never shadow it)
	// and always appears in the available tools.
	if _, ok := staticBuiltinToolNames["resume_flight_plan"]; !ok {
		t.Error("resume_flight_plan missing from staticBuiltinToolNames")
	}
	s := &server{aileronURL: "http://daemon"}
	if _, ok := toolByName(s.availableTools(), "resume_flight_plan"); !ok {
		t.Error("resume_flight_plan missing from availableTools()")
	}
}

func TestDispatchTool_RoutesFlightPlan(t *testing.T) {
	d := &fakeDaemon{}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{
		aileronURL:        srv.URL,
		httpClient:        srv.Client(),
		flightPlanNameMap: map[string]string{"weekly_digest": "weekly-digest"},
	}
	res := s.dispatchTool(context.Background(), "weekly_digest", map[string]any{"a": "b"})
	if res.IsError {
		t.Fatalf("dispatch returned error: %v", res)
	}
	d.mu.Lock()
	got := d.launchedName
	d.mu.Unlock()
	if got != "weekly-digest" {
		t.Errorf("launched name = %q, want weekly-digest", got)
	}
}

func TestDiscoverFlightPlans_CollisionWithBuiltinDropped(t *testing.T) {
	// A plan normalizing to a static built-in name is dropped; the built-in wins.
	d := &fakeDaemon{}
	d.setPlans(
		flightPlanMeta{Name: "read-messages", Version: "v1"},       // collides with read_messages
		flightPlanMeta{Name: "aileron-diagnostics", Version: "v1"}, // collides with aileron_diagnostics
		flightPlanMeta{Name: "safe-plan", Version: "v1"},
	)
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	tools, nameMap, err := s.discoverFlightPlans(context.Background(), nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, ok := nameMap["read_messages"]; ok {
		t.Error("plan colliding with read_messages built-in was not dropped")
	}
	if _, ok := nameMap["aileron_diagnostics"]; ok {
		t.Error("plan colliding with aileron_diagnostics built-in was not dropped")
	}
	if _, ok := toolByName(tools, "safe_plan"); !ok {
		t.Error("non-colliding plan safe_plan missing")
	}
	if len(tools) != 1 {
		t.Errorf("len(tools) = %d, want 1 (only safe_plan)", len(tools))
	}
}

func TestDiscoverFlightPlans_CollisionWithActionDropped(t *testing.T) {
	// A plan normalizing to a discovered action's tool name is dropped; the
	// action wins.
	d := &fakeDaemon{}
	d.setPlans(
		flightPlanMeta{Name: "ship-update", Version: "v1"}, // collides with action ship_update
		flightPlanMeta{Name: "plan-only", Version: "v1"},
	)
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	actionNames := map[string]string{"ship_update": "ship-update"}
	tools, nameMap, err := s.discoverFlightPlans(context.Background(), actionNames)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, ok := nameMap["ship_update"]; ok {
		t.Error("plan colliding with a discovered action was not dropped")
	}
	if _, ok := nameMap["plan_only"]; !ok {
		t.Error("non-colliding plan-only missing")
	}
	if len(tools) != 1 {
		t.Errorf("len(tools) = %d, want 1 (only plan_only)", len(tools))
	}
}

func TestRefreshOnce_EmitsOnPlanAdded(t *testing.T) {
	d := &fakeDaemon{actions: []actionMeta{{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()}}}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	// Seed the startup surface (actions + no plans).
	if !s.refreshOnce(context.Background()) {
		// First refresh from an empty cache surfaces the action → one emit.
	}
	baseline := countListChanged(t, out.String())

	// Install a plan mid-session.
	d.setPlans(flightPlanMeta{Name: "weekly-digest", Version: "v1"})
	if !s.refreshOnce(context.Background()) {
		t.Fatal("refreshOnce returned false; expected a change (plan added)")
	}
	if got := countListChanged(t, out.String()) - baseline; got != 1 {
		t.Fatalf("list_changed emissions for plan add = %d, want 1", got)
	}
	// The plan is now routable.
	s.actionsMu.RLock()
	_, ok := s.flightPlanNameMap["weekly_digest"]
	s.actionsMu.RUnlock()
	if !ok {
		t.Error("added plan not present in flightPlanNameMap after refresh")
	}
}

func TestRefreshOnce_PlanFailureLeavesActionSurfaceIntact(t *testing.T) {
	// /v1/flightplans errors while /v1/actions is healthy: the action surface
	// and its diagnostic stay intact, and no spurious list_changed is emitted
	// once the surfaces are steady.
	d := &fakeDaemon{actions: []actionMeta{{Name: "ship-update", Body: "# Ship", Enabled: enabledPtr()}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/flightplans" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		d.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	out := &syncBuffer{}
	s := &server{aileronURL: srv.URL, httpClient: srv.Client(), out: out}
	// First refresh: action surface appears (one emit); plan discovery fails.
	if !s.refreshOnce(context.Background()) {
		t.Fatal("first refreshOnce returned false; expected the action surface to appear")
	}
	// Action surface is intact.
	s.actionsMu.RLock()
	_, actionOK := s.actionNameMap["ship_update"]
	planCount := len(s.flightPlanTools)
	s.actionsMu.RUnlock()
	if !actionOK {
		t.Error("action ship_update missing despite healthy action surface")
	}
	if planCount != 0 {
		t.Errorf("plan tools = %d, want 0 (plan discovery failed)", planCount)
	}
	// Action diagnostic reports OK (plan failure did not corrupt it).
	if !s.discoveryState().ok() {
		t.Error("action diagnostic degraded by a plan-only failure")
	}
	// A second steady refresh emits nothing (no spurious list_changed).
	baseline := countListChanged(t, out.String())
	if s.refreshOnce(context.Background()) {
		t.Fatal("second refreshOnce returned true; expected no change")
	}
	if got := countListChanged(t, out.String()) - baseline; got != 0 {
		t.Errorf("spurious list_changed emissions = %d, want 0", got)
	}
}

func TestDiscoverFlightPlans_HTTPErrorClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	_, _, err := s.discoverFlightPlans(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	var de *discoveryError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want a *discoveryError", err)
	}
}

func TestDiscoverFlightPlans_Unreachable(t *testing.T) {
	s := &server{aileronURL: "http://127.0.0.1:1", httpClient: &http.Client{}}
	_, _, err := s.discoverFlightPlans(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error for an unreachable daemon")
	}
	var de *discoveryError
	if !errors.As(err, &de) || de.reason != reasonUnreachable {
		t.Fatalf("err = %v, want reasonUnreachable", err)
	}
}
