package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"
)

// stubContainerBoot swaps the single real-Docker call site for a recorder so the
// container image runner's spec-construction is testable with no live runtime.
// It also stubs the CLI-version-skew preflight to "" so every Run test stays
// Docker-free and warning-silent by default; skew tests override it via
// stubBakedCLIVersion.
func stubContainerBoot(t *testing.T, capture *sandboxcontainer.RunOptions, err error) {
	t.Helper()
	stubBakedCLIVersion(t, "")
	orig := containerRunFlightPlan
	containerRunFlightPlan = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		*capture = opts
		return sandboxcontainer.RunResult{}, err
	}
	t.Cleanup(func() { containerRunFlightPlan = orig })
}

// stubBakedCLIVersion swaps the CLI-version inspect seam for a fake returning
// value, recording the image it was asked to inspect. This keeps the preflight
// off `docker image inspect` in unit tests and lets skew tests assert the
// three cases (match, mismatch, missing label) through Run. It returns a pointer
// to the recorded image so tests can assert the pin was passed, never re-resolved.
func stubBakedCLIVersion(t *testing.T, value string) *string {
	t.Helper()
	var inspected string
	orig := containerBakedCLIVersion
	containerBakedCLIVersion = func(_ context.Context, _ string, image string) string {
		inspected = image
		return value
	}
	t.Cleanup(func() { containerBakedCLIVersion = orig })
	return &inspected
}

func TestContainerImageRunner_BootsExactImageWithMounts(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	outDir := t.TempDir()
	spec := runtime.ImageRunSpec{
		Image:   "registry.example.com/runner:1.4@sha256:abc",
		Name:    "weekly-metrics-digest",
		Version: "1.0.0",
		Inputs:  runtime.LaunchArgs{"window_days": "30"},
		OutDir:  outDir,
	}
	res, err := containerImageRunner{}.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The booted image must be the exact pin, never re-resolved.
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
	// The store is mounted read-only; the out-dir writable.
	var storeRO, outRW bool
	for _, v := range got.Volumes {
		if v.Source == storeDir && v.ReadOnly {
			storeRO = true
		}
		if v.Source == outDir && !v.ReadOnly {
			outRW = true
		}
	}
	if !storeRO {
		t.Errorf("frozen store must be mounted read-only, got %+v", got.Volumes)
	}
	if !outRW {
		t.Errorf("out-dir must be mounted writable, got %+v", got.Volumes)
	}
	// The in-container command re-enters aileron against the mounted unit.
	joined := strings.Join(got.Command, " ")
	if !strings.Contains(joined, "skill launch weekly-metrics-digest") {
		t.Errorf("command = %v, want an in-container skill launch of the unit", got.Command)
	}
	if !strings.Contains(joined, "--version 1.0.0") {
		t.Errorf("command must pin the version: %v", got.Command)
	}
	// The inner binary must be pointed at the bind-mounted store, not the empty
	// default store inside the image.
	if !strings.Contains(joined, "--store-dir /aileron/skills") {
		t.Errorf("command must point the inner binary at the mounted store: %v", got.Command)
	}
	// The container name carries a run-unique suffix so concurrent launches of
	// the same unit never collide.
	if got.Name == flightPlanContainerName(spec) {
		t.Error("container name must include a run-unique suffix, not a fully deterministic value")
	}
	if !strings.HasPrefix(got.Name, "aileron-flightplan-weekly-metrics-digest-1.0.0-") {
		t.Errorf("container name = %q, want the stable base prefix plus a suffix", got.Name)
	}
	// The result echoes the resolved inputs in v1.
	if res.ResolvedInputs["window_days"] != "30" {
		t.Errorf("result inputs = %v, want the launch inputs echoed", res.ResolvedInputs)
	}
}

// fakeImageDaemonEnv is a canned imageDaemonEnv for the runner tests: it returns
// the recorded env map and ok flag so the runner's RunOptions.Env wiring is
// asserted with no live daemon and no Docker.
type fakeImageDaemonEnv struct {
	env        map[string]string
	ok         bool
	gotRuntime string
	callCount  int
}

func (f *fakeImageDaemonEnv) Env(runtimeName string) (map[string]string, bool) {
	f.callCount++
	f.gotRuntime = runtimeName
	return f.env, f.ok
}

