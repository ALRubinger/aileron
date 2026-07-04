package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/model"
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

// fakeAuditSink records the audit records it receives and returns a
// deterministic id, keeping launch tests that aren't exercising the daemon
// audit path hermetic (no POST attempts against a base URL).
type fakeAuditSink struct {
	records []runtime.AuditRecord
}

func (f *fakeAuditSink) Record(_ context.Context, rec runtime.AuditRecord) string {
	f.records = append(f.records, rec)
	if rec.ActionRef != "" {
		return "fake-audit-" + rec.ActionRef
	}
	return "fake-audit-summary"
}

// stubLaunchSeams points the CLI launch seams at fakes. The transform that
// materializes digest.csv is supplied via the runtime default registry, so the
// fake dispatcher + a file-map-emitting transform drive the worked example.
//
// By default the audit sink is a hermetic recording fake so tests that don't
// wire a fake daemon never attempt a POST. Passing useRealAudit=true leaves
// the production daemon-backed sink in place so the acceptance path
// (runSkillLaunch → sink → fake daemon → back) can be exercised via withDaemon.
func stubLaunchSeams(t *testing.T, disp runtime.ActionDispatcher, useRealAudit ...bool) {
	t.Helper()
	origD, origA, origS := newLaunchDispatcher, newLaunchApprover, newLaunchAuditSink
	newLaunchDispatcher = func() runtime.ActionDispatcher { return disp }
	newLaunchApprover = func() runtime.Approver { return daemonApprover{} }
	if !(len(useRealAudit) > 0 && useRealAudit[0]) {
		newLaunchAuditSink = func(io.Writer) runtime.AuditSink { return &fakeAuditSink{} }
	}
	// The composed-tools boot-time Id-vs-Digest guard (#1863) consults the
	// production container-backed digest resolver, which would shell out to
	// `docker image inspect`. Default it to nil in launch tests (the guard's
	// backward-compatible no-op path) so a launch that boots a fake image runner
	// never touches Docker. Tests exercising the guard end-to-end opt in via
	// stubLaunchImageDigestResolver.
	origR := newLaunchImageDigestResolver
	newLaunchImageDigestResolver = func() runtime.LocalImageDigestResolver { return nil }
	t.Cleanup(func() {
		newLaunchDispatcher, newLaunchApprover, newLaunchAuditSink = origD, origA, origS
		newLaunchImageDigestResolver = origR
	})
}

// stubLaunchImageDigestResolver swaps the production digest-resolver seam for a
// fake so a composed-pin launch exercises the boot-time guard (#1863) end to
// end with no live Docker. Call it AFTER stubLaunchSeams (which defaults the
// resolver to nil), so the fake wins.
func stubLaunchImageDigestResolver(t *testing.T, resolver runtime.LocalImageDigestResolver) {
	t.Helper()
	orig := newLaunchImageDigestResolver
	newLaunchImageDigestResolver = func() runtime.LocalImageDigestResolver { return resolver }
	t.Cleanup(func() { newLaunchImageDigestResolver = orig })
}

// stubLaunchDigestResolverFake is a launch-test digest resolver double: it
// returns a canned digest or error and records the tag it was asked about.
type stubLaunchDigestResolverFake struct {
	digest   string
	err      error
	gotImage string
}

func (f *stubLaunchDigestResolverFake) Resolve(_ context.Context, image string) (string, error) {
	f.gotImage = image
	return f.digest, f.err
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
// environment-pinned launch never boots a real container. It returns the fake for
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

// freezeNoImageForLaunch installs a no-environment variant of the worked
// example (the same stepped plan with the environment block removed) and
// freezes it, so the frozen unit pins no image and the launch stays on the
// in-process path.
func freezeNoImageForLaunch(t *testing.T, storeDir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootForTest(t), "docs", "schema", "flight-plan-manifest.example.skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	stripped := stripEnvironmentBlock(t, string(raw))
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

// stripEnvironmentBlock removes the `environment:` mapping (indented at 2
// spaces under the aileron block) and its more-deeply-indented children from
// the worked-example frontmatter, leaving a valid no-environment manifest.
func stripEnvironmentBlock(t *testing.T, md string) string {
	t.Helper()
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, ln := range lines {
		if !skipping {
			if strings.HasPrefix(ln, "  environment:") && strings.TrimSpace(ln) == "environment:" {
				skipping = true
				continue
			}
			out = append(out, ln)
			continue
		}
		trimmed := strings.TrimLeft(ln, " ")
		indent := len(ln) - len(trimmed)
		if trimmed == "" || indent > 2 {
			continue
		}
		skipping = false
		out = append(out, ln)
	}
	res := strings.Join(out, "\n")
	if strings.Contains(res, "environment:") {
		t.Fatal("stripEnvironmentBlock left the block in place")
	}
	return res
}

// TestRunSkillLaunch_BootsPinnedEnvironmentImage proves the acceptance
// property: the worked example declares environment tools, so its verified
// lock pins a composed image carrying a bootable local-daemon tag (#1856).
// Launch boots that local tag through the image runner and exits 0. The
// recorded image is the load-bearing assertion that the lock's signed
// composed-tools claim corresponds to the daemon-resolvable image actually
// entered, not the unbootable descriptive ref@digest join.
func TestRunSkillLaunch_BootsPinnedEnvironmentImage(t *testing.T) {
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
		t.Fatal("launch did not boot the pinned image for an environment unit")
	}
	if !strings.HasPrefix(runner.spec.Image, "aileron/sandbox-tools:") {
		t.Errorf("booted image = %q, want the composed pin's bootable local-daemon tag", runner.spec.Image)
	}
	if strings.Contains(runner.spec.Image, "@"+fakeFreezeDigest) || strings.Contains(runner.spec.Image, "+tools(") {
		t.Errorf("booted the unbootable descriptive ref@digest join %q, want the local tag", runner.spec.Image)
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

// TestRunSkillLaunch_GuardBootsOnDigestMatch proves the boot-time Id-vs-Digest
// guard (#1863) end to end through the CLI: a composed-tools unit whose local
// tag resolves in the daemon to the exact attested digest boots normally. The
// resolver is consulted with the composed local tag and the launch exits 0.
func TestRunSkillLaunch_GuardBootsOnDigestMatch(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)

	runner := stubLaunchImageRunner(t, &fakeLaunchImageRunner{
		result: runtime.ImageRunResult{ContentHash: "sha256:booted"},
	})
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})
	// The composed pin's Digest is fakeFreezeDigest (the composer's returned
	// digest); a resolver returning it makes the guard pass.
	resolver := &stubLaunchDigestResolverFake{digest: fakeFreezeDigest}
	stubLaunchImageDigestResolver(t, resolver)

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.HasPrefix(resolver.gotImage, "aileron/sandbox-tools:") {
		t.Errorf("guard resolved %q, want the composed local tag", resolver.gotImage)
	}
	if !runner.called {
		t.Fatal("a matching guard must boot the image runner")
	}
}

