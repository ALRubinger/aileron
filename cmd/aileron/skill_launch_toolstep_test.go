package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// stubToolStepExec swaps the subprocess exec seam for a recorder. onExec, when
// non-nil, runs in place of the subprocess (e.g. to write the collect file).
func stubToolStepExec(t *testing.T, gotArgv *[]string, gotEnv *[]string, onExec func() error) {
	t.Helper()
	orig := toolStepExecCommand
	toolStepExecCommand = func(_ context.Context, env []string, _, _ io.Writer, argv []string) error {
		*gotArgv = append([]string(nil), argv...)
		*gotEnv = append([]string(nil), env...)
		if onExec != nil {
			return onExec()
		}
		return nil
	}
	t.Cleanup(func() { toolStepExecCommand = orig })
}

// envValue returns the LAST value of key in env ("" when absent), matching
// how exec.Cmd resolves duplicate env entries.
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			v = strings.TrimPrefix(kv, key+"=")
		}
	}
	return v
}

// stepScopeDaemon is an httptest daemon serving the step-scope mint/release
// endpoints. It records mint requests and released scope ids.
type stepScopeDaemon struct {
	t        *testing.T
	mints    []map[string]any
	releases []string
	mintFail bool
	server   *httptest.Server
}

func newStepScopeDaemon(t *testing.T) *stepScopeDaemon {
	t.Helper()
	d := &stepScopeDaemon{t: t}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandbox-proxy/step-scopes":
			if d.mintFail {
				http.Error(w, "mint refused", http.StatusInternalServerError)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			d.mints = append(d.mints, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"scope_id":"scope-1","token":"scope-token-xyz","expires_at":"2100-01-01T00:00:00Z"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sandbox-proxy/step-scopes/"):
			d.releases = append(d.releases, strings.TrimPrefix(r.URL.Path, "/v1/sandbox-proxy/step-scopes/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(d.server.Close)

	origBase := bindingAPIBaseURL
	bindingAPIBaseURL = func() (string, error) { return d.server.URL + "/v1", nil }
	t.Cleanup(func() { bindingAPIBaseURL = origBase })
	return d
}

// inContainerEnv arranges the #1759/#1828 boot environment the runner reads:
// the booted-image sentinel and the session-authed boot proxy URL.
func inContainerEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envSkillImageBooted, "1")
	t.Setenv("HTTPS_PROXY", "http://boot-session:daemon-token@host.docker.internal:9210")
	t.Setenv("https_proxy", "http://boot-session:daemon-token@host.docker.internal:9210")
	t.Setenv("AILERON_TOKEN", "daemon-token")
}

// TestInContainerToolStepRunner_RefusesOnHost proves the sentinel guard: a
// signed argv never execs outside the pinned plan image.
func TestInContainerToolStepRunner_RefusesOnHost(t *testing.T) {
	t.Setenv(envSkillImageBooted, "")
	var argv, env []string
	stubToolStepExec(t, &argv, &env, nil)

	_, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:  "extract",
		Command: []string{"extract-tool"},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to exec on the host") {
		t.Fatalf("err = %v, want the host-exec refusal", err)
	}
	if len(argv) != 0 {
		t.Fatal("the subprocess must never exec outside the pinned image")
	}
}

// TestInContainerToolStepRunner_MountRunCollectRoundTrip proves the file I/O
// contract: the resolved input lands at MountPath/input.json before the exec,
// the argv execs verbatim, and the collect path is read back as the output.
func TestInContainerToolStepRunner_MountRunCollectRoundTrip(t *testing.T) {
	inContainerEnv(t)
	work := t.TempDir()
	mountDir := filepath.Join(work, "in")
	collect := filepath.Join(work, "out", "result.txt")

	var argv, env []string
	stubToolStepExec(t, &argv, &env, func() error {
		// The "tool": prove the input was mounted BEFORE the exec, then write
		// the collect file.
		raw, err := os.ReadFile(filepath.Join(mountDir, toolStepInputFile))
		if err != nil {
			t.Errorf("input.json must exist before the exec: %v", err)
		}
		var in map[string]any
		if err := json.Unmarshal(raw, &in); err != nil || in["payload"] != "hello" {
			t.Errorf("mounted input = %s, want the resolved bindings", raw)
		}
		if err := os.MkdirAll(filepath.Dir(collect), 0o755); err != nil {
			return err
		}
		return os.WriteFile(collect, []byte("COLLECTED-BYTES"), 0o644)
	})

	res, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:      "extract",
		Command:     []string{"extract-tool", "--mode", "csv"},
		MountPath:   mountDir,
		Input:       map[string]any{"payload": "hello"},
		CollectPath: collect,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(argv, " ") != "extract-tool --mode csv" {
		t.Errorf("exec argv = %v, want the step argv verbatim (no shell)", argv)
	}
	if res.Output != "COLLECTED-BYTES" {
		t.Errorf("output = %v, want the collected bytes", res.Output)
	}
	// An unscoped step (no sealed hosts) runs under the plan-boot proxy env
	// unchanged.
	if got := envValue(env, "HTTPS_PROXY"); got != "http://boot-session:daemon-token@host.docker.internal:9210" {
		t.Errorf("unscoped HTTPS_PROXY = %q, want the boot proxy env unchanged", got)
	}
}

// TestInContainerToolStepRunner_ScopedStepMintsAndReleases proves the sealed
// reach path (#1829): the scope is minted BEFORE the exec with the boot
// session + step id + sealed hosts, the subprocess env carries the
// step-scoped proxy URL (session username preserved, scope token as the
// password) with AILERON_TOKEN removed, and the scope is released after.
func TestInContainerToolStepRunner_ScopedStepMintsAndReleases(t *testing.T) {
	inContainerEnv(t)
	t.Setenv("HTTP_PROXY", "http://boot-session:daemon-token@host.docker.internal:9210")
	daemon := newStepScopeDaemon(t)

	var argv, env []string
	stubToolStepExec(t, &argv, &env, func() error {
		// Minted before the exec: by exec time the daemon has one mint and no
		// release yet.
		if len(daemon.mints) != 1 {
			t.Errorf("mints at exec time = %d, want 1 (scope obtained before the subprocess runs)", len(daemon.mints))
		}
		if len(daemon.releases) != 0 {
			t.Errorf("releases at exec time = %d, want 0", len(daemon.releases))
		}
		return nil
	})

	_, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:  "extract",
		Command: []string{"extract-tool"},
		Hosts:   []string{"api.sealed.example.com"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The mint carried the boot session, the step id, and the SEALED hosts.
	if len(daemon.mints) != 1 {
		t.Fatalf("mints = %d, want 1", len(daemon.mints))
	}
	mint := daemon.mints[0]
	if mint["session_id"] != "boot-session" || mint["step_id"] != "extract" {
		t.Errorf("mint = %v, want the boot session + step id", mint)
	}
	hosts, _ := mint["hosts"].([]any)
	if len(hosts) != 1 || hosts[0] != "api.sealed.example.com" {
		t.Errorf("mint hosts = %v, want the sealed reach", mint["hosts"])
	}
	// A step with no declared credential identity sends no credential block:
	// omitempty drops the field, so the wire bytes are exactly today's
	// (#1980 optionality — regression guard).
	if _, ok := mint["credential"]; ok {
		t.Errorf("mint carried a credential block %v, want none for a step with no credential identity", mint["credential"])
	}

	// The subprocess env: HTTPS_PROXY/https_proxy (and HTTP_PROXY, originally
	// set) carry the scoped URL; AILERON_TOKEN is gone.
	wantScoped := "http://boot-session:scope-token-xyz@host.docker.internal:9210"
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY"} {
		if got := envValue(env, key); got != wantScoped {
			t.Errorf("%s = %q, want the step-scoped proxy URL %q", key, got, wantScoped)
		}
	}
	if got := envValue(env, "AILERON_TOKEN"); got != "" {
		t.Error("AILERON_TOKEN must be removed from the scoped subprocess env")
	}
	// The scoped URL must never carry the full daemon token as the password.
	if u, err := url.Parse(envValue(env, "HTTPS_PROXY")); err == nil {
		if pw, _ := u.User.Password(); pw == "daemon-token" {
			t.Error("the scoped proxy URL must not carry the unscoped daemon token")
		}
	}

	// Released after the step.
	if len(daemon.releases) != 1 || daemon.releases[0] != "scope-1" {
		t.Errorf("releases = %v, want [scope-1]", daemon.releases)
	}
}

// TestInContainerToolStepRunner_ScopedStepCarriesCredentialIdentity proves a
// sealed step whose spec declares a credential identity puts it on the mint
// wire (#1980): the mint body's credential block carries the matching kind +
// identityLabel, letting the daemon learn which credential identity the step's
// egress belongs to. The block carries only the non-secret identity, never
// credential material.
func TestInContainerToolStepRunner_ScopedStepCarriesCredentialIdentity(t *testing.T) {
	inContainerEnv(t)
	daemon := newStepScopeDaemon(t)

	var argv, env []string
	stubToolStepExec(t, &argv, &env, nil)

	_, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:         "extract",
		Command:        []string{"extract-tool"},
		Hosts:          []string{"api.sealed.example.com"},
		CredentialKind: "aws_sigv4",
		IdentityLabel:  "prod-reader",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(daemon.mints) != 1 {
		t.Fatalf("mints = %d, want 1", len(daemon.mints))
	}
	cred, ok := daemon.mints[0]["credential"].(map[string]any)
	if !ok {
		t.Fatalf("mint carried no credential block, want one: %v", daemon.mints[0])
	}
	if cred["kind"] != "aws_sigv4" || cred["identityLabel"] != "prod-reader" {
		t.Errorf("credential block = %v, want kind=aws_sigv4 identityLabel=prod-reader", cred)
	}
}

// TestInContainerToolStepRunner_FailsClosedOnMintFailure proves a sealed step
// that cannot obtain its scope never runs: the exec seam is not reached.
func TestInContainerToolStepRunner_FailsClosedOnMintFailure(t *testing.T) {
	inContainerEnv(t)
	daemon := newStepScopeDaemon(t)
	daemon.mintFail = true

	var argv, env []string
	stubToolStepExec(t, &argv, &env, nil)

	_, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:  "extract",
		Command: []string{"extract-tool"},
		Hosts:   []string{"api.sealed.example.com"},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to run unscoped") {
		t.Fatalf("err = %v, want the fail-closed refusal", err)
	}
	if len(argv) != 0 {
		t.Fatal("a sealed step whose scope mint failed must never exec")
	}
}