// TestContainerImageRunner_InjectsDaemonEnv proves that when a daemon-env
// resolver is wired and resolves, the booted container carries the host daemon
// coordinates so the re-entered in-container launch reaches the host daemon
// action + audit boundary (#1759).
func TestContainerImageRunner_InjectsDaemonEnv(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	fake := &fakeImageDaemonEnv{
		env: map[string]string{
			"AILERON_API_URL": "http://host.docker.internal:48123/v1",
			"AILERON_TOKEN":   "daemon-token",
		},
		ok: true,
	}
	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
	if _, err := (containerImageRunner{daemonEnv: fake}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.callCount != 1 {
		t.Fatalf("resolver called %d times, want 1", fake.callCount)
	}
	// The resolver is passed the resolved runtime name so it can host-rewrite the
	// daemon URL for that runtime.
	if fake.gotRuntime == "" {
		t.Error("resolver must be passed the resolved runtime name")
	}
	if got.Env["AILERON_API_URL"] != "http://host.docker.internal:48123/v1" {
		t.Errorf("AILERON_API_URL = %q, want the host-rewritten /v1 daemon URL", got.Env["AILERON_API_URL"])
	}
	if got.Env["AILERON_TOKEN"] != "daemon-token" {
		t.Errorf("AILERON_TOKEN = %q, want the daemon token", got.Env["AILERON_TOKEN"])
	}
	if got.Env[envSkillImageBooted] != "1" {
		t.Errorf("%s = %q, want the boot sentinel alongside the daemon env", envSkillImageBooted, got.Env[envSkillImageBooted])
	}
}

// TestContainerImageRunner_PassthroughWhenNoDaemonConfig proves that when the
// resolver reports ok=false (no daemon config), the boot carries no injected env
// so a no-daemon launch is unaffected (#1759).
func TestContainerImageRunner_PassthroughWhenNoDaemonConfig(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	fake := &fakeImageDaemonEnv{ok: false}
	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
	if _, err := (containerImageRunner{daemonEnv: fake}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only the boot sentinel: no daemon coordinates on the passthrough boot.
	if len(got.Env) != 1 || got.Env[envSkillImageBooted] != "1" {
		t.Errorf("RunOptions.Env = %v, want only the boot sentinel on the passthrough (ok=false) boot", got.Env)
	}
}

// TestContainerImageRunner_ZeroValueInjectsNoEnv proves the zero-value runner
// (daemonEnv == nil) injects no daemon env, so the CLI unit tests stay
// deterministic irrespective of any live ~/.aileron daemon (#1759). The boot
// sentinel is still present: every whole-plan boot marks the re-entry.
func TestContainerImageRunner_ZeroValueInjectsNoEnv(t *testing.T) {
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)

	spec := runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
	if _, err := (containerImageRunner{}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.Env) != 1 || got.Env[envSkillImageBooted] != "1" {
		t.Errorf("RunOptions.Env = %v, want only the boot sentinel on the zero-value runner", got.Env)
	}
}

// TestNewLaunchImageRunner_WiresDaemonEnvResolver proves the production seam
// wires the daemon-backed env resolver so production boots carry the host daemon
// coordinates (#1759), mirroring newLaunchToolImageRunner's proxy wiring.
func TestNewLaunchImageRunner_WiresDaemonEnvResolver(t *testing.T) {
	runner, ok := newLaunchImageRunner().(containerImageRunner)
	if !ok {
		t.Fatalf("newLaunchImageRunner returned %T, want containerImageRunner", newLaunchImageRunner())
	}
	if _, ok := runner.daemonEnv.(daemonImageEnv); !ok {
		t.Errorf("daemonEnv = %T, want daemonImageEnv (the production resolver)", runner.daemonEnv)
	}
}

// --- CLI-version skew preflight (#1809) ---

// newSkewTestSpec returns a minimal spec with the store dir pointed at a temp
// dir so Run reaches the boot without touching the real store.
func newSkewTestSpec(t *testing.T) runtime.ImageRunSpec {
	t.Helper()
	storeDir := t.TempDir()
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })
	return runtime.ImageRunSpec{
		Image: "registry.example.com/runner:1.4@sha256:abc",
		Name:  "weekly-metrics-digest",
	}
}

