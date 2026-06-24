package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// fakeLaunchDispatcher returns canned per-ref results so the launch runs
// end-to-end without a daemon. It records the refs it was asked to dispatch.
type fakeLaunchDispatcher struct {
	results map[string]map[string]any
	refs    []string
}

func (f *fakeLaunchDispatcher) Dispatch(_ context.Context, ref string, _ map[string]any) (runtime.DispatchResult, error) {
	f.refs = append(f.refs, ref)
	return runtime.DispatchResult{Output: f.results[ref]}, nil
}

// stubLaunchSeams points the CLI launch seams at fakes. The transform that
// materializes digest.csv is supplied via the runtime default registry, so the
// fake dispatcher + a file-map-emitting transform drive the worked example.
func stubLaunchSeams(t *testing.T, disp runtime.ActionDispatcher) {
	t.Helper()
	origD, origA, origS := newLaunchDispatcher, newLaunchApprover, newLaunchAuditSink
	newLaunchDispatcher = func() runtime.ActionDispatcher { return disp }
	newLaunchApprover = func() runtime.Approver { return daemonApprover{} }
	newLaunchAuditSink = func() runtime.AuditSink { return stdoutAuditSink{} }
	t.Cleanup(func() {
		newLaunchDispatcher, newLaunchApprover, newLaunchAuditSink = origD, origA, origS
	})
}

// freezeExampleForLaunch installs + freezes the worked example into the temp
// store so a launch has a verifiable frozen version to load.
func freezeExampleForLaunch(t *testing.T, storeDir string) {
	t.Helper()
	installExample(t, storeDir)
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)
	var out, errOut bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "weekly-metrics-digest"}, &out, &errOut); code != 0 {
		t.Fatalf("freeze failed: %s", errOut.String())
	}
}

func TestRunSkillLaunch_EndToEnd(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)

	// The tracker write materializes filed_issue.json; the metrics read feeds
	// render_csv. The render_csv transform is the default identity, which for
	// this worked example forwards the series; to materialize a CSV file-map
	// the worked example's transform must emit one. We rely on the runtime's
	// action-call materialization for filed_issue.json and skip CSV bytes by
	// having the metrics read return a file-map-shaped series the transform
	// forwards. Simpler: assert the launch loads, verifies, dispatches both
	// actions, and exits 0.
	disp := &fakeLaunchDispatcher{results: map[string]map[string]any{
		"aileron:metrics.query_series": {
			"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "name\ncpu\n",
		},
		"aileron:tracker.create_issue": {
			"path": "filed_issue.json", "mimeType": "application/json", "encoding": "utf-8", "content": "{}",
		},
	}}
	stubLaunchSeams(t, disp)
	// Supply a seam so the llm-seam step runs.
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", outDir, "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Launched \"weekly-metrics-digest\"") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(out, "ContentHash: sha256:") {
		t.Errorf("stdout missing content hash: %q", out)
	}
	// The worked example dispatches the metrics read twice (once for the
	// active_metric_set source input in Phase A, once for the query_metrics
	// step in Phase B) and the tracker write once.
	var metrics, tracker int
	for _, ref := range disp.refs {
		switch ref {
		case "aileron:metrics.query_series":
			metrics++
		case "aileron:tracker.create_issue":
			tracker++
		}
	}
	if metrics != 2 || tracker != 1 {
		t.Errorf("dispatch counts: metrics=%d tracker=%d, want 2/1 (%v)", metrics, tracker, disp.refs)
	}
	// filed_issue.json materialized to the out dir.
	if _, err := os.Stat(filepath.Join(outDir, "filed_issue.json")); err != nil {
		t.Errorf("filed_issue.json not materialized: %v", err)
	}
}

func TestRunSkillLaunch_MissingVersionRefuses(t *testing.T) {
	withTempStore(t)
	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"weekly-metrics-digest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("launching a skill with no frozen versions must fail")
	}
	if !strings.Contains(stderr.String(), "no frozen versions") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunSkillLaunch_InputOverride(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)
	disp := &fakeLaunchDispatcher{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"path": "digest.csv", "encoding": "utf-8", "content": "x", "mimeType": "text/csv"},
		"aileron:tracker.create_issue": {"path": "filed_issue.json", "encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	stubLaunchSeams(t, disp)
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "--input", "window_days=30", "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "window_days = 30") {
		t.Errorf("override not reflected in resolved inputs: %q", stdout.String())
	}
}