// TestInContainerToolStepRunner_FailsClosedWithoutBootProxy proves a sealed
// step with no boot proxy environment (no HTTPS_PROXY to scope against)
// fails closed rather than egressing unscoped.
func TestInContainerToolStepRunner_FailsClosedWithoutBootProxy(t *testing.T) {
	t.Setenv(envSkillImageBooted, "1")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")

	var argv, env []string
	stubToolStepExec(t, &argv, &env, nil)

	_, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:  "extract",
		Command: []string{"extract-tool"},
		Hosts:   []string{"api.sealed.example.com"},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to run unscoped") {
		t.Fatalf("err = %v, want the fail-closed refusal", err)
	}
	if len(argv) != 0 {
		t.Fatal("the subprocess must not exec without a scope")
	}
}

// TestInContainerToolStepRunner_CommandFailureSurfaces proves a failing
// subprocess surfaces as a step error rather than being swallowed.
func TestInContainerToolStepRunner_CommandFailureSurfaces(t *testing.T) {
	inContainerEnv(t)
	var argv, env []string
	stubToolStepExec(t, &argv, &env, func() error { return os.ErrPermission })

	_, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:  "extract",
		Command: []string{"extract-tool"},
	})
	if err == nil || !strings.Contains(err.Error(), "command failed") {
		t.Fatalf("err = %v, want the command failure surfaced", err)
	}
}