// TestContainerImageRunner_SkewWarnsOnMismatch proves a baked CLI version that
// differs from the host version prints one stderr warning naming both versions
// and the image, still boots the exact pin, and returns no error.
func TestContainerImageRunner_SkewWarnsOnMismatch(t *testing.T) {
	spec := newSkewTestSpec(t)
	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	inspected := stubBakedCLIVersion(t, "9.9.9")

	var buf bytes.Buffer
	if _, err := (containerImageRunner{diag: &buf}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "warning:") {
		t.Errorf("expected a stderr warning, got %q", out)
	}
	if !strings.Contains(out, "9.9.9") {
		t.Errorf("warning must name the baked version 9.9.9: %q", out)
	}
	if !strings.Contains(out, version.Version) {
		t.Errorf("warning must name the host version %q: %q", version.Version, out)
	}
	if !strings.Contains(out, spec.Image) {
		t.Errorf("warning must name the image %q: %q", spec.Image, out)
	}
	// The launch proceeds with the exact pin regardless of skew.
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
	// The preflight inspects the pin, never a re-resolved image.
	if *inspected != spec.Image {
		t.Errorf("preflight inspected %q, want the pin %q", *inspected, spec.Image)
	}
}

// TestContainerImageRunner_SkewSilentOnMatch proves a baked CLI version equal to
// the host version emits nothing (stdout stays byte-identical, no OK line) and
// still boots the pin.
func TestContainerImageRunner_SkewSilentOnMatch(t *testing.T) {
	spec := newSkewTestSpec(t)
	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	stubBakedCLIVersion(t, version.Version)

	var buf bytes.Buffer
	if _, err := (containerImageRunner{diag: &buf}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("matching version must be silent, got %q", buf.String())
	}
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
}

// TestContainerImageRunner_SkewSilentOnMissingLabel proves an unlabeled or
// uninspectable image (baked == "") stays silent because a custom image's
// contract is the operator's under ADR-0027, and still boots.
func TestContainerImageRunner_SkewSilentOnMissingLabel(t *testing.T) {
	spec := newSkewTestSpec(t)
	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, nil)
	stubBakedCLIVersion(t, "")

	var buf bytes.Buffer
	if _, err := (containerImageRunner{diag: &buf}).Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("missing label must be silent, got %q", buf.String())
	}
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
}

// TestContainerImageRunner_DiagDefaultsToStderr proves the zero-value diag field
// resolves to os.Stderr in production wiring, so a nil diag never routes the
// warning to a discarded writer.
func TestContainerImageRunner_DiagDefaultsToStderr(t *testing.T) {
	if got := (containerImageRunner{}).diagWriter(); got != os.Stderr {
		t.Errorf("zero-value diag writer = %v, want os.Stderr", got)
	}
	var buf bytes.Buffer
	if got := (containerImageRunner{diag: &buf}).diagWriter(); got != &buf {
		t.Errorf("injected diag writer = %v, want the injected buffer", got)
	}
}

// TestReportBakedCLIVersion covers the skew helper's three-way contract directly.
func TestReportBakedCLIVersion(t *testing.T) {
	cases := []struct {
		name       string
		baked      string
		host       string
		wantOutput bool
	}{
		{name: "empty is silent", baked: "", host: "0.0.5", wantOutput: false},
		{name: "match is silent", baked: "0.0.5", host: "0.0.5", wantOutput: false},
		{name: "mismatch warns", baked: "9.9.9", host: "0.0.5", wantOutput: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			reportBakedCLIVersion(&buf, "img:test", tc.baked, tc.host)
			if got := buf.Len() > 0; got != tc.wantOutput {
				t.Fatalf("output presence = %v, want %v (got %q)", got, tc.wantOutput, buf.String())
			}
		})
	}
}

// stubContainerToolBoot swaps the single real-Docker call site for the tool
// image runner with a recorder that also simulates the tool writing its output
// to the collect mount, so the runner's spec-construction and collect-readback
// are testable with no live runtime. collectContent is written to the collect
// volume's host source under the CollectPath basename.
func stubContainerToolBoot(t *testing.T, capture *sandboxcontainer.RunOptions, collectBasename, collectContent string, err error) {
	t.Helper()
	orig := containerRunToolImage
	containerRunToolImage = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		*capture = opts
		if err != nil {
			return sandboxcontainer.RunResult{}, err
		}
		// Simulate the tool writing its output into the collect mount so the
		// runner's readback path is exercised end to end.
		if collectBasename != "" {
			for _, v := range opts.Volumes {
				if !v.ReadOnly {
					_ = os.WriteFile(filepath.Join(v.Source, collectBasename), []byte(collectContent), 0o644)
				}
			}
		}
		return sandboxcontainer.RunResult{}, nil
	}
	t.Cleanup(func() { containerRunToolImage = orig })
}