// TestRunSkillLaunch_GuardFailsClosedOnDigestMismatch proves the guard fails the
// whole launch closed (#1863) when the composed local tag resolves to a
// different digest than the signed lock attested: the image runner is NEVER
// booted, the CLI exits non-zero, and stderr names the mismatch.
func TestRunSkillLaunch_GuardFailsClosedOnDigestMismatch(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)

	runner := stubLaunchImageRunner(t, &fakeLaunchImageRunner{
		result: runtime.ImageRunResult{ContentHash: "sha256:booted"},
	})
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})
	// A resolver returning a DIFFERENT digest than the attested fakeFreezeDigest
	// makes the guard fire.
	otherDigest := "sha256:" + strings.Repeat("e", 64)
	stubLaunchImageDigestResolver(t, &stubLaunchDigestResolverFake{digest: otherDigest})

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("a digest mismatch must fail the launch closed, exit=%d stdout=%s", code, stdout.String())
	}
	if runner.called {
		t.Fatal("a mismatched guard must NOT boot the image runner")
	}
	if !strings.Contains(stderr.String(), otherDigest) || !strings.Contains(stderr.String(), "refusing to boot") {
		t.Errorf("stderr must name the mismatch and the refusal, got %q", stderr.String())
	}
}

// TestRunSkillLaunch_InProcessWhenNoEnvironment proves parity: a frozen unit that pins
// no image stays on the in-process path. The image runner is never touched and
// the dispatcher walks the step graph, materializing artifacts as before.
func TestRunSkillLaunch_InProcessWhenNoEnvironment(t *testing.T) {
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
// CLI: an environment-pinned unit with a misconfigured (nil) image runner refuses with
// a non-zero exit and a clear error, never silently running in-process.
func TestRunSkillLaunch_PinnedButNoRunnerErrors(t *testing.T) {
	storeDir := withTempStore(t)
	freezeExampleForLaunch(t, storeDir)

	stubLaunchImageRunner(t, nil) // nil runner: the guard must fire.
	stubLaunchSeams(t, &fakeLaunchDispatcher{results: map[string]map[string]any{}})

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("an environment-pinned unit with no image runner must exit non-zero")
	}
	if !strings.Contains(stderr.String(), "no image runner is configured") {
		t.Errorf("stderr = %q, want a no-runner-configured message", stderr.String())
	}
}