// TestInContainerToolStepRunner_SymlinkedCollectRefused is the CWE-61
// regression proof carried over from the sibling-dispatch runner: a tool that
// writes the collect path as a symlink to a sensitive file must be refused,
// never followed.
func TestInContainerToolStepRunner_SymlinkedCollectRefused(t *testing.T) {
	inContainerEnv(t)
	work := t.TempDir()
	secret := filepath.Join(work, "secret")
	if err := os.WriteFile(secret, []byte("TOP SECRET BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	collect := filepath.Join(work, "out", "result")

	var argv, env []string
	stubToolStepExec(t, &argv, &env, func() error {
		if err := os.MkdirAll(filepath.Dir(collect), 0o755); err != nil {
			return err
		}
		return os.Symlink(secret, collect)
	})

	res, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:      "extract",
		Command:     []string{"extract-tool"},
		CollectPath: collect,
	})
	if err == nil {
		t.Fatalf("a symlinked collect must be refused, got output %v", res.Output)
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("error should explain the non-regular-file refusal, got: %v", err)
	}
	if res.Output != nil {
		t.Errorf("no smuggled bytes may reach the output, got %v", res.Output)
	}
}

// TestInContainerToolStepRunner_MissingCollectErrors proves a declared
// collect path the tool never wrote surfaces an error rather than silently
// producing an empty output.
func TestInContainerToolStepRunner_MissingCollectErrors(t *testing.T) {
	inContainerEnv(t)
	var argv, env []string
	stubToolStepExec(t, &argv, &env, nil)

	_, err := inContainerToolStepRunner{}.Run(context.Background(), runtime.ToolStepSpec{
		StepID:      "extract",
		Command:     []string{"extract-tool"},
		CollectPath: filepath.Join(t.TempDir(), "never-written"),
	})
	if err == nil || !strings.Contains(err.Error(), "collect output") {
		t.Fatalf("err = %v, want a collect readback error", err)
	}
}