// TestContainerToolImageRunner_MountsRunsCollects proves the production tool
// runner boots the exact pinned image, mounts the resolved input read-only at
// the declared mount path, mounts the collect dir writable, and reads back the
// collected output as the step's result. It is the #1733 CLI-wiring acceptance:
// the pinned image and mount/collect wiring are asserted without touching Docker.
func TestContainerToolImageRunner_MountsRunsCollects(t *testing.T) {
	var got sandboxcontainer.RunOptions
	stubContainerToolBoot(t, &got, "out", "COLLECTED-BYTES", nil)

	spec := runtime.ToolRunSpec{
		Image:       "registry.example.com/tool-a:1@sha256:abc",
		StepID:      "extract",
		MountPath:   "/work/in",
		Input:       map[string]any{"payload": "hello"},
		CollectPath: "/work/out/out",
	}
	res, err := containerToolImageRunner{}.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The booted image is the exact pin, never re-resolved.
	if got.Image != spec.Image {
		t.Errorf("booted image = %q, want the exact pin %q", got.Image, spec.Image)
	}
	// The mount is read-only at the declared mount path; the collect is writable
	// at the collect path's parent.
	var mountRO, collectRW bool
	for _, v := range got.Volumes {
		if v.Target == "/work/in" && v.ReadOnly {
			mountRO = true
		}
		if v.Target == "/work/out" && !v.ReadOnly {
			collectRW = true
		}
	}
	if !mountRO {
		t.Errorf("input must be mounted read-only at the declared mount path, got %+v", got.Volumes)
	}
	if !collectRW {
		t.Errorf("collect dir must be mounted writable at the collect path parent, got %+v", got.Volumes)
	}
	// The collected bytes become the step's output.
	if res.Output != "COLLECTED-BYTES" {
		t.Errorf("collected output = %v, want the tool's written bytes", res.Output)
	}
	// The container name carries a run-unique suffix.
	if !strings.HasPrefix(got.Name, "aileron-flightplan-tool-extract-") {
		t.Errorf("container name = %q, want the stable tool prefix plus a suffix", got.Name)
	}
}