// TestRunSkillLaunch_StoreDirFlagRoutesStore proves --store-dir points the
// launch at an explicit store rather than the default seam. The frozen unit
// lives only under the flag's directory while the process store seam is empty,
// so a successful load can only come from honoring the flag. This is the
// mechanism the in-container re-entry relies on to read the bind-mounted store.
func TestRunSkillLaunch_StoreDirFlagRoutesStore(t *testing.T) {
	// Freeze the unit into flagStore (freeze reads the process seam, so point it
	// there for the freeze), then swap the seam to a different empty store so
	// only the --store-dir flag can resolve the unit at launch.
	flagStore := withTempStore(t)
	freezeNoImageForLaunch(t, flagStore)
	skillStoreDir = t.TempDir()

	disp := &fakeLaunchDispatcher{results: map[string]map[string]any{
		"aileron:metrics.query_series": {
			"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "name\ncpu\n",
		},
		"aileron:tracker.create_issue": {
			"path": "filed_issue.json", "mimeType": "application/json", "encoding": "utf-8", "content": "{}",
		},
	}}
	stubLaunchSeams(t, disp)
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--store-dir", flagStore, "--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch with --store-dir exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Launched \"weekly-metrics-digest\"") {
		t.Errorf("stdout = %q", stdout.String())
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
// threads into the boot path: the worked example pins an environment image, so the override must
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

func TestDaemonDispatcher_SurfacesActorProvenance(t *testing.T) {
	// The daemon's 200 run response carries actor provenance (#1753); the
	// launch dispatcher threads it onto the runtime DispatchResult so a
	// materialized output's audit record can attribute the connector build
	// and identity that produced it.
	withDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"audit_id":"a1",
			"result":"{\"QueryExecutionId\":\"qeid-1\"}",
			"connector_version":"2.3.1",
			"connector_hash":"sha256:conn",
			"identity_label":"work",
			"credential_binding":"aws_sigv4/athena/work",
			"consent_decision":"unattended"
		}`))
	})
	res, err := daemonDispatcher{}.Dispatch(context.Background(), "aileron:athena.query", nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.ConnectorVersion != "2.3.1" || res.ConnectorHash != "sha256:conn" {
		t.Errorf("connector build = %q/%q, want 2.3.1/sha256:conn", res.ConnectorVersion, res.ConnectorHash)
	}
	if res.IdentityLabel != "work" || res.CredentialBinding != "aws_sigv4/athena/work" {
		t.Errorf("identity/binding = %q/%q", res.IdentityLabel, res.CredentialBinding)
	}
	if res.ConsentDecision != "unattended" {
		t.Errorf("consent = %q, want unattended", res.ConsentDecision)
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
	// A plain JSON object result decodes into a map the runtime can bind
	// against, passed through unchanged (no dispatch envelope to unwrap).
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

// TestParseResultPayload_UnwrapsDispatchEnvelope is the #1801 regression: the
// daemon's action executor returns the dispatch envelope
// {"action", "output", "steps"} as the result string. parseResultPayload must
// return the inner output map — not the whole envelope — so the runtime binds
// steps.<id>.result to the real result (no action/steps keys nested in the
// materialized artifact, and the audit query-execution-id lift fires because
// QueryExecutionId sits at the top level of the bound value).
func TestParseResultPayload_UnwrapsDispatchEnvelope(t *testing.T) {
	env := `{"action":"query","output":{"QueryExecutionId":"qeid-1","ResultSet":{"rows":[1]}},"steps":{"run":{"QueryExecutionId":"qeid-1"}}}`
	m := parseResultPayload(&env)
	if m["QueryExecutionId"] != "qeid-1" {
		t.Errorf("envelope must unwrap to inner output, got %v", m)
	}
	if _, ok := m["ResultSet"]; !ok {
		t.Errorf("inner output fields must be present at top level, got %v", m)
	}
	// The outer envelope keys must NOT leak into the bound value.
	for _, leaked := range []string{"action", "steps", "output"} {
		if _, ok := m[leaked]; ok {
			t.Errorf("envelope key %q must not leak into bound result, got %v", leaked, m)
		}
	}
}

// TestParseResultPayload_PassthroughNonEnvelope proves the unwrap only fires for
// the real envelope shape. A StubExecutor-shaped result carries "action" but no
// "output" object, and a plain result carries neither; both must pass through
// unchanged so no non-envelope payload is mistaken for the envelope.
func TestParseResultPayload_PassthroughNonEnvelope(t *testing.T) {
	// StubExecutor shape: "action" present, "output" absent.
	stub := `{"executed":false,"stub":true,"action":"query","args":{}}`
	m := parseResultPayload(&stub)
	if m["action"] != "query" || m["stub"] != true {
		t.Errorf("stub result must pass through unchanged, got %v", m)
	}
	// "action" present but "output" is not an object: not the envelope.
	notObj := `{"action":"query","output":"a string"}`
	m = parseResultPayload(&notObj)
	if m["output"] != "a string" {
		t.Errorf("non-object output must pass through unchanged, got %v", m)
	}
	// Plain object with neither key: passes through unchanged.
	plainObj := `{"series":[1,2]}`
	m = parseResultPayload(&plainObj)
	if _, ok := m["series"]; !ok {
		t.Errorf("plain object must pass through unchanged, got %v", m)
	}
}

// TestDaemonDispatcher_UnwrapsEnvelopeResult is the #1801 regression at the
// dispatcher seam: a fake daemon returns an envelope-shaped result string, and
// DispatchResult.Output must equal the inner output map, not the whole envelope.
func TestDaemonDispatcher_UnwrapsEnvelopeResult(t *testing.T) {
	withDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// result is the executor's dispatch envelope with the real result nested
		// under "output"; the inner result JSON-escaped into the envelope string.
		_, _ = w.Write([]byte(`{"audit_id":"a1","result":"{\"action\":\"query\",\"output\":{\"QueryExecutionId\":\"qeid-1\",\"ResultSet\":{\"rows\":[1]}},\"steps\":{\"run\":{\"QueryExecutionId\":\"qeid-1\"}}}"}`))
	})
	res, err := daemonDispatcher{}.Dispatch(context.Background(), "aileron:athena.query", nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Output["QueryExecutionId"] != "qeid-1" {
		t.Errorf("Output must be the inner output map, got %v", res.Output)
	}
	for _, leaked := range []string{"action", "steps"} {
		if _, ok := res.Output[leaked]; ok {
			t.Errorf("envelope key %q leaked into Output: %v", leaked, res.Output)
		}
	}
}

// TestOperatorActorID_ComposesUserAtHost proves the operator-identity helper
// resolves a non-empty "<user>@<host>" string so launch records correlate to
// the human operator (#1875). Both halves come from the host, so the test
// asserts the shape (exactly one "@", non-empty parts) rather than a fixed
// value.
func TestOperatorActorID_ComposesUserAtHost(t *testing.T) {
	got := operatorActorID()
	user, host, ok := strings.Cut(got, "@")
	if !ok {
		t.Fatalf("operatorActorID() = %q, want a single \"@\"-joined user@host", got)
	}
	if user == "" || host == "" {
		t.Errorf("operatorActorID() = %q, want non-empty user and host halves", got)
	}
	if strings.Contains(host, "@") {
		t.Errorf("operatorActorID() = %q, want exactly one \"@\" separator", got)
	}
}

// TestOperatorActorID_PrefersEnvOverride is the #1881 regression: on the
// composed-environment model the CLI that emits audit records runs INSIDE the
// sealed container, where user.Current()@os.Hostname() resolves to the image's
// fixed non-root user + the ephemeral container id (identical for every
// operator). The host resolves the real operator identity once and carries it
// into the boot via AILERON_OPERATOR_ID; the inner CLI must stamp that value
// VERBATIM. A host-run launch leaves the env unset and keeps the user@host
// floor. Fails before the env-preference seam (the override would be ignored
// and the floor stamped instead).
func TestOperatorActorID_PrefersEnvOverride(t *testing.T) {
	t.Run("env set stamps the host-resolved id verbatim", func(t *testing.T) {
		t.Setenv("AILERON_OPERATOR_ID", "alice@laptop-42")
		if got := operatorActorID(); got != "alice@laptop-42" {
			t.Errorf("operatorActorID() = %q, want %q (host-resolved override verbatim)", got, "alice@laptop-42")
		}
	})

	t.Run("blank env falls back to user@host floor", func(t *testing.T) {
		// A whitespace-only value must not shadow the floor: it carries no
		// identity, so trimming to empty keeps the user@host resolution.
		t.Setenv("AILERON_OPERATOR_ID", "   ")
		got := operatorActorID()
		user, host, ok := strings.Cut(got, "@")
		if !ok || user == "" || host == "" {
			t.Fatalf("operatorActorID() = %q, want user@host floor when env is blank", got)
		}
		if got == "   " {
			t.Errorf("operatorActorID() = %q, blank env must not shadow the floor", got)
		}
	})
}

// TestNewLaunchAuditSink_StampsOperatorHumanActor is the #1875 regression: the
// production sink seam must stamp the operator identity as a {type: human,
// id: "<user>@<host>"} Actor on every record kind it emits — the launch
// summary, per-action, reach, and output records — rather than the old
// {type: service, id: "flightplan-launch"} runtime component. It overrides the
// operatorActorID seam so the assertion is deterministic and independent of the
// host user/hostname.
func TestNewLaunchAuditSink_StampsOperatorHumanActor(t *testing.T) {
	origActor := operatorActorID
	operatorActorID = func() string { return "bob@ci-runner" }
	t.Cleanup(func() { operatorActorID = origActor })

	var gotBody auditIngestRequest
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"audit_id":"audit-op"}`))
	})

	sink := newLaunchAuditSink(io.Discard)
	for _, tc := range []struct {
		name string
		rec  runtime.AuditRecord
	}{
		{"launch-summary", runtime.AuditRecord{Kind: runtime.RecordKindLaunch, Fields: map[string]any{"artifacts": 1}}},
		{"per-action", runtime.AuditRecord{Kind: runtime.RecordKindAction, ActionRef: "aileron:x.y"}},
		{"reach", runtime.AuditRecord{Kind: runtime.RecordKindReach, Fields: map[string]any{"aileron.step.id": "s"}}},
		{"output", runtime.AuditRecord{Kind: runtime.RecordKindOutput, Fields: map[string]any{"aileron.output.name": "o"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotBody = auditIngestRequest{}
			if id := sink.Record(context.Background(), tc.rec); id != "audit-op" {
				t.Fatalf("Record returned %q, want audit-op", id)
			}
			if gotBody.Actor.Type != string(model.ActorTypeHuman) {
				t.Errorf("actor.type = %q, want human", gotBody.Actor.Type)
			}
			if gotBody.Actor.ID != "bob@ci-runner" {
				t.Errorf("actor.id = %q, want bob@ci-runner", gotBody.Actor.ID)
			}
		})
	}
}