// --- contract regression test 2 (#1829): one boot, zero sibling dispatches ---

// toolStepLaunchSkillMD is an inline schema-valid skill with an action-call
// (orchestrator), a tool step, and a transform that materializes the tool's
// collected output — the three-kind mix the umbrella's in-container model
// must run under EXACTLY ONE container boot.
const toolStepLaunchSkillMD = `---
name: tool-step-e2e
description: Tool step end-to-end fixture.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential: { kind: none }
          hosts: [api.example.com]
          effect: read
          idempotency: { safeToRetry: true }
          audit: { fields: [result] }
  environment:
    tools: [aws-cli@2.x]
  inputs:
    - name: payload
      type: string
      resolution: { rule: literal, default: hello }
  outputs:
    - name: out.txt
      mimeType: text/plain
      encoding: utf-8
      publish: { target: file, path: out.txt }
  steps:
    - id: query
      kind: action-call
      actionRef: aileron:metrics.query_series
      args: { q: inputs.payload }
      outputs: [rows]
    - id: extract
      kind: tool
      command: [extract-tool, --mode, csv]
      bindings: { rows: steps.query.rows }
      mount: { path: /work/in }
      collect: { path: /work/out/result.txt }
      trustContract:
        credential: { kind: none }
        hosts: [api.sealed.example.com]
        effect: read
        idempotency: { safeToRetry: true }
        audit: { fields: [result] }
      outputs: [result]
    - id: render
      kind: transform
      bindings: { upstream: steps.extract.result }
      outputs: [file]
      materializesOutput: out.txt
---
# Tool step e2e fixture
`

// fakeToolStepRunner records the tool-step specs the runtime hands the CLI
// seam and returns the collected output in the PRODUCTION shape: the raw
// string bytes of the collect file (inContainerToolStepRunner returns
// string(out), never a decoded structure). Here the "tool" wrote a file-map
// JSON document, so the downstream transform + materialize path is exercised
// against exactly what a real collect readback yields.
type fakeToolStepRunner struct {
	specs []runtime.ToolStepSpec
}

func (f *fakeToolStepRunner) Run(_ context.Context, spec runtime.ToolStepSpec) (runtime.ToolStepResult, error) {
	f.specs = append(f.specs, spec)
	return runtime.ToolStepResult{
		Output: `{"path":"out.txt","mimeType":"text/plain","encoding":"utf-8","content":"collected\n"}`,
	}, nil
}

