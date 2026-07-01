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

// fakeLaunchImageRunner records the exact image string and spec it was handed
// and returns a canned result, so the boot path is testable with no live Docker.
type fakeLaunchImageRunner struct {
	called bool
	spec   runtime.ImageRunSpec
	result runtime.ImageRunResult
	err    error
}

func (f *fakeLaunchImageRunner) Run(_ context.Context, spec runtime.ImageRunSpec) (runtime.ImageRunResult, error) {
	f.called = true
	f.spec = spec
	return f.result, f.err
}

// stubLaunchImageRunner swaps the production image-runner seam for a fake so a
// rung-pinned launch never boots a real container. It returns the fake for
// assertions. When runner is nil the seam yields nil so the runtime's
// pinned-but-no-runner guard fires.
func stubLaunchImageRunner(t *testing.T, runner *fakeLaunchImageRunner) *fakeLaunchImageRunner {
	t.Helper()
	orig := newLaunchImageRunner
	newLaunchImageRunner = func() runtime.ImageRunner {
		if runner == nil {
			return nil
		}
		return runner
	}
	t.Cleanup(func() { newLaunchImageRunner = orig })
	return runner
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

// TestRunSkillLaunch_BootsPinnedRung2Image proves the acceptance property: the
// worked example is rung-2, so its verified lock pins a composed image digest.
// Launch boots that exact ref@sha256:<digest-from-lock> through the image runner
// and exits 0. The recorded image is the load-bearing assertion that the lock's
// signed-image claim corresponds to the image actually entered.
// freezeNoImageForLaunch installs a no-execution-environment variant of the
// worked example (the same stepped plan with the executionEnvironment block
// removed) and freezes it, so the frozen unit pins no image and the launch
// stays on the in-process path.
func freezeNoImageForLaunch(t *testing.T, storeDir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootForTest(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	stripped := stripExecEnvBlock(t, string(raw))
	dir := filepath.Join(storeDir, "weekly-metrics-digest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)
	var out, errOut bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "weekly-metrics-digest"}, &out, &errOut); code != 0 {
		t.Fatalf("freeze no-image variant failed: %s", errOut.String())
	}
}

// stripExecEnvBlock removes the `executionEnvironment:` mapping (indented at 4
// spaces) and its more-deeply-indented children from the worked-example
// frontmatter, leaving a valid no-rung manifest.
func stripExecEnvBlock(t *testing.T, md string) string {
	t.Helper()
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, ln := range lines {
		if !skipping {
			if strings.HasPrefix(ln, "    executionEnvironment:") && strings.TrimSpace(ln) == "executionEnvironment:" {
				skipping = true
				continue
			}
			out = append(out, ln)
			continue
		}
		trimmed := strings.TrimLeft(ln, " ")
		indent := len(ln) - len(trimmed)
		if trimmed == "" || indent > 4 {
			continue
		}
		skipping = false
		out = append(out, ln)
	}
	res := strings.Join(out, "\n")
	if strings.Contains(res, "executionEnvironment:") {
		t.Fatal("stripExecEnvBlock left the block in place")
	}
	return res
}

func TestRunSkillLaunch_BootsPinnedRung2Image(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)

	runner := stubLaunchImageRunner(t, &fakeLaunchImageRunner{
		result: runtime.ImageRunResult{ContentHash: "sha256:booted"},
	})
	// The dispatcher must never be walked on the boot path; wire it so a stray
	// in-process dispatch would be observable.
	disp := &fakeLaunchDispatcher{results: map[string]map[string]any{}}
	stubLaunchSeams(t, disp)

	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", outDir, "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if !runner.called {
		t.Fatal("launch did not boot the pinned image for a rung-2 unit")
	}
	if !strings.HasSuffix(runner.spec.Image, "@"+fakeFreezeDigest) {
		t.Errorf("booted image = %q, want it to end with @%s (the verified lock digest)", runner.spec.Image, fakeFreezeDigest)
	}
	if runner.spec.Name != "weekly-metrics-digest" {
		t.Errorf("spec.Name = %q, want the launched skill name", runner.spec.Name)
	}
	if runner.spec.OutDir != outDir {
		t.Errorf("spec.OutDir = %q, want %q", runner.spec.OutDir, outDir)
	}
	if len(disp.refs) != 0 {
		t.Errorf("boot path must not dispatch in-process, got %v", disp.refs)
	}
	if !strings.Contains(stdout.String(), "Launched \"weekly-metrics-digest\"") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestRunSkillLaunch_InProcessWhenNoRung proves parity: a frozen unit that pins
// no image stays on the in-process path. The image runner is never touched and
// the dispatcher walks the step graph, materializing artifacts as before.
func TestRunSkillLaunch_InProcessWhenNoRung(t *testing.T) {
	storeDir := withTempStore(t)
	freezeNoImageForLaunch(t, storeDir)

	disp := &fakeLaunchDispatcher{results: map[string]map[string]any{
		"aileron:metrics.query_series": {
			"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "name\ncpu\n",
		},
		"aileron:tracker.create_issue": {
			"path": "filed_issue.json", "mimeType": "application/json", "encoding": "utf-8", "content": "{}",
		},
	}}
	stubLaunchSeams(t, disp)
	runner := stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", outDir, "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if runner.called {
		t.Fatal("a unit that pins no image must not boot the image runner")
	}
	if !strings.Contains(stdout.String(), "ContentHash: sha256:") {
		t.Errorf("stdout missing content hash: %q", stdout.String())
	}
	// The worked example dispatches the metrics read twice (Phase A source input
	// + Phase B step) and the tracker write once.
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
	if _, err := os.Stat(filepath.Join(outDir, "filed_issue.json")); err != nil {
		t.Errorf("filed_issue.json not materialized: %v", err)
	}
}

// TestRunSkillLaunch_PinnedButNoRunnerErrors proves the guard fires through the
// CLI: a rung-pinned unit with a misconfigured (nil) image runner refuses with
// a non-zero exit and a clear error, never silently running in-process.
func TestRunSkillLaunch_PinnedButNoRunnerErrors(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)

	stubLaunchImageRunner(t, nil) // nil runner: the guard must fire.
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("a rung-pinned unit with no image runner must exit non-zero")
	}
	if !strings.Contains(stderr.String(), "no image runner is configured") {
		t.Errorf("stderr = %q, want a no-runner-configured message", stderr.String())
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

// TestRunSkillLaunch_InputOverrideReachesImageRunner proves a --input override
// threads into the boot path: the worked example is rung-2, so the override must
// reach the image runner's spec (which carries it into the in-container run)
// rather than being dropped at the boot boundary.
func TestRunSkillLaunch_InputOverrideReachesImageRunner(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)

	runner := stubLaunchImageRunner(t, &fakeLaunchImageRunner{
		result: runtime.ImageRunResult{ResolvedInputs: map[string]any{"window_days": "30"}},
	})
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "--input", "window_days=30", "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if runner.spec.Inputs["window_days"] != "30" {
		t.Errorf("input override not threaded into the image spec: %v", runner.spec.Inputs)
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