// TestDaemonAuditSink_PostsPerActionRecord proves a record with an ActionRef
// posts a flightplan.launch.action event to /v1/audit carrying the human
// operator actor and the actionRef/fields/sink payload, and threads the 201's
// minted audit_id back as the Record return value.
func TestDaemonAuditSink_PostsPerActionRecord(t *testing.T) {
	var gotPath string
	var gotBody auditIngestRequest
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode ingest body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"audit_id":"audit-per-action"}`))
	})

	id := daemonAuditSink{stderr: io.Discard, actorID: "alice@dev-box"}.Record(context.Background(), runtime.AuditRecord{
		ActionRef: "aileron:metrics.query_series",
		Fields:    map[string]any{"rows": 3},
		Sink:      "customer-store",
	})
	if id != "audit-per-action" {
		t.Errorf("Record returned %q, want the minted id from the 201", id)
	}
	if gotPath != "/v1/audit" {
		t.Errorf("posted to %q, want /v1/audit", gotPath)
	}
	if gotBody.EventType != string(model.EventTypeFlightPlanLaunchAction) {
		t.Errorf("event_type = %q, want %q", gotBody.EventType, model.EventTypeFlightPlanLaunchAction)
	}
	if gotBody.Actor.Type != string(model.ActorTypeHuman) || gotBody.Actor.ID != "alice@dev-box" {
		t.Errorf("actor = %+v, want {human, alice@dev-box}", gotBody.Actor)
	}
	if gotBody.Payload["actionRef"] != "aileron:metrics.query_series" {
		t.Errorf("payload.actionRef = %v", gotBody.Payload["actionRef"])
	}
	if gotBody.Payload["sink"] != "customer-store" {
		t.Errorf("payload.sink = %v", gotBody.Payload["sink"])
	}
	if _, ok := gotBody.Payload["fields"]; !ok {
		t.Errorf("payload missing fields: %+v", gotBody.Payload)
	}
}

// TestDaemonAuditSink_PostsLaunchSummary proves a record with no ActionRef
// posts the per-launch summary event type.
func TestDaemonAuditSink_PostsLaunchSummary(t *testing.T) {
	var gotBody auditIngestRequest
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"audit_id":"audit-summary"}`))
	})

	id := daemonAuditSink{stderr: io.Discard}.Record(context.Background(), runtime.AuditRecord{
		Kind:   runtime.RecordKindLaunch,
		Fields: map[string]any{"artifacts": 1},
	})
	if id != "audit-summary" {
		t.Errorf("Record returned %q, want audit-summary", id)
	}
	if gotBody.EventType != string(model.EventTypeFlightPlanLaunch) {
		t.Errorf("event_type = %q, want %q", gotBody.EventType, model.EventTypeFlightPlanLaunch)
	}
	if _, ok := gotBody.Payload["actionRef"]; ok {
		t.Errorf("summary payload must omit actionRef: %+v", gotBody.Payload)
	}
}