func TestRunSkillLaunch_UnknownSkillErrors(t *testing.T) {
	withTempStore(t)
	var stdout, stderr bytes.Buffer
	if code := runSkillLaunch([]string{"ghost"}, &stdout, &stderr); code == 0 {
		t.Fatal("an unknown skill must fail")
	}
}

func TestRunSkillLaunch_BadInputFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runSkillLaunch([]string{"--input", "noequals", "x"}, &stdout, &stderr); code == 0 {
		t.Fatal("a malformed --input must fail")
	}
}

// withDaemon points the CLI's daemon base URL + HTTP client at an httptest
// server so the daemon-backed dispatcher path runs without a live daemon.
func withDaemon(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	origBase := bindingAPIBaseURL
	origClient := actionsHTTPClient
	bindingAPIBaseURL = func() (string, error) { return srv.URL + "/v1", nil }
	actionsHTTPClient = srv.Client()
	t.Cleanup(func() {
		srv.Close()
		bindingAPIBaseURL = origBase
		actionsHTTPClient = origClient
	})
}

func TestDaemonDispatcher_OKResult(t *testing.T) {
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/actions/query_series/run" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"audit_id":"a1","result":"{\"series\":[1]}"}`))
	})
	res, err := daemonDispatcher{}.Dispatch(context.Background(), "aileron:metrics.query_series", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, ok := res.Output["series"]; !ok {
		t.Errorf("result not surfaced: %v", res.Output)
	}
}

func TestDaemonDispatcher_PendingApprovalErrors(t *testing.T) {
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"pending","approval_id":"ap1"}`))
	})
	_, err := daemonDispatcher{}.Dispatch(context.Background(), "aileron:t.create_issue", nil)
	if err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("a 202 pending must surface an approval error, got %v", err)
	}
}

func TestDaemonDispatcher_ServerErrorSurfaces(t *testing.T) {
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	})
	_, err := daemonDispatcher{}.Dispatch(context.Background(), "aileron:m.read", nil)
	if err == nil {
		t.Fatal("a 500 must surface an error")
	}
}

func TestDaemonActionName(t *testing.T) {
	cases := map[string]string{
		"aileron:metrics.query_series": "query_series",
		"aileron:tracker.create_issue": "create_issue",
		"aileron:a.b.c":                "c",
		"bare":                         "bare",
	}
	for ref, want := range cases {
		if got := daemonActionName(ref); got != want {
			t.Errorf("daemonActionName(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestParseResultPayload(t *testing.T) {
	// A JSON object result decodes into a map the runtime can bind against.
	s := `{"series":[1,2]}`
	m := parseResultPayload(&s)
	if _, ok := m["series"]; !ok {
		t.Errorf("JSON result must decode into a map, got %v", m)
	}
	// A non-JSON result surfaces under "result" so a binding still resolves.
	plain := "hello"
	m = parseResultPayload(&plain)
	if m["result"] != "hello" {
		t.Errorf("non-JSON result must surface under result, got %v", m)
	}
	// Nil and empty results yield an empty map, never nil.
	if got := parseResultPayload(nil); got == nil || len(got) != 0 {
		t.Errorf("nil result = %v, want empty map", got)
	}
	empty := ""
	if got := parseResultPayload(&empty); len(got) != 0 {
		t.Errorf("empty result = %v, want empty map", got)
	}
}

// fakeCLISeam is a deterministic seam used by CLI launch tests so the llm-seam
// step produces its declared output.
type fakeCLISeam struct{}

func (fakeCLISeam) Run(_ context.Context, req runtime.SeamRequest) (map[string]any, error) {
	out := map[string]any{}
	for _, name := range req.Outputs {
		out[name] = "summary"
	}
	return out, nil
}