// TestContainerToolImageRunner_NoMountNoCollect proves a minimal rung-3 step
// (image only, no mount/collect) boots the image and returns a nil output with
// no mount or collect volume.
func TestContainerToolImageRunner_NoMountNoCollect(t *testing.T) {
	var got sandboxcontainer.RunOptions
	stubContainerToolBoot(t, &got, "", "", nil)

	res, err := containerToolImageRunner{}.Run(context.Background(), runtime.ToolRunSpec{
		Image:  "registry.example.com/tool-a:1@sha256:abc",
		StepID: "s1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.Volumes) != 0 {
		t.Errorf("a mount/collect-free dispatch must mount nothing, got %+v", got.Volumes)
	}
	if res.Output != nil {
		t.Errorf("a collect-free dispatch must produce no output, got %v", res.Output)
	}
}

// TestContainerToolImageRunner_EmptyImageErrors proves an empty pin errors
// before any boot.
func TestContainerToolImageRunner_EmptyImageErrors(t *testing.T) {
	if _, err := (containerToolImageRunner{}).Run(context.Background(), runtime.ToolRunSpec{}); err == nil {
		t.Fatal("an empty tool image must error before any boot")
	}
}

// TestContainerToolImageRunner_BootFailureSurfaces proves a boot failure
// surfaces rather than being swallowed.
func TestContainerToolImageRunner_BootFailureSurfaces(t *testing.T) {
	var got sandboxcontainer.RunOptions
	stubContainerToolBoot(t, &got, "", "", errors.New("tool boot exploded"))

	_, err := containerToolImageRunner{}.Run(context.Background(), runtime.ToolRunSpec{
		Image:  "img@sha256:abc",
		StepID: "s1",
	})
	if err == nil || !strings.Contains(err.Error(), "tool boot exploded") {
		t.Fatalf("boot failure must surface, got %v", err)
	}
}

// TestContainerToolImageRunner_MissingCollectedFileErrors proves a tool that
// declares a collect path but writes no file surfaces a readback error rather
// than silently producing an empty output.
func TestContainerToolImageRunner_MissingCollectedFileErrors(t *testing.T) {
	var got sandboxcontainer.RunOptions
	// collectBasename empty => the stub writes nothing, so the readback fails.
	stubContainerToolBoot(t, &got, "", "", nil)

	_, err := containerToolImageRunner{}.Run(context.Background(), runtime.ToolRunSpec{
		Image:       "img@sha256:abc",
		StepID:      "s1",
		CollectPath: "/work/out/out",
	})
	if err == nil || !strings.Contains(err.Error(), "collect output") {
		t.Fatalf("a missing collected file must error, got %v", err)
	}
}

// TestContainerToolImageRunner_SymlinkedCollectRefused is the CWE-61 regression
// proof: a tool image that writes the collected file as a SYMLINK pointing at an
// arbitrary host path must not have that path followed and its contents smuggled
// into the step output. The runner must refuse a non-regular collected file.
func TestContainerToolImageRunner_SymlinkedCollectRefused(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(secret, []byte("TOP SECRET HOST BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := containerRunToolImage
	containerRunToolImage = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		// Simulate a malicious tool: write the collect file as a symlink to a
		// host path outside the mount.
		for _, v := range opts.Volumes {
			if !v.ReadOnly {
				_ = os.Symlink(secret, filepath.Join(v.Source, "out"))
			}
		}
		return sandboxcontainer.RunResult{}, nil
	}
	t.Cleanup(func() { containerRunToolImage = orig })

	res, err := containerToolImageRunner{}.Run(context.Background(), runtime.ToolRunSpec{
		Image:       "img@sha256:abc",
		StepID:      "s1",
		CollectPath: "/work/out/out",
	})
	if err == nil {
		t.Fatalf("a symlinked collected file must be refused, got output %v", res.Output)
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("error should explain the non-regular-file refusal, got: %v", err)
	}
	if res.Output != nil {
		t.Errorf("no host bytes must be smuggled into the output, got %v", res.Output)
	}
}

// fakeToolProxyBootstrapper is a test double for toolProxyBootstrapper. It
// records that Prepare/cleanup ran and returns a canned enrichment so the
// enriched RunOptions shape is asserted with no live daemon and no Docker.
type fakeToolProxyBootstrapper struct {
	env        map[string]string
	mount      sandboxcontainer.Volume
	ok         bool
	err        error
	prepared   bool
	cleaned    bool
	gotRuntime string
	gotStepID  string
}

func (f *fakeToolProxyBootstrapper) Prepare(runtimeName, stepID string) (toolProxyBootstrap, func(), bool, error) {
	f.prepared = true
	f.gotRuntime = runtimeName
	f.gotStepID = stepID
	if f.err != nil {
		return toolProxyBootstrap{}, nil, false, f.err
	}
	return toolProxyBootstrap{Env: f.env, Mount: f.mount}, func() { f.cleaned = true }, f.ok, nil
}

// TestContainerToolImageRunner_ProxyEnrichesRunOptions proves that when a proxy
// bootstrapper is wired, the dispatch appends the read-only CA mount, sets the
// proxy/CA-bundle env, seeds placeholder creds, never sets a Command, and runs
// cleanup after the boot. This is the #1769 credential-injection wiring proof:
// the enriched RunOptions shape is asserted without touching Docker.
func TestContainerToolImageRunner_ProxyEnrichesRunOptions(t *testing.T) {
	var got sandboxcontainer.RunOptions
	stubContainerToolBoot(t, &got, "out", "COLLECTED", nil)

	fake := &fakeToolProxyBootstrapper{
		ok: true,
		env: map[string]string{
			"HTTPS_PROXY":           "http://sess:tok@host.docker.internal:48123",
			"https_proxy":           "http://sess:tok@host.docker.internal:48123",
			"AWS_CA_BUNDLE":         "/etc/aileron/proxy/ca.pem",
			"AWS_ACCESS_KEY_ID":     placeholderAWSAccessKeyID,
			"AWS_SECRET_ACCESS_KEY": placeholderAWSSecretKey,
		},
		mount: sandboxcontainer.Volume{Source: "/host/ca.pem", Target: "/etc/aileron/proxy/ca.pem", ReadOnly: true},
	}
	runner := containerToolImageRunner{proxy: fake}

	spec := runtime.ToolRunSpec{
		Image:       "amazon/aws-cli@sha256:abc",
		StepID:      "query-athena",
		MountPath:   "/work/in",
		Input:       map[string]any{"sql": "SELECT 1"},
		CollectPath: "/work/out/out",
	}
	if _, err := runner.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !fake.prepared {
		t.Error("Prepare must be called when a proxy bootstrapper is wired")
	}
	if fake.gotStepID != "query-athena" {
		t.Errorf("Prepare stepID = %q, want the spec step id", fake.gotStepID)
	}
	if !fake.cleaned {
		t.Error("cleanup must run after the boot")
	}

	// The CA mount is appended read-only at the well-known in-container CA path,
	// alongside the mount/collect volumes.
	var caMountRO bool
	for _, v := range got.Volumes {
		if v.Target == "/etc/aileron/proxy/ca.pem" && v.ReadOnly {
			caMountRO = true
		}
	}
	if !caMountRO {
		t.Errorf("CA must be mounted read-only at /etc/aileron/proxy/ca.pem, got %+v", got.Volumes)
	}

	// The proxy + CA-bundle + placeholder-cred env is set on the run.
	if !strings.Contains(got.Env["HTTPS_PROXY"], "host.docker.internal") {
		t.Errorf("HTTPS_PROXY = %q, want host.docker.internal", got.Env["HTTPS_PROXY"])
	}
	if got.Env["AWS_CA_BUNDLE"] != "/etc/aileron/proxy/ca.pem" {
		t.Errorf("AWS_CA_BUNDLE = %q, want the container CA path", got.Env["AWS_CA_BUNDLE"])
	}
	if got.Env["AWS_ACCESS_KEY_ID"] != placeholderAWSAccessKeyID {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want placeholder", got.Env["AWS_ACCESS_KEY_ID"])
	}

	// The tool image's own entrypoint must run: Command is NEVER set.
	if len(got.Command) != 0 {
		t.Errorf("Command must never be set for a tool dispatch, got %v", got.Command)
	}
}

// TestContainerToolImageRunner_ProxyPassthroughWhenNotOK proves that a
// bootstrapper returning ok=false leaves the dispatch un-enriched (no CA mount,
// no env) and still cleans up nothing.
func TestContainerToolImageRunner_ProxyPassthroughWhenNotOK(t *testing.T) {
	var got sandboxcontainer.RunOptions
	stubContainerToolBoot(t, &got, "out", "COLLECTED", nil)

	fake := &fakeToolProxyBootstrapper{ok: false}
	runner := containerToolImageRunner{proxy: fake}

	if _, err := runner.Run(context.Background(), runtime.ToolRunSpec{
		Image:       "img@sha256:abc",
		StepID:      "s1",
		CollectPath: "/work/out/out",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !fake.prepared {
		t.Error("Prepare must still be consulted")
	}
	for _, v := range got.Volumes {
		if v.Target == "/etc/aileron/proxy/ca.pem" {
			t.Errorf("passthrough dispatch must not mount a CA, got %+v", got.Volumes)
		}
	}
	if len(got.Env) != 0 {
		t.Errorf("passthrough dispatch must set no env, got %+v", got.Env)
	}
}

// TestContainerToolImageRunner_ProxyPrepareErrorFailsClosed proves that a
// Prepare error (daemon config resolved but the CA could not be written) fails
// the dispatch rather than silently egressing un-proxied.
func TestContainerToolImageRunner_ProxyPrepareErrorFailsClosed(t *testing.T) {
	var got sandboxcontainer.RunOptions
	booted := false
	orig := containerRunToolImage
	containerRunToolImage = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		booted = true
		got = opts
		return sandboxcontainer.RunResult{}, nil
	}
	t.Cleanup(func() { containerRunToolImage = orig })

	runner := containerToolImageRunner{proxy: &fakeToolProxyBootstrapper{err: errors.New("ca write failed")}}
	_, err := runner.Run(context.Background(), runtime.ToolRunSpec{Image: "img@sha256:abc", StepID: "s1"})
	if err == nil || !strings.Contains(err.Error(), "ca write failed") {
		t.Fatalf("Prepare error must fail the dispatch closed, got %v", err)
	}
	if booted {
		t.Errorf("the tool image must NOT boot after a fail-closed Prepare error, got %+v", got)
	}
}

func TestContainerImageRunner_EmptyImageErrors(t *testing.T) {
	_, err := containerImageRunner{}.Run(context.Background(), runtime.ImageRunSpec{})
	if err == nil {
		t.Fatal("an empty image must error before any boot")
	}
}

func TestContainerImageRunner_BootFailureSurfaces(t *testing.T) {
	origStore := skillStoreDir
	skillStoreDir = t.TempDir()
	t.Cleanup(func() { skillStoreDir = origStore })

	var got sandboxcontainer.RunOptions
	stubContainerBoot(t, &got, errors.New("boot exploded"))

	_, err := containerImageRunner{}.Run(context.Background(), runtime.ImageRunSpec{
		Image: "img@sha256:abc",
		Name:  "unit",
	})
	if err == nil || !strings.Contains(err.Error(), "boot exploded") {
		t.Fatalf("boot failure must surface, got %v", err)
	}
}