// TestDaemonAuditSink_PostsOutputMaterializedFlatPayload proves a
// RecordKindOutput record maps to the output.materialized event type and
// surfaces its flat aileron.* map as the top-level payload (no "fields"
// nesting), matching the vault.user.credential.* convention (#1752).
func TestDaemonAuditSink_PostsOutputMaterializedFlatPayload(t *testing.T) {
	var gotBody auditIngestRequest
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"audit_id":"audit-output"}`))
	})

	id := daemonAuditSink{stderr: io.Discard}.Record(context.Background(), runtime.AuditRecord{
		Kind: runtime.RecordKindOutput,
		Fields: map[string]any{
			"aileron.output.name":         "digest.csv",
			"aileron.output.content_hash": "sha256:abc",
			"aileron.step.kind":           "transform",
		},
	})
	if id != "audit-output" {
		t.Errorf("Record returned %q, want audit-output", id)
	}
	if gotBody.EventType != string(model.EventTypeOutputMaterialized) {
		t.Errorf("event_type = %q, want %q", gotBody.EventType, model.EventTypeOutputMaterialized)
	}
	// The flat aileron.* keys are top-level payload attributes, not nested under
	// "fields".
	if gotBody.Payload["aileron.output.content_hash"] != "sha256:abc" {
		t.Errorf("payload.aileron.output.content_hash = %v, want it at top level", gotBody.Payload["aileron.output.content_hash"])
	}
	if _, nested := gotBody.Payload["fields"]; nested {
		t.Errorf("output payload must be flat, not nested under fields: %+v", gotBody.Payload)
	}
	if gotBody.Payload["aileron.step.kind"] != "transform" {
		t.Errorf("payload.aileron.step.kind = %v", gotBody.Payload["aileron.step.kind"])
	}
}

// TestDaemonAuditSink_PostsReachFlatPayload proves a RecordKindReach record
// (#1784) maps to the flightplan.launch.reach event type and surfaces its flat
// aileron.* map as the top-level payload (no "fields" nesting), including the
// literal not-enforced marker `aileron.reach.enforced: false`.
func TestDaemonAuditSink_PostsReachFlatPayload(t *testing.T) {
	var gotBody auditIngestRequest
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"audit_id":"audit-reach"}`))
	})

	id := daemonAuditSink{stderr: io.Discard}.Record(context.Background(), runtime.AuditRecord{
		Kind: runtime.RecordKindReach,
		Fields: map[string]any{
			"aileron.step.id":        "extract",
			"aileron.reach.effect":   "external-send",
			"aileron.reach.hosts":    []string{"api.example.com"},
			"aileron.reach.enforced": false,
		},
	})
	if id != "audit-reach" {
		t.Errorf("Record returned %q, want audit-reach", id)
	}
	if gotBody.EventType != string(model.EventTypeFlightPlanLaunchReach) {
		t.Errorf("event_type = %q, want %q", gotBody.EventType, model.EventTypeFlightPlanLaunchReach)
	}
	// The flat aileron.* keys are top-level payload attributes, not nested under
	// "fields".
	if _, nested := gotBody.Payload["fields"]; nested {
		t.Errorf("reach payload must be flat, not nested under fields: %+v", gotBody.Payload)
	}
	if gotBody.Payload["aileron.step.id"] != "extract" {
		t.Errorf("payload.aileron.step.id = %v, want extract", gotBody.Payload["aileron.step.id"])
	}
	if gotBody.Payload["aileron.reach.effect"] != "external-send" {
		t.Errorf("payload.aileron.reach.effect = %v, want external-send", gotBody.Payload["aileron.reach.effect"])
	}
	// The not-enforced marker must survive round-trip as a literal false.
	enforced, ok := gotBody.Payload["aileron.reach.enforced"].(bool)
	if !ok || enforced {
		t.Errorf("payload.aileron.reach.enforced = %v, want literal false", gotBody.Payload["aileron.reach.enforced"])
	}
}

