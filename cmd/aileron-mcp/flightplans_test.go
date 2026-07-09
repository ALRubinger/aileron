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
	planBody     string // raw body for /launch (used for non-2xx envelopes)
	launchedName string // last launched plan name
	launchedReq  flightPlanLaunchRequest
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

func TestLaunchFlightPlan_GatedActionReturns403ErrorResult(t *testing.T) {
	// A gated constituent action makes the daemon return a 403 FailureEnvelope
	// (#2106). The plan tool surfaces it as an IsError result with the envelope
	// text verbatim. NOTE: #2101 will rewrite this to a 202 passthrough.
	envelope := `{"error":{"class":"capability_denied","boundary":"action","message":"action requires approval"}}`
	d := &fakeDaemon{planStatus: http.StatusForbidden, planBody: envelope}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := &server{aileronURL: srv.URL, httpClient: srv.Client()}
	res := s.launchFlightPlan(context.Background(), "write-plan", nil)
	if !res.IsError {
		t.Fatal("expected IsError result for a 403 gated-action launch")
	}
	if len(res.Content) == 0 || res.Content[0].Text != envelope {
		t.Errorf("result text = %q, want the envelope verbatim", res.Content)
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