// TestRunSkillLaunch_ToolStepsRunInSingleBoot is contract regression test 2
// for #1829: a plan mixing an action-call, a transform, and a tool step
// launches with EXACTLY ONE container boot recorded and ZERO sibling tool
// boots — the in-container re-entry executes the tool step as a subprocess
// through the ToolStepRunner seam with the SEALED reach threaded, and the
// launch completes end-to-end (artifact materialized).
//
// On origin/main this fails: the runtime has no `kind: tool` step kind (the
// tool-step plan is refused at decode) and tool execution rode a sibling
// container dispatch seam that this change deletes.
func TestRunSkillLaunch_ToolStepsRunInSingleBoot(t *testing.T) {
	storeDir := withTempStore(t)
	// Install + freeze the inline fixture.
	dir := filepath.Join(storeDir, "tool-step-e2e")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(toolStepLaunchSkillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	stubFreezeResolvers(t, fakeFreezeDigest)
	key := writeSigningKey(t)
	var out, errOut bytes.Buffer
	if code := runSkillFreeze([]string{"--signing-key", key, "--version", "1.0.0", "tool-step-e2e"}, &out, &errOut); code != 0 {
		t.Fatalf("freeze failed: %s", errOut.String())
	}

	// Wire the seams: a fake dispatcher for the action-call, a fake tool-step
	// runner for the tool step, and a boot recorder that simulates the
	// in-container re-entry by re-invoking runSkillLaunch with the sentinel
	// set (the exact production model: one boot, then in-process execution
	// inside it).
	disp := &fakeLaunchDispatcher{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"rows": []any{"r1", "r2"}},
	}}
	stubLaunchSeams(t, disp)

	toolRunner := &fakeToolStepRunner{}
	origTool := newLaunchToolStepRunner
	newLaunchToolStepRunner = func() runtime.ToolStepRunner { return toolRunner }
	t.Cleanup(func() { newLaunchToolStepRunner = origTool })

	outDir := t.TempDir()
	bootCount := 0
	var innerCode int
	var innerErr bytes.Buffer
	origBoot := newLaunchImageRunner
	newLaunchImageRunner = func() runtime.ImageRunner {
		return imageRunnerFunc(func(ctx context.Context, spec runtime.ImageRunSpec) (runtime.ImageRunResult, error) {
			bootCount++
			// Simulate the in-container re-entry: same binary, sentinel set,
			// against the same (host-mounted) store and out-dir.
			t.Setenv(envSkillImageBooted, "1")
			defer t.Setenv(envSkillImageBooted, "")
			var innerOut bytes.Buffer
			innerCode = runSkillLaunch(
				[]string{"--store-dir", storeDir, "--out-dir", outDir, "--version", spec.Version, spec.Name},
				&innerOut, &innerErr)
			return runtime.ImageRunResult{ContentHash: "sha256:booted"}, nil
		})
	}
	t.Cleanup(func() { newLaunchImageRunner = origBoot })

	var stdout, stderr bytes.Buffer
	code := runSkillLaunch([]string{"--out-dir", outDir, "tool-step-e2e"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("launch exit = %d, stderr=%s", code, stderr.String())
	}
	if innerCode != 0 {
		t.Fatalf("in-container re-entry exit = %d, stderr=%s", innerCode, innerErr.String())
	}

	// EXACTLY ONE container boot, zero sibling tool boots: the whole plan —
	// action-call, tool step, transform — ran inside the single booted
	// environment.
	if bootCount != 1 {
		t.Fatalf("container boots = %d, want exactly 1", bootCount)
	}
	if len(toolRunner.specs) != 1 {
		t.Fatalf("tool step executions = %d, want exactly 1 (in-container subprocess)", len(toolRunner.specs))
	}
	spec := toolRunner.specs[0]
	if strings.Join(spec.Command, " ") != "extract-tool --mode csv" {
		t.Errorf("tool argv = %v, want the signed argv verbatim", spec.Command)
	}
	// The sealed reach from the verified lock (never the frontmatter) is what
	// the runner receives.
	if strings.Join(spec.Hosts, ",") != "api.sealed.example.com" {
		t.Errorf("tool sealed hosts = %v, want the lock's stepTrust reach", spec.Hosts)
	}
	// The action-call dispatched through the daemon boundary exactly once.
	if len(disp.refs) != 1 || disp.refs[0] != "aileron:metrics.query_series" {
		t.Errorf("dispatches = %v, want exactly the orchestrator action-call", disp.refs)
	}
	// End-to-end completion: the transform materialized the collected output.
	got, err := os.ReadFile(filepath.Join(outDir, "out.txt"))
	if err != nil {
		t.Fatalf("materialized artifact missing: %v", err)
	}
	if string(got) != "collected\n" {
		t.Errorf("artifact = %q, want the tool's collected bytes", got)
	}
}

// imageRunnerFunc adapts a func to runtime.ImageRunner for boot recorders.
type imageRunnerFunc func(ctx context.Context, spec runtime.ImageRunSpec) (runtime.ImageRunResult, error)

func (f imageRunnerFunc) Run(ctx context.Context, spec runtime.ImageRunSpec) (runtime.ImageRunResult, error) {
	return f(ctx, spec)
}