// TestDaemonAuditSink_Non201LogsAndReturnsEmpty proves a non-201 daemon reply
// is best-effort: the sink logs to stderr and returns "" rather than failing
// the launch.
func TestDaemonAuditSink_Non201LogsAndReturnsEmpty(t *testing.T) {
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"audit_unavailable"}}`))
	})
	var stderr bytes.Buffer
	id := daemonAuditSink{stderr: &stderr}.Record(context.Background(), runtime.AuditRecord{ActionRef: "aileron:x.y"})
	if id != "" {
		t.Errorf("Record returned %q, want empty on non-201", id)
	}
	if !strings.Contains(stderr.String(), "launch audit not recorded") {
		t.Errorf("stderr missing best-effort warning: %q", stderr.String())
	}
}

// TestRunSkillLaunch_InProcessRecordsAuditToDaemon is the acceptance path: an
// in-process launch posts its audit records to /v1/audit through the real
// daemon-backed sink, and the returned ids surface as the `Audit records: N`
// count. This proves launch provenance reaches the daemon audit trail.
func TestRunSkillLaunch_InProcessRecordsAuditToDaemon(t *testing.T) {
	storeDir := withTempStore(t)
	freezeNoImageForLaunch(t, storeDir)

	var mu sync.Mutex
	var auditPosts int
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/run"):
			w.WriteHeader(http.StatusOK)
			// The worked example binds file-target outputs from the result.
			if strings.Contains(r.URL.Path, "query_series") {
				_, _ = w.Write([]byte(`{"result":"{\"path\":\"digest.csv\",\"mimeType\":\"text/csv\",\"encoding\":\"utf-8\",\"content\":\"name\\ncpu\\n\"}"}`))
			} else {
				_, _ = w.Write([]byte(`{"result":"{\"path\":\"filed_issue.json\",\"mimeType\":\"application/json\",\"encoding\":\"utf-8\",\"content\":\"{}\"}"}`))
			}
		case r.URL.Path == "/v1/audit" && r.Method == http.MethodPost:
			mu.Lock()
			auditPosts++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"audit_id":"audit-landed"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// Dispatch goes through the real daemon-backed dispatcher (wired to the fake
	// daemon by withDaemon); the audit sink is the real daemon-backed sink.
	stubLaunchSeams(t, daemonDispatcher{}, true)
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	mu.Lock()
	posts := auditPosts
	mu.Unlock()
	if posts == 0 {
		t.Fatal("launch recorded no audit events to the daemon")
	}
	if !strings.Contains(stdout.String(), "Audit records:") {
		t.Errorf("stdout missing audit count: %q", stdout.String())
	}
}

// TestRunSkillLaunch_InProcessEmitsOutputMaterializedPerArtifact is the #1752
// acceptance + regression path. The worked example materializes two outputs:
// digest.csv from a `transform` step and filed_issue.json from an action-call
// step. The launch must POST exactly one output.materialized event per
// materialized artifact (two total), each carrying the correct aileron.step.kind,
// and each output's aileron.output.content_hash must equal the digest the launch
// printed to stdout (the dashboard hash equals the stdout digest).
func TestRunSkillLaunch_InProcessEmitsOutputMaterializedPerArtifact(t *testing.T) {
	storeDir := withTempStore(t)
	freezeNoImageForLaunch(t, storeDir)

	var mu sync.Mutex
	var outputPayloads []map[string]any
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/run"):
			w.WriteHeader(http.StatusOK)
			if strings.Contains(r.URL.Path, "query_series") {
				_, _ = w.Write([]byte(`{"result":"{\"path\":\"digest.csv\",\"mimeType\":\"text/csv\",\"encoding\":\"utf-8\",\"content\":\"name\\ncpu\\n\"}"}`))
			} else {
				_, _ = w.Write([]byte(`{"result":"{\"path\":\"filed_issue.json\",\"mimeType\":\"application/json\",\"encoding\":\"utf-8\",\"content\":\"{}\"}"}`))
			}
		case r.URL.Path == "/v1/audit" && r.Method == http.MethodPost:
			var body auditIngestRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.EventType == string(model.EventTypeOutputMaterialized) {
				mu.Lock()
				outputPayloads = append(outputPayloads, body.Payload)
				mu.Unlock()
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"audit_id":"audit-landed"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	stubLaunchSeams(t, daemonDispatcher{}, true)
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}

	mu.Lock()
	defer mu.Unlock()
	// Exactly one output.materialized per materialized artifact (two total).
	if len(outputPayloads) != 2 {
		t.Fatalf("output.materialized events = %d, want exactly 2 (one per materialized artifact)", len(outputPayloads))
	}

	kinds := map[string]bool{}
	for _, p := range outputPayloads {
		kind, _ := p["aileron.step.kind"].(string)
		kinds[kind] = true
		hash, _ := p["aileron.output.content_hash"].(string)
		if hash == "" {
			t.Errorf("output event missing content_hash: %+v", p)
			continue
		}
		// The audited content hash must equal the digest printed to stdout.
		if !strings.Contains(stdout.String(), hash) {
			t.Errorf("content_hash %q not present in launch stdout:\n%s", hash, stdout.String())
		}
	}
	// The two materialized outputs come from a transform step and an action-call
	// step; both kinds must be represented.
	if !kinds["transform"] || !kinds["action-call"] {
		t.Errorf("output step kinds = %v, want both transform and action-call", kinds)
	}
}

// TestRunSkillLaunch_OutputMaterializedCarriesActorProvenanceEndToEnd is the
// #1753 full-chain acceptance through the launch orchestration with a fake
// daemon: the daemon returns actor provenance on the query action-call run
// response, and the transform-materialized output.materialized event walks that
// actor back through its upstream binding — surfacing the connector build and
// identity, the step.inputs content_hash, and the consent decision on the same
// record that already carried the output digest and plan identity.
func TestRunSkillLaunch_OutputMaterializedCarriesActorProvenanceEndToEnd(t *testing.T) {
	storeDir := withTempStore(t)
	freezeNoImageForLaunch(t, storeDir)

	var mu sync.Mutex
	byOutputName := map[string]map[string]any{}
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/run"):
			w.WriteHeader(http.StatusOK)
			if strings.Contains(r.URL.Path, "query_series") {
				// The read action-call carries the actor provenance the daemon
				// resolved: a single-connector, single-identity Athena-style read.
				_, _ = w.Write([]byte(`{
					"result":"{\"path\":\"digest.csv\",\"mimeType\":\"text/csv\",\"encoding\":\"utf-8\",\"content\":\"name\\ncpu\\n\"}",
					"connector_version":"2.3.1",
					"connector_hash":"sha256:conn",
					"identity_label":"work",
					"credential_binding":"aws_sigv4/athena/work",
					"consent_decision":"unattended"
				}`))
			} else {
				_, _ = w.Write([]byte(`{"result":"{\"path\":\"filed_issue.json\",\"mimeType\":\"application/json\",\"encoding\":\"utf-8\",\"content\":\"{}\"}"}`))
			}
		case r.URL.Path == "/v1/audit" && r.Method == http.MethodPost:
			var body auditIngestRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.EventType == string(model.EventTypeOutputMaterialized) {
				name, _ := body.Payload["aileron.output.name"].(string)
				mu.Lock()
				byOutputName[name] = body.Payload
				mu.Unlock()
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"audit_id":"audit-landed"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	stubLaunchSeams(t, daemonDispatcher{}, true)
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", t.TempDir(), "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}

	mu.Lock()
	defer mu.Unlock()
	// The transform output digest.csv binds the query action-call's output, so
	// its record walks the actor back to that read's identity + connector build.
	csv, ok := byOutputName["digest.csv"]
	if !ok {
		t.Fatalf("no output.materialized for digest.csv; saw %v", byOutputName)
	}
	if csv["aileron.actor.identity_label"] != "work" {
		t.Errorf("actor.identity_label = %v, want work (walked back from upstream query)", csv["aileron.actor.identity_label"])
	}
	if csv["aileron.actor.credential_binding"] != "aws_sigv4/athena/work" {
		t.Errorf("actor.credential_binding = %v", csv["aileron.actor.credential_binding"])
	}
	if csv["aileron.actor.connector_version"] != "2.3.1" || csv["aileron.actor.connector_hash"] != "sha256:conn" {
		t.Errorf("actor connector build = %v/%v", csv["aileron.actor.connector_version"], csv["aileron.actor.connector_hash"])
	}
	if csv["aileron.consent.decision"] != "unattended" {
		t.Errorf("consent.decision = %v, want unattended", csv["aileron.consent.decision"])
	}
	// The step.inputs walk-back is present with a content_hash for the bound
	// upstream value (JSON round-trips the []map to []any).
	inputs, ok := csv["aileron.step.inputs"].([]any)
	if !ok || len(inputs) == 0 {
		t.Fatalf("step.inputs = %v, want at least one entry", csv["aileron.step.inputs"])
	}
	entry, _ := inputs[0].(map[string]any)
	if entry["source"] != "steps.query_metrics.series" {
		t.Errorf("step.inputs[0].source = %v, want steps.query_metrics.series", entry["source"])
	}
	if ch, _ := entry["content_hash"].(string); !strings.HasPrefix(ch, "sha256:") {
		t.Errorf("step.inputs[0].content_hash = %v, want a sha256: digest", entry["content_hash"])
	}
	// The record still carries the output digest and plan identity: the full
	// chain lives on one event.
	if ch, _ := csv["aileron.output.content_hash"].(string); !strings.Contains(stdout.String(), ch) {
		t.Errorf("output content_hash %q not present in launch stdout", csv["aileron.output.content_hash"])
	}
	if csv["aileron.plan.signature_status"] != "verified" {
		t.Errorf("plan.signature_status = %v, want verified", csv["aileron.plan.signature_status"])
	}
}

// TestRunSkillLaunch_DaemonEnvelopeUnwrapsThroughMaterialize is the #1801
// full-chain regression: the fake daemon returns the ACTUAL executor dispatch
// envelope {action, output, steps} as the run result string, with an
// Athena-shaped {QueryExecutionId, ResultSet} inner output. The launch must
// unwrap that envelope at the dispatcher seam so:
//
//   - the materialized digest.csv artifact bytes are the inner result JSON only
//     (no action/output/steps keys leaking from the envelope), and
//   - the output.materialized record's step.inputs[] entry — walking back to the
//     materializing step's bound upstream value — carries query_execution_id,
//     which the #1771 lift can only find when QueryExecutionId sits at the top
//     level of the bound value (it would nest under .output without the unwrap).
//
// Before the fix parseResultPayload bound the whole envelope, so both assertions
// failed (the artifact carried the envelope keys and the qid never lifted).
func TestRunSkillLaunch_DaemonEnvelopeUnwrapsThroughMaterialize(t *testing.T) {
	storeDir := withTempStore(t)
	freezeNoImageForLaunch(t, storeDir)

	outDir := t.TempDir()
	var mu sync.Mutex
	byOutputName := map[string]map[string]any{}
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/run"):
			w.WriteHeader(http.StatusOK)
			if strings.Contains(r.URL.Path, "query_series") {
				// The daemon executor wraps the last step's output in the dispatch
				// envelope {action, output, steps}; output is the Athena-shaped
				// result. This is the real over-the-wire shape (#1801), not the
				// pre-fix flat assumption the other tests used.
				_, _ = w.Write([]byte(`{"result":"{\"action\":\"query_series\",\"output\":{\"QueryExecutionId\":\"qeid-1\",\"ResultSet\":{\"rows\":[[\"cpu\",\"42\"]]}},\"steps\":{\"query_series\":{\"QueryExecutionId\":\"qeid-1\"}}}"}`))
			} else {
				// The file_issue action-call also returns the envelope; its inner
				// output is a plain data result the run materializes directly.
				_, _ = w.Write([]byte(`{"result":"{\"action\":\"create_issue\",\"output\":{\"issue\":\"filed\"},\"steps\":{\"create_issue\":{\"issue\":\"filed\"}}}"}`))
			}
		case r.URL.Path == "/v1/audit" && r.Method == http.MethodPost:
			var body auditIngestRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.EventType == string(model.EventTypeOutputMaterialized) {
				name, _ := body.Payload["aileron.output.name"].(string)
				mu.Lock()
				byOutputName[name] = body.Payload
				mu.Unlock()
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"audit_id":"audit-landed"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	stubLaunchSeams(t, daemonDispatcher{}, true)
	stubLaunchImageRunner(t, &fakeLaunchImageRunner{})
	origRun := launchSeamForTest
	launchSeamForTest = fakeCLISeam{}
	t.Cleanup(func() { launchSeamForTest = origRun })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", outDir, "weekly-metrics-digest"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}

	// The digest.csv artifact binds steps.query_metrics.series through the
	// identity transform, so its bytes are the inner Athena output re-serialized
	// as canonical JSON. The envelope's action/output/steps keys MUST NOT appear.
	digest, err := os.ReadFile(filepath.Join(outDir, "digest.csv"))
	if err != nil {
		t.Fatalf("read materialized digest.csv: %v", err)
	}
	var artifact map[string]any
	if err := json.Unmarshal(digest, &artifact); err != nil {
		t.Fatalf("materialized artifact is not the declared result JSON: %v (%s)", err, digest)
	}
	if artifact["QueryExecutionId"] != "qeid-1" {
		t.Errorf("materialized artifact = %s, want the inner Athena result", digest)
	}
	if _, ok := artifact["ResultSet"]; !ok {
		t.Errorf("materialized artifact missing ResultSet: %s", digest)
	}
	for _, leaked := range []string{"action", "output", "steps"} {
		if _, ok := artifact[leaked]; ok {
			t.Errorf("envelope key %q leaked into materialized artifact: %s", leaked, digest)
		}
	}

	// The output.materialized record walks step.inputs back to the bound upstream
	// value (steps.query_metrics.series = the Athena output), so the qid lifts.
	mu.Lock()
	defer mu.Unlock()
	csv, ok := byOutputName["digest.csv"]
	if !ok {
		t.Fatalf("no output.materialized for digest.csv; saw %v", byOutputName)
	}
	inputs, ok := csv["aileron.step.inputs"].([]any)
	if !ok || len(inputs) == 0 {
		t.Fatalf("step.inputs = %v, want at least one entry", csv["aileron.step.inputs"])
	}
	entry, _ := inputs[0].(map[string]any)
	if entry["query_execution_id"] != "qeid-1" {
		t.Errorf("step.inputs[0].query_execution_id = %v, want qeid-1 (the #1771 lift only fires when QueryExecutionId is top-level, i.e. the envelope was unwrapped)", entry["query_execution_id"])
	}
}

// TestDaemonAuditSink_BaseURLErrorReturnsEmpty proves an unresolvable daemon
// base URL is best-effort: the sink logs and returns "" without failing.
func TestDaemonAuditSink_BaseURLErrorReturnsEmpty(t *testing.T) {
	origBase := bindingAPIBaseURL
	bindingAPIBaseURL = func() (string, error) { return "", context.DeadlineExceeded }
	t.Cleanup(func() { bindingAPIBaseURL = origBase })

	var stderr bytes.Buffer
	id := daemonAuditSink{stderr: &stderr}.Record(context.Background(), runtime.AuditRecord{ActionRef: "aileron:x.y"})
	if id != "" {
		t.Errorf("Record returned %q, want empty when base URL is unresolvable", id)
	}
	if !strings.Contains(stderr.String(), "resolve daemon URL") {
		t.Errorf("stderr = %q, want a resolve-URL warning", stderr.String())
	}
}

// TestDaemonAuditSink_TransportErrorReturnsEmpty proves a transport failure is
// best-effort: the sink logs and returns "".
func TestDaemonAuditSink_TransportErrorReturnsEmpty(t *testing.T) {
	origBase := bindingAPIBaseURL
	origClient := actionsHTTPClient
	bindingAPIBaseURL = func() (string, error) { return "http://127.0.0.1:1/v1", nil }
	actionsHTTPClient = &http.Client{Transport: errRoundTripper{}}
	t.Cleanup(func() {
		bindingAPIBaseURL = origBase
		actionsHTTPClient = origClient
	})

	var stderr bytes.Buffer
	id := daemonAuditSink{stderr: &stderr}.Record(context.Background(), runtime.AuditRecord{})
	if id != "" {
		t.Errorf("Record returned %q, want empty on transport error", id)
	}
	if !strings.Contains(stderr.String(), "post audit event") {
		t.Errorf("stderr = %q, want a post-failure warning", stderr.String())
	}
}

// TestDaemonAuditSink_BadJSONResponseReturnsEmpty proves a 201 with an
// undecodable body is best-effort: the sink logs and returns "".
func TestDaemonAuditSink_BadJSONResponseReturnsEmpty(t *testing.T) {
	withDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{not json`))
	})
	var stderr bytes.Buffer
	id := daemonAuditSink{stderr: &stderr}.Record(context.Background(), runtime.AuditRecord{})
	if id != "" {
		t.Errorf("Record returned %q, want empty on undecodable 201 body", id)
	}
	if !strings.Contains(stderr.String(), "decode audit response") {
		t.Errorf("stderr = %q, want a decode warning", stderr.String())
	}
}

// TestDaemonAuditSink_NilStderrDoesNotPanic proves logErr tolerates a nil
// writer (the launch always passes one, but the sink must not panic).
func TestDaemonAuditSink_NilStderrDoesNotPanic(t *testing.T) {
	origBase := bindingAPIBaseURL
	bindingAPIBaseURL = func() (string, error) { return "", context.DeadlineExceeded }
	t.Cleanup(func() { bindingAPIBaseURL = origBase })

	if id := (daemonAuditSink{}).Record(context.Background(), runtime.AuditRecord{}); id != "" {
		t.Errorf("Record returned %q, want empty", id)
	}
}

// errRoundTripper always fails, so a request never leaves the process.
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
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
